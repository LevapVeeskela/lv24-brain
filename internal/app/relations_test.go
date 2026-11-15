package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// relationFixture is a local brain with a real Markdown store and index.
type relationFixture struct {
	app     *app.App
	store   *fsstore.Store
	clock   *time.Time
	dataDir string
}

func newRelationFixture(t *testing.T) relationFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	fixture := relationFixture{
		app:     app.New(cfg, app.Dependencies{Store: store, Index: index, Now: func() time.Time { return clock }}),
		store:   store,
		clock:   &clock,
		dataDir: dataDir,
	}
	t.Cleanup(func() { _ = index.Close() })
	if err := store.PutProject(context.Background(), domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindOther}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f relationFixture) note(t *testing.T, title, body string) string {
	t.Helper()
	result, err := f.app.Note(context.Background(), app.NoteInput{
		ProjectID: "p", Title: title, Body: body, AllowDuplicate: true,
	})
	if err != nil {
		t.Fatalf("Note(%q): %v", title, err)
	}
	return result.Note.ID
}

func (f relationFixture) decision(t *testing.T, title, contextText string, relations ...domain.Relation) string {
	t.Helper()
	result, err := f.app.Decision(context.Background(), app.DecisionInput{
		ProjectID: "p", Title: title, Context: contextText, Status: domain.DecisionAccepted,
		Relations: relations,
	})
	if err != nil {
		t.Fatalf("Decision(%q): %v", title, err)
	}
	return result.Decision.ID
}

func (f relationFixture) record(t *testing.T, id string) app.Record {
	t.Helper()
	record, err := f.app.GetRecord(context.Background(), "p", id)
	if err != nil {
		t.Fatalf("GetRecord(%s): %v", id, err)
	}
	return record
}

func relationTo(t *testing.T, list domain.RelationList, target string) domain.Relation {
	t.Helper()
	for _, relation := range list {
		if relation.Target == target {
			return relation.Normalize()
		}
	}
	t.Fatalf("no relation to %s in %+v", target, list)
	return domain.Relation{}
}

func TestWriteAddsOnlyPendingRelationCandidates(t *testing.T) {
	fixture := newRelationFixture(t)
	first := fixture.note(t, "Cache design", "use one sqlite index for the search cache with bm25 ranking")
	second := fixture.note(t, "Cache design", "use one sqlite index for the search cache with bm25 ranking and recency")
	relations := fixture.record(t, second).Relations()
	pending := relations.Pending()
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want one candidate", pending)
	}
	candidate := pending[0]
	if candidate.Target != first || candidate.Relation != domain.RelationRelatesTo {
		t.Fatalf("candidate = %+v", candidate)
	}
	if candidate.Confidence < 0.6 || candidate.Confidence > 1 {
		t.Fatalf("confidence = %v", candidate.Confidence)
	}
	if len(relations.Accepted()) != 0 {
		t.Fatalf("candidate detection wrote an accepted verdict: %+v", relations)
	}
	if tombstone := fixture.record(t, second).MergeInfo(); tombstone.MergedInto != "" || len(tombstone.MergedFrom) != 0 {
		t.Fatalf("candidate detection merged records: %+v", tombstone)
	}
	reported, err := fixture.app.Relations(context.Background(), app.RelationListOptions{ProjectID: "p", ID: second})
	if err != nil || len(reported) != 1 || reported[0].Relation.Status != domain.RelationPending {
		t.Fatalf("Relations = %+v, %v", reported, err)
	}
	if reported[0].TargetState != app.RelationTargetFound || reported[0].Target == nil {
		t.Fatalf("target not annotated: %+v", reported[0])
	}
	accepted, err := fixture.app.Relations(context.Background(), app.RelationListOptions{
		ProjectID: "p", ID: second, Statuses: []domain.RelationStatus{domain.RelationAccepted},
	})
	if err != nil || len(accepted) != 0 {
		t.Fatalf("accepted listing = %+v, %v", accepted, err)
	}
}

func TestExplicitRelationsAreAcceptedAndValidated(t *testing.T) {
	fixture := newRelationFixture(t)
	target := fixture.decision(t, "Use SQLite", "one store")
	relation := domain.Relation{
		Target: target, Relation: domain.RelationConflictsWith, Reason: "the opposite call",
		Status: domain.RelationAccepted, JudgedAt: fixture.clock.UTC(), VerdictBy: domain.VerdictByHuman,
	}
	holder := fixture.decision(t, "Use Postgres", "another store", relation)
	got := relationTo(t, fixture.record(t, holder).Relations(), target)
	if got.Status != domain.RelationAccepted || got.VerdictBy != domain.VerdictByHuman {
		t.Fatalf("explicit relation = %+v", got)
	}
	listed, err := fixture.app.Relations(context.Background(), app.RelationListOptions{
		ProjectID: "p", ID: holder, Statuses: []domain.RelationStatus{domain.RelationAccepted},
	})
	if err != nil || len(listed) != 1 || listed[0].Relation.Relation != domain.RelationConflictsWith {
		t.Fatalf("accepted listing = %+v, %v", listed, err)
	}
}

func TestExplicitRelationsRequireALiveTarget(t *testing.T) {
	fixture := newRelationFixture(t)
	_, err := fixture.app.Decision(context.Background(), app.DecisionInput{
		ProjectID: "p", Title: "Dangling", Context: "x",
		Relations: []domain.Relation{{Target: "decision-missing", Relation: domain.RelationRelatesTo}},
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing target err = %v", err)
	}
}

func TestExplicitRelationRefusals(t *testing.T) {
	fixture := newRelationFixture(t)
	target := fixture.decision(t, "Target", "context")
	cases := []struct {
		name      string
		relations []domain.Relation
		want      string
	}{
		{"missing target", []domain.Relation{{Target: "decision-missing", Relation: domain.RelationRelatesTo}}, "not found"},
		{"unknown verb", []domain.Relation{{Target: target, Relation: "likes"}}, "unknown relation"},
		{"two verdicts", []domain.Relation{
			{Target: target, Relation: domain.RelationCompatible},
			{Target: target, Relation: domain.RelationConflictsWith},
		}, "one verdict"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := fixture.app.Decision(context.Background(), app.DecisionInput{
				ProjectID: "p", Title: "Refused " + test.name, Context: "x", Relations: test.relations,
			})
			if err == nil {
				t.Fatal("write accepted")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
	// A self-link is refused when the caller pins the id.
	_, err := fixture.app.Note(context.Background(), app.NoteInput{
		ProjectID: "p", ID: "note-self", Title: "Self", Body: "x",
		Relations: []domain.Relation{{Target: "note-self", Relation: domain.RelationRelatesTo}},
	})
	if err == nil || !strings.Contains(err.Error(), "itself") {
		t.Fatalf("self relation err = %v", err)
	}
}

func TestExplicitRelationCycles(t *testing.T) {
	fixture := newRelationFixture(t)
	a := fixture.decision(t, "A", "a")
	b := fixture.decision(t, "B", "b", domain.Relation{Target: a, Relation: domain.RelationDependsOn, Reason: "b needs a"})
	if b == "" {
		t.Fatal("no b")
	}
	c := fixture.decision(t, "C", "c", domain.Relation{Target: b, Relation: domain.RelationDependsOn, Reason: "c needs b"})
	if c == "" {
		t.Fatal("no c")
	}
	if _, err := fixture.app.JudgeRelation(context.Background(), app.JudgeOptions{
		ProjectID: "p", From: a, To: b, Verdict: domain.RelationDependsOn, Reason: "a needs b",
	}); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("cycle err = %v", err)
	}
	// A relates_to edge may close the same loop: it claims no hierarchy.
	if _, err := fixture.app.JudgeRelation(context.Background(), app.JudgeOptions{
		ProjectID: "p", From: a, To: b, Verdict: domain.RelationRelatesTo, Reason: "same area",
	}); err != nil {
		t.Fatalf("relates_to loop refused: %v", err)
	}
}

func TestJudgeTransitionsAndNeverTouchesTheTarget(t *testing.T) {
	fixture := newRelationFixture(t)
	first := fixture.note(t, "Cache design", "use one sqlite index for the search cache with bm25 ranking")
	second := fixture.note(t, "Cache design", "use one sqlite index for the search cache with bm25 ranking and recency")
	targetBefore := fixture.record(t, first).Summary()

	result, err := fixture.app.JudgeRelation(context.Background(), app.JudgeOptions{
		ProjectID: "p", From: second, To: first, Verdict: domain.RelationNotConflict,
		Reason: "the recency boost is compatible", Confidence: 0.8,
	})
	if err != nil {
		t.Fatalf("JudgeRelation: %v", err)
	}
	if !result.Changed || result.Relation.Relation.Status != domain.RelationAccepted {
		t.Fatalf("judge = %+v", result)
	}
	if result.Relation.Relation.VerdictBy != domain.VerdictByHuman || result.Relation.Relation.JudgedAt.IsZero() {
		t.Fatalf("verdict metadata = %+v", result.Relation.Relation)
	}
	relations := fixture.record(t, second).Relations()
	if len(relations) != 1 || relations[0].Relation != domain.RelationNotConflict || relations[0].Status != domain.RelationAccepted {
		t.Fatalf("relations = %+v", relations)
	}
	// A repeated verdict writes nothing.
	repeat, err := fixture.app.JudgeRelation(context.Background(), app.JudgeOptions{
		ProjectID: "p", From: second, To: first, Verdict: domain.RelationNotConflict,
		Reason: "the recency boost is compatible", Confidence: 0.8,
	})
	if err != nil || repeat.Changed {
		t.Fatalf("repeat judge = %+v, %v", repeat, err)
	}
	// A different verdict replaces the pair's verb.
	if _, err := fixture.app.JudgeRelation(context.Background(), app.JudgeOptions{
		ProjectID: "p", From: second, To: first, Verdict: domain.RelationConflictsWith,
		Reason: "they contradict", By: domain.VerdictByAgent,
	}); err != nil {
		t.Fatalf("replace verdict: %v", err)
	}
	relations = fixture.record(t, second).Relations()
	if len(relations) != 1 || relations[0].Relation != domain.RelationConflictsWith || relations[0].VerdictBy != domain.VerdictByAgent {
		t.Fatalf("relations after replace = %+v", relations)
	}
	// The target record is untouched: same status, still current.
	targetAfter := fixture.record(t, first)
	if targetAfter.Summary().Status != targetBefore.Status || !targetAfter.Validity().ValidTo.IsZero() {
		t.Fatalf("judging edited the target: %+v", targetAfter.Summary())
	}
}

func TestJudgeRefusals(t *testing.T) {
	fixture := newRelationFixture(t)
	a := fixture.decision(t, "A", "a")
	b := fixture.decision(t, "B", "b")
	cases := []struct {
		name string
		opts app.JudgeOptions
		want string
	}{
		{"self", app.JudgeOptions{ProjectID: "p", From: a, To: a, Verdict: domain.RelationRelatesTo, Reason: "x"}, "itself"},
		{"unknown verdict", app.JudgeOptions{ProjectID: "p", From: b, To: a, Verdict: "likes", Reason: "x"}, "unknown relation"},
		{"missing reason", app.JudgeOptions{ProjectID: "p", From: b, To: a, Verdict: domain.RelationRelatesTo}, "required"},
		{"bad confidence", app.JudgeOptions{ProjectID: "p", From: b, To: a, Verdict: domain.RelationRelatesTo, Reason: "x", Confidence: 2}, "between 0 and 1"},
		{"missing target", app.JudgeOptions{ProjectID: "p", From: b, To: "decision-missing", Verdict: domain.RelationRelatesTo, Reason: "x"}, "not found"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := fixture.app.JudgeRelation(context.Background(), test.opts)
			if err == nil {
				t.Fatal("judge accepted")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}

func TestJudgeAcceptsATombstoneTarget(t *testing.T) {
	fixture := newRelationFixture(t)
	survivor := fixture.decision(t, "Survivor", "kept")
	duplicate := fixture.decision(t, "Duplicate", "absorbed")
	plan, err := fixture.app.PlanMerge(context.Background(), app.MergeOptions{
		ProjectID: "p", Survivor: survivor, Duplicates: []string{duplicate},
	})
	if err != nil || len(plan.Conflicts) != 0 {
		t.Fatalf("PlanMerge = %+v, %v", plan, err)
	}
	if _, err := fixture.app.Merge(context.Background(), app.MergeOptions{
		ProjectID: "p", Survivor: survivor, Duplicates: []string{duplicate}, Apply: true,
	}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	result, err := fixture.app.JudgeRelation(context.Background(), app.JudgeOptions{
		ProjectID: "p", From: survivor, To: duplicate, Verdict: domain.RelationSupersedes, Reason: "it absorbed the duplicate",
	})
	if err != nil {
		t.Fatalf("judge tombstone: %v", err)
	}
	if result.Relation.TargetState != app.RelationTargetTombstone {
		t.Fatalf("target state = %q", result.Relation.TargetState)
	}
}

func TestPendingRelationFinding(t *testing.T) {
	fixture := newRelationFixture(t)
	first := fixture.note(t, "Cache design", "use one sqlite index for the search cache with bm25 ranking")
	second := fixture.note(t, "Cache design", "use one sqlite index for the search cache with bm25 ranking and recency")
	// The daily notes file's updated_at is stamped by the store with its own
	// clock (the real one), and a read exposes that stamp as every note's
	// UpdatedAt. Anchor the fixture clock to the stamp actually stored, so the
	// asserted age does not depend on the time of day the test runs.
	since := fixture.record(t, second).Summary().UpdatedAt
	if since.IsZero() {
		t.Fatal("source record has no UpdatedAt")
	}
	pendingFindings := func() []app.Finding {
		t.Helper()
		findings, err := fixture.app.Findings(context.Background(), "p")
		if err != nil {
			t.Fatalf("Findings: %v", err)
		}
		var pending []app.Finding
		for _, finding := range findings {
			if finding.Kind == app.FindingPendingRelations {
				pending = append(pending, finding)
			}
		}
		return pending
	}
	// The same relation is fresh right after the write that proposed it.
	*fixture.clock = since
	pending := pendingFindings()
	if len(pending) != 1 || !strings.Contains(pending[0].Message, "just now") {
		t.Fatalf("fresh pending findings = %+v", pending)
	}
	// Three days and an hour later it must report three full days, not two:
	// a sub-day rounding error or a two-day boundary regression fails here.
	*fixture.clock = since.Add(3*24*time.Hour + time.Hour)
	pending = pendingFindings()
	if len(pending) != 1 {
		t.Fatalf("pending findings = %+v", pending)
	}
	if pending[0].Severity != app.SeverityWarn {
		t.Fatalf("severity = %q", pending[0].Severity)
	}
	if !strings.Contains(pending[0].Message, second) || !strings.Contains(pending[0].Message, first) || !strings.Contains(pending[0].Message, "3 days old") {
		t.Fatalf("message = %q", pending[0].Message)
	}
	if _, err := fixture.app.JudgeRelation(context.Background(), app.JudgeOptions{
		ProjectID: "p", From: second, To: first, Verdict: domain.RelationNotConflict, Reason: "compatible",
	}); err != nil {
		t.Fatalf("judge: %v", err)
	}
	if pending = pendingFindings(); len(pending) != 0 {
		t.Fatalf("judged relation still reported: %+v", pending)
	}
}

func TestAcceptedConflictDoesNotResolveEitherRecord(t *testing.T) {
	fixture := newRelationFixture(t)
	a := fixture.decision(t, "Use SQLite", "one store")
	b := fixture.decision(t, "Use Postgres", "another store")
	if _, err := fixture.app.JudgeRelation(context.Background(), app.JudgeOptions{
		ProjectID: "p", From: b, To: a, Verdict: domain.RelationConflictsWith, Reason: "opposite storage choices",
	}); err != nil {
		t.Fatalf("judge: %v", err)
	}
	for _, id := range []string{a, b} {
		record := fixture.record(t, id)
		if !record.Validity().ValidTo.IsZero() || record.Summary().Status != string(domain.DecisionAccepted) {
			t.Fatalf("conflict resolved %s automatically: %+v", id, record.Summary())
		}
	}
	listed, err := fixture.app.ListRecords(context.Background(), app.ListRecordsOptions{ProjectID: "p", Kinds: []string{"decision"}})
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed = %+v, %v", listed, err)
	}
}

func TestRelationsListWithoutAnIndex(t *testing.T) {
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	noIndex := app.New(cfg, app.Dependencies{Store: store, Now: func() time.Time { return time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC) }})
	if err := store.PutProject(context.Background(), domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindOther}); err != nil {
		t.Fatal(err)
	}
	first, err := noIndex.Note(context.Background(), app.NoteInput{
		ProjectID: "p", Title: "Cache design", Body: "use one sqlite index for the search cache with bm25 ranking",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noIndex.Note(context.Background(), app.NoteInput{
		ProjectID: "p", Title: "Cache design", Body: "use one sqlite index for the search cache with bm25 ranking and recency",
	}); err != nil {
		t.Fatal(err)
	}
	views, err := noIndex.Relations(context.Background(), app.RelationListOptions{ProjectID: "p"})
	if err != nil || len(views) != 1 || views[0].TargetID != first.Note.ID {
		t.Fatalf("views = %+v, %v", views, err)
	}
}

func TestMergeThroughAppRefreshesTheIndex(t *testing.T) {
	fixture := newRelationFixture(t)
	survivor := fixture.decision(t, "Cache survivor", "one store for the cache")
	duplicate := fixture.decision(t, "Cache duplicate", "a second cache decision")
	before, err := fixture.store.CurrentRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dry, err := fixture.app.Merge(context.Background(), app.MergeOptions{
		ProjectID: "p", Survivor: survivor, Duplicates: []string{duplicate}, DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Applied || !dry.Plan.Changed {
		t.Fatalf("dry run = %+v", dry)
	}
	afterDry, err := fixture.store.CurrentRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.Value != afterDry.Value {
		t.Fatalf("dry run wrote: %s -> %s", before.Value, afterDry.Value)
	}
	applied, err := fixture.app.Merge(context.Background(), app.MergeOptions{
		ProjectID: "p", Survivor: survivor, Duplicates: []string{duplicate}, Apply: true,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !applied.Applied || applied.Manifest == "" {
		t.Fatalf("apply = %+v", applied)
	}
	// The duplicate is gone from search and the survivor carries merged_from.
	hits, err := fixture.app.SearchWith(context.Background(), app.SearchOptions{Text: "second cache decision", ProjectID: "p"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, hit := range hits {
		if hit.ID == duplicate {
			t.Fatalf("merged duplicate still searchable: %+v", hit)
		}
	}
	if len(fixture.record(t, survivor).MergeInfo().MergedFrom) != 1 {
		t.Fatalf("survivor merge info = %+v", fixture.record(t, survivor).MergeInfo())
	}

	rolled, err := fixture.app.RollbackMerge(context.Background(), applied.Manifest)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if !rolled.Reindexed {
		t.Fatal("rollback did not rebuild the index")
	}
	restored, err := fixture.app.GetRecord(context.Background(), "p", duplicate)
	if err != nil {
		t.Fatalf("duplicate not restored: %v", err)
	}
	if !restored.Validity().ValidTo.IsZero() {
		t.Fatalf("restored duplicate is still ended: %+v", restored.Summary())
	}
}

func TestMergeRefusesProtectedRecords(t *testing.T) {
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutProject(context.Background(), domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindOther}); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC) }
	writer := app.New(config.Default(), app.Dependencies{Store: store, Now: now})
	survivor, err := writer.Decision(context.Background(), app.DecisionInput{ProjectID: "p", Title: "Survivor", Context: "x"})
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := writer.Decision(context.Background(), app.DecisionInput{ProjectID: "p", Title: "Duplicate", Context: "y"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.Limits.ProtectedProjects = []string{"p"}
	protected := app.New(cfg, app.Dependencies{Store: store, Now: now})
	_, err = protected.PlanMerge(context.Background(), app.MergeOptions{
		ProjectID: "p", Survivor: survivor.Decision.ID, Duplicates: []string{duplicate.Decision.ID},
	})
	if err == nil || !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("protected merge err = %v", err)
	}
}

func TestMergePlanConflictsAreRefusedByApply(t *testing.T) {
	fixture := newRelationFixture(t)
	survivor := fixture.decision(t, "Survivor", "x")
	note := fixture.note(t, "Note", "a note")
	if _, err := fixture.app.Merge(context.Background(), app.MergeOptions{
		ProjectID: "p", Survivor: survivor, Duplicates: []string{note}, Apply: true,
	}); err == nil || !strings.Contains(err.Error(), "one kind") {
		t.Fatalf("kind mismatch err = %v", err)
	}
	if _, err := app.ParseRelationSpecs([]string{"decision-x:supersedes:reason"}, domain.VerdictByHuman, *fixture.clock); err != nil {
		t.Fatalf("ParseRelationSpecs: %v", err)
	}
}
