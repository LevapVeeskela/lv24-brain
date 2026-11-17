package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// consolidateFixture is a git-mode app on a temp data directory, with the
// directory kept for manifest assertions. The clock is shared with the app's
// Now closure, so tick moves both.
type consolidateFixture struct {
	app   *App
	dir   string
	clock *time.Time
}

func newConsolidateFixture(t *testing.T) consolidateFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := fsstore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	a := New(cfg, Dependencies{Store: store, Journal: journal.New(dir), Now: func() time.Time { return now }})
	if _, err := a.Note(context.Background(), NoteInput{ProjectID: "p", Body: "seed", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	return consolidateFixture{app: a, dir: dir, clock: &now}
}

// tick advances the shared fixture clock.
func (f consolidateFixture) tick(days int) {
	*f.clock = f.clock.Add(time.Duration(days) * 24 * time.Hour)
}

// nearPairFixture builds two notes whose normalized texts are near (reported
// by the near detector) but strictly below the 0.98 merge threshold. The pair
// is searched deterministically: a few spread word substitutions keep the
// shingle Jaccard above the 0.8 report threshold while pushing the simhash
// sketch beyond its Hamming threshold, so the reported score comes from the
// shingles and lands below 0.98.
func nearPairFixture(t *testing.T, f consolidateFixture) (string, string) {
	t.Helper()
	words := make([]string, 0, 300)
	for i := 0; i < 300; i++ {
		words = append(words, fmt.Sprintf("w%d", i))
	}
	score := func(left, right string) (float64, bool) {
		a := newDupRecord("a", time.Time{}, "", left, false)
		b := newDupRecord("b", time.Time{}, "", right, false)
		value, _, ok := duplicatePair(a, b)
		return value, ok
	}
	base := strings.Join(words, " ")
	for subs := 3; subs <= 20; subs++ {
		variant := append([]string(nil), words...)
		for i := 0; i < subs; i++ {
			variant[i*13] = fmt.Sprintf("zz%d", i)
		}
		changed := strings.Join(variant, " ")
		if score, ok := score(base, changed); ok && score >= duplicateThreshold && score < ConsolidateNearMergeThreshold {
			if _, err := f.app.Note(context.Background(), NoteInput{ProjectID: "p", Title: "Near pair", Body: base}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.app.Note(context.Background(), NoteInput{ProjectID: "p", Title: "Near pair", Body: changed, AllowDuplicate: true}); err != nil {
				t.Fatal(err)
			}
			return base, changed
		}
	}
	t.Fatal("no deterministic near pair found below the merge threshold")
	return "", ""
}

func TestConsolidatePlanMergesExactClustersWithoutWriting(t *testing.T) {
	ctx := context.Background()
	f := newConsolidateFixture(t)
	const title = "Cache invalidation on sync"
	const body = "the cache is invalidated after a successful synchronization, never before"
	first, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: title, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(1)
	second, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: title + "!", Body: body + ".", AllowDuplicate: true})
	if err != nil {
		t.Fatal(err)
	}
	before := f.app.revision(ctx)

	plan, applied, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if applied != nil {
		t.Fatalf("plan phase returned an apply result: %+v", applied)
	}
	if len(plan.Ops) != 1 || plan.Ops[0].Kind != ConsolidateMerge {
		t.Fatalf("ops = %+v, want one merge", plan.Ops)
	}
	op := plan.Ops[0]
	if op.ID != "merge-"+first.Note.ID || op.RecordKind != "note" {
		t.Fatalf("op = %+v, want merge-%s of kind note", op, first.Note.ID)
	}
	if len(op.IDs) != 2 || op.IDs[0] != first.Note.ID || op.IDs[1] != second.Note.ID {
		t.Fatalf("op ids = %v, want oldest first", op.IDs)
	}
	if op.Risk != ConsolidateRiskLow || !strings.Contains(op.Reversible, "rollback") {
		t.Fatalf("op risk/reversible = %q / %q", op.Risk, op.Reversible)
	}
	if op.Fingerprint == "" || plan.Fingerprint == "" {
		t.Fatalf("fingerprints are empty: op %q plan %q", op.Fingerprint, plan.Fingerprint)
	}

	// A plan is deterministic and writes nothing.
	again, _, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Fingerprint != plan.Fingerprint {
		t.Fatalf("plan fingerprint changed between runs: %s != %s", plan.Fingerprint, again.Fingerprint)
	}
	if after := f.app.revision(ctx); after != before {
		t.Fatalf("plan changed the revision: %s -> %s", before, after)
	}
	if trashed, err := f.app.ListTrash(ctx, "p"); err != nil || len(trashed) != 0 {
		t.Fatalf("plan trashed records: %+v, %v", trashed, err)
	}
}

func TestConsolidateApplyExactMergeThenRollback(t *testing.T) {
	ctx := context.Background()
	f := newConsolidateFixture(t)
	const title = "Cache invalidation on sync"
	const body = "the cache is invalidated after a successful synchronization, never before"
	first, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: title, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(1)
	second, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: title + "!", Body: body + ".", AllowDuplicate: true})
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p"})
	if err != nil || len(plan.Ops) != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}

	_, applied, err := f.app.Consolidate(ctx, ConsolidateOptions{
		ProjectID: "p", Phase: "apply", Ops: []string{plan.Ops[0].ID}, Fingerprint: plan.Fingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Manifest == "" || len(applied.Merged) != 1 || len(applied.Archived) != 0 {
		t.Fatalf("apply = %+v", applied)
	}
	if applied.Merged[0].Survivor != first.Note.ID || applied.Merged[0].Manifest == "" {
		t.Fatalf("merged = %+v", applied.Merged[0])
	}
	if _, err := f.app.GetRecord(ctx, "p", second.Note.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("duplicate %s still live: %v", second.Note.ID, err)
	}
	if _, err := f.app.GetRecord(ctx, "p", first.Note.ID); err != nil {
		t.Fatalf("survivor is gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, filepath.FromSlash(applied.Manifest))); err != nil {
		t.Fatalf("consolidation manifest missing: %v", err)
	}
	if !strings.Contains(applied.Manifest, "backups/consolidate-") {
		t.Fatalf("manifest ref = %q, want backups/consolidate-<ts>/manifest.json", applied.Manifest)
	}

	// The rollback restores the merged state and stamps the manifest.
	rolled, err := f.app.ConsolidateRollback(ctx, applied.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Merges != 1 || rolled.AlreadyRolledBack {
		t.Fatalf("rollback = %+v", rolled)
	}
	restored, err := f.app.GetRecord(ctx, "p", second.Note.ID)
	if err != nil || restored.Note == nil || restored.Note.Body != body+"." {
		t.Fatalf("restored note = %+v, %v", restored, err)
	}
	again, err := f.app.ConsolidateRollback(ctx, applied.Manifest)
	if err != nil || !again.AlreadyRolledBack {
		t.Fatalf("second rollback = %+v, %v", again, err)
	}
}

func TestConsolidateApplyRefusesStaleFingerprint(t *testing.T) {
	ctx := context.Background()
	f := newConsolidateFixture(t)
	const title = "Cache invalidation on sync"
	const body = "the cache is invalidated after a successful synchronization, never before"
	if _, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: title, Body: body}); err != nil {
		t.Fatal(err)
	}
	f.tick(1)
	if _, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: title, Body: body, AllowDuplicate: true}); err != nil {
		t.Fatal(err)
	}
	before := f.app.revision(ctx)
	plan, _, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p"})
	if err != nil || len(plan.Ops) != 1 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	_, _, err = f.app.Consolidate(ctx, ConsolidateOptions{
		ProjectID: "p", Phase: "apply", Ops: []string{plan.Ops[0].ID}, Fingerprint: "stale",
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale fingerprint error = %v, want a conflict", err)
	}
	if after := f.app.revision(ctx); after != before {
		t.Fatalf("refused apply changed the revision: %s -> %s", before, after)
	}
}

func TestConsolidateApplyRefusesUnknownOpID(t *testing.T) {
	ctx := context.Background()
	f := newConsolidateFixture(t)
	if _, _, err := f.app.Consolidate(ctx, ConsolidateOptions{
		ProjectID: "p", Phase: "apply", Ops: []string{"merge-nope"},
	}); err == nil || !strings.Contains(err.Error(), "not in the current plan") {
		t.Fatalf("unknown op error = %v", err)
	}
}

func TestConsolidateIdempotentApplyWritesNoManifest(t *testing.T) {
	ctx := context.Background()
	f := newConsolidateFixture(t)
	plan, _, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Ops) != 0 {
		t.Fatalf("seed project has ops: %+v", plan.Ops)
	}
	_, applied, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p", Phase: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Manifest != "" || len(applied.Merged) != 0 || len(applied.Archived) != 0 {
		t.Fatalf("empty plan applied something: %+v", applied)
	}
}

func TestConsolidateArchiveCandidateRules(t *testing.T) {
	ctx := context.Background()
	f := newConsolidateFixture(t)
	// old: superseded 40 days ago, no links -> archive candidate.
	old, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: "Old rule", Body: "the old rule of the project"})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(1)
	if _, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: "New rule", Body: "the new rule of the project", Supersedes: old.Note.ID}); err != nil {
		t.Fatal(err)
	}
	// linked: superseded too, but the replacement body links to it.
	linkedOld, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: "Linked old", Body: "linked old rule text"})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(1)
	if _, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: "Linked new", Body: "linked new rule text, see [[" + linkedOld.Note.ID + "]]", Supersedes: linkedOld.Note.ID}); err != nil {
		t.Fatal(err)
	}
	// pinned: superseded but pinned by the owner.
	pinnedOld, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: "Pinned old", Body: "pinned old rule text"})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(1)
	if _, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: "Pinned new", Body: "pinned new rule text", Supersedes: pinnedOld.Note.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.SetRecordPinned(ctx, "p", pinnedOld.Note.ID, true); err != nil {
		t.Fatal(err)
	}
	f.tick(40)

	plan, _, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p", ArchiveAfterDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	var archiveOps []ConsolidateOp
	for _, op := range plan.Ops {
		if op.Kind == ConsolidateArchive {
			archiveOps = append(archiveOps, op)
		}
	}
	if len(archiveOps) != 1 || archiveOps[0].ID != "archive-"+old.Note.ID {
		t.Fatalf("archive ops = %+v, want only %s", archiveOps, old.Note.ID)
	}
	op := archiveOps[0]
	if op.Risk != ConsolidateRiskMedium || !strings.Contains(op.Reversible, "brain record restore") {
		t.Fatalf("archive op = %+v", op)
	}

	// A young superseded record is not a candidate yet.
	young, _, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p", ArchiveAfterDays: 90})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range young.Ops {
		if candidate.Kind == ConsolidateArchive {
			t.Fatalf("young archive op = %+v", candidate)
		}
	}

	// An expired record without a replacement is never archivable (that is
	// the stale-superseded finding), and a current record with a replacement
	// is not either.
	if consolidateArchivable(Record{Note: &domain.Note{ID: "x", Validity: domain.Validity{ValidTo: *f.clock}}}) {
		t.Fatal("an expired record without a replacement must not be archivable")
	}
	if consolidateArchivable(Record{Note: &domain.Note{ID: "x", Validity: domain.Validity{SupersededBy: "y"}}}) {
		t.Fatal("a current record with a replacement must not be archivable")
	}
}

func TestConsolidateApplyArchivesAndRollsBack(t *testing.T) {
	ctx := context.Background()
	f := newConsolidateFixture(t)
	old, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: "Old rule", Body: "the old rule of the project"})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(1)
	if _, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: "New rule", Body: "the new rule of the project", Supersedes: old.Note.ID}); err != nil {
		t.Fatal(err)
	}
	f.tick(40)
	plan, _, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p", ArchiveAfterDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	_, applied, err := f.app.Consolidate(ctx, ConsolidateOptions{
		ProjectID: "p", Phase: "apply", Ops: []string{"archive-" + old.Note.ID}, Fingerprint: plan.Fingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Archived) != 1 || applied.Archived[0].ID != old.Note.ID || applied.Manifest == "" {
		t.Fatalf("archive apply = %+v", applied)
	}
	if _, err := f.app.GetRecord(ctx, "p", old.Note.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("archived record is still live: %v", err)
	}
	rolled, err := f.app.ConsolidateRollback(ctx, applied.Manifest)
	if err != nil || rolled.Archived != 1 || rolled.AlreadyRolledBack {
		t.Fatalf("rollback = %+v, %v", rolled, err)
	}
	if _, err := f.app.GetRecord(ctx, "p", old.Note.ID); err != nil {
		t.Fatalf("archived record not restored: %v", err)
	}
}

func TestArchiveWithoutLinksRejectsLiveRelations(t *testing.T) {
	records := []Record{
		{Note: &domain.Note{ID: "a", Relations: domain.RelationList{{Target: "b", Relation: domain.RelationRelatesTo, Status: domain.RelationPending}}}},
		{Note: &domain.Note{ID: "b"}},
	}
	a := &App{}
	if a.archiveWithoutLinks("p", records, "b") {
		t.Fatal("a pending relation is a live link")
	}
	records[0].Note.Relations[0].Status = domain.RelationRejected
	if !a.archiveWithoutLinks("p", records, "b") {
		t.Fatal("a rejected relation is not a live link")
	}
}

func TestNearMergeOpThreshold(t *testing.T) {
	f := newConsolidateFixture(t)
	high := DuplicateCluster{ProjectID: "p", Kind: "note", Similarity: 0.99, Members: []DuplicateMember{{ID: "a"}, {ID: "b"}}}
	op, ok := f.app.nearMergeOp(high)
	if !ok || op.Kind != ConsolidateMerge || op.Risk != ConsolidateRiskMedium || op.ID != "merge-a" || len(op.IDs) != 2 {
		t.Fatalf("near merge op at 0.99 = %+v, %v", op, ok)
	}
	low := DuplicateCluster{ProjectID: "p", Kind: "note", Similarity: 0.97, Members: []DuplicateMember{{ID: "a"}, {ID: "b"}}}
	if _, ok := f.app.nearMergeOp(low); ok {
		t.Fatal("a 0.97 cluster must not become a merge op")
	}
}

func TestConsolidatePlanReportsNearClustersBelowThreshold(t *testing.T) {
	ctx := context.Background()
	f := newConsolidateFixture(t)
	nearPairFixture(t, f)
	plan, _, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Near) != 1 || len(plan.Near[0].Members) != 2 {
		t.Fatalf("near clusters = %+v", plan.Near)
	}
	if plan.Near[0].Similarity >= ConsolidateNearMergeThreshold {
		t.Fatalf("fixture similarity %v is not below the threshold", plan.Near[0].Similarity)
	}
	for _, op := range plan.Ops {
		if op.Kind == ConsolidateMerge {
			t.Fatalf("below-threshold cluster produced a merge op: %+v", op)
		}
	}
	// Naming the cluster's would-be op is a validation error, not a merge.
	older := plan.Near[0].Members[0].ID
	if _, _, err := f.app.Consolidate(ctx, ConsolidateOptions{
		ProjectID: "p", Phase: "apply", Ops: []string{"merge-" + older},
	}); err == nil || !strings.Contains(err.Error(), "not in the current plan") {
		t.Fatalf("near merge apply error = %v", err)
	}
}

func TestConsolidateCapTruncatesThePlan(t *testing.T) {
	ctx := context.Background()
	f := newConsolidateFixture(t)
	const title = "Cache invalidation on sync"
	const body = "the cache is invalidated after a successful synchronization, never before"
	if _, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: title, Body: body}); err != nil {
		t.Fatal(err)
	}
	f.tick(1)
	if _, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: title, Body: body, AllowDuplicate: true}); err != nil {
		t.Fatal(err)
	}
	old, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: "Old rule", Body: "the old rule of the project"})
	if err != nil {
		t.Fatal(err)
	}
	f.tick(1)
	if _, err := f.app.Note(ctx, NoteInput{ProjectID: "p", Title: "New rule", Body: "the new rule of the project", Supersedes: old.Note.ID}); err != nil {
		t.Fatal(err)
	}
	f.tick(40)
	full, _, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p", ArchiveAfterDays: 30})
	if err != nil || full.Truncated || len(full.Ops) != 2 {
		t.Fatalf("full plan = %+v, %v", full, err)
	}
	capped, _, err := f.app.Consolidate(ctx, ConsolidateOptions{ProjectID: "p", ArchiveAfterDays: 30, Max: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !capped.Truncated || len(capped.Ops) != 1 {
		t.Fatalf("capped plan = %+v, want one op and truncated", capped)
	}
}
