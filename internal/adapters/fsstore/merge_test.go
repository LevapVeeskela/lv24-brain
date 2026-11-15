package fsstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// mergeFixture seeds two decisions and a note that references the duplicate.
type mergeFixture struct {
	store     *Store
	project   domain.Project
	survivor  string
	duplicate string
	notefile  string
}

func newMergeFixture(t *testing.T) mergeFixture {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	survivor, duplicate := "decision-survivor", "decision-duplicate"
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ID: survivor, ProjectID: project.ID, Title: "Keep the cache in SQLite",
		Status: domain.DecisionAccepted, Context: "one store for the cache",
		CreatedAt: testClock.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("AppendDecision survivor: %v", err)
	}
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ID: duplicate, ProjectID: project.ID, Title: "Keep the cache in SQLite (copy)",
		Status: domain.DecisionAccepted, Context: "one store for the cache",
		Relations: domain.RelationList{{Target: survivor, Relation: domain.RelationRelatesTo, Status: domain.RelationPending, Reason: "candidate"}},
		CreatedAt: testClock.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("AppendDecision duplicate: %v", err)
	}
	body := "See [[" + duplicate + "]] for the cache plan."
	if _, err := store.AppendNote(ctx, domain.Note{
		ID: "note-linker", ProjectID: project.ID, Title: "Linker", Body: body, CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	return mergeFixture{store: store, project: project, survivor: survivor, duplicate: duplicate, notefile: "note-linker"}
}

// snapshotProject hashes every file under projects/<id>.
func snapshotProject(t *testing.T, store *Store, projectID string) map[string]string {
	t.Helper()
	root := filepath.Join(store.Root(), "projects", projectID)
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(store.Root(), path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return out
}

func sameSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	keys := map[string]struct{}{}
	for key := range before {
		keys[key] = struct{}{}
	}
	for key := range after {
		keys[key] = struct{}{}
	}
	names := make([]string, 0, len(keys))
	for key := range keys {
		names = append(names, key)
	}
	sort.Strings(names)
	for _, name := range names {
		if before[name] != after[name] {
			t.Fatalf("file %s changed: %q -> %q", name, before[name], after[name])
		}
	}
}

func TestPlanMergeDoesNotWrite(t *testing.T) {
	fixture := newMergeFixture(t)
	ctx := context.Background()
	before := snapshotProject(t, fixture.store, fixture.project.ID)

	plan, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{fixture.duplicate},
	})
	if err != nil {
		t.Fatalf("PlanMerge: %v", err)
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("conflicts = %v", plan.Conflicts)
	}
	if !plan.Changed || plan.Kind != ports.RecordDecision {
		t.Fatalf("plan = %+v", plan)
	}
	if strings.Join(plan.MergedFrom, ",") != fixture.duplicate {
		t.Fatalf("merged_from = %v", plan.MergedFrom)
	}
	if len(plan.Links) != 1 || plan.Links[0].SourceID != fixture.notefile || plan.Links[0].OldID != fixture.duplicate {
		t.Fatalf("links = %+v", plan.Links)
	}
	if plan.Fingerprint == "" {
		t.Fatal("plan has no fingerprint")
	}
	sameSnapshot(t, before, snapshotProject(t, fixture.store, fixture.project.ID))
	if _, err := os.Stat(filepath.Join(fixture.store.Root(), mergesDir)); !os.IsNotExist(err) {
		t.Fatalf("plan wrote a manifest: %v", err)
	}
}

func TestApplyMergeTombstonesRewritesAndRollsBack(t *testing.T) {
	fixture := newMergeFixture(t)
	ctx := context.Background()
	before := snapshotProject(t, fixture.store, fixture.project.ID)
	plan, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{fixture.duplicate},
	})
	if err != nil {
		t.Fatalf("PlanMerge: %v", err)
	}
	outcome, err := fixture.store.ApplyMerge(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	if outcome.Manifest == "" {
		t.Fatal("apply returned no manifest")
	}

	// The survivor lists the absorbed record.
	survivor, err := readDecisionFile(recordFilePath(t, fixture.store, fixture.project.ID, "decision", fixture.survivor))
	if err != nil {
		t.Fatalf("read survivor: %v", err)
	}
	if strings.Join(survivor.MergeInfo.MergedFrom, ",") != fixture.duplicate {
		t.Fatalf("survivor merged_from = %+v", survivor.MergeInfo)
	}
	// The duplicate is out of the live listing and in the trash as a tombstone.
	decisions, err := fixture.store.ListDecisions(ctx, fixture.project.ID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 || decisions[0].ID != fixture.survivor {
		t.Fatalf("live decisions = %+v", decisions)
	}
	trashPath := filepath.Join(fixture.store.Root(), "projects", fixture.project.ID, trashDir, "decisions")
	trashed, err := recordPaths(trashPath)
	if err != nil || len(trashed) != 1 {
		t.Fatalf("trash files = %v, %v", trashed, err)
	}
	tombstone, err := readDecisionFile(trashed[0])
	if err != nil {
		t.Fatalf("read tombstone: %v", err)
	}
	if tombstone.MergeInfo.MergedInto != fixture.survivor {
		t.Fatalf("tombstone merged_into = %q", tombstone.MergeInfo.MergedInto)
	}
	if tombstone.Validity.ValidTo.IsZero() {
		t.Fatal("tombstone is still current")
	}
	// The [[duplicate]] link now names the survivor.
	notes, err := fixture.store.ListNotes(ctx, fixture.project.ID)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes = %+v, %v", notes, err)
	}
	if !strings.Contains(notes[0].Body, "[["+fixture.survivor+"]]") {
		t.Fatalf("note body = %q", notes[0].Body)
	}
	// The manifest records ids, hashes and the before bytes.
	rawManifest, err := os.ReadFile(filepath.Join(fixture.store.Root(), filepath.FromSlash(outcome.Manifest)))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest mergeManifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if manifest.Survivor != fixture.survivor || len(manifest.Duplicates) != 1 || manifest.Status != mergeManifestApplied {
		t.Fatalf("manifest = %+v", manifest)
	}
	if len(manifest.Files) == 0 {
		t.Fatal("manifest has no files")
	}
	for _, file := range manifest.Files {
		if file.BeforeSHA256 == "" || file.AfterSHA256 == "" {
			t.Fatalf("manifest file = %+v", file)
		}
		if !file.BeforeMissing && file.BeforeBase64 == "" {
			t.Fatalf("manifest does not store the before bytes: %+v", file)
		}
	}

	// An idempotent repeat writes nothing.
	repeat, err := fixture.store.ApplyMerge(ctx, plan)
	if err != nil {
		t.Fatalf("repeat apply: %v", err)
	}
	if repeat.Manifest != "" || len(repeat.Files) != 0 {
		t.Fatalf("repeat wrote: %+v", repeat)
	}
	replan, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{fixture.duplicate},
	})
	if err != nil {
		t.Fatalf("replan: %v", err)
	}
	if replan.Changed || len(replan.AlreadyMerged) != 1 {
		t.Fatalf("replan = %+v", replan)
	}

	// Rollback restores the exact pre-merge state.
	rolled, err := fixture.store.RollbackMerge(ctx, outcome.Manifest)
	if err != nil {
		t.Fatalf("RollbackMerge: %v", err)
	}
	if rolled.Manifest != outcome.Manifest || rolled.AlreadyRolledBack {
		t.Fatalf("rollback = %+v", rolled)
	}
	sameSnapshot(t, before, snapshotProject(t, fixture.store, fixture.project.ID))
	again, err := fixture.store.RollbackMerge(ctx, outcome.Manifest)
	if err != nil || !again.AlreadyRolledBack {
		t.Fatalf("second rollback = %+v, %v", again, err)
	}
}

func TestMergeNotesMovesEntryToTrash(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	for _, note := range []domain.Note{
		{ID: "note-survivor", ProjectID: project.ID, Title: "Survivor", Body: "the surviving note", CreatedAt: testClock},
		{ID: "note-copy", ProjectID: project.ID, Title: "Copy", Body: "a duplicate note", CreatedAt: testClock},
	} {
		if _, err := store.AppendNote(ctx, note); err != nil {
			t.Fatalf("AppendNote %s: %v", note.ID, err)
		}
	}
	before := snapshotProject(t, store, project.ID)
	plan, err := store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: project.ID, Survivor: "note-survivor", Duplicates: []string{"note-copy"},
	})
	if err != nil || len(plan.Conflicts) != 0 {
		t.Fatalf("PlanMerge = %+v, %v", plan, err)
	}
	if _, err := store.ApplyMerge(ctx, plan); err != nil {
		t.Fatalf("ApplyMerge: %v", err)
	}
	notes, err := store.ListNotes(ctx, project.ID)
	if err != nil || len(notes) != 1 || notes[0].ID != "note-survivor" {
		t.Fatalf("live notes = %+v, %v", notes, err)
	}
	if len(notes[0].MergeInfo.MergedFrom) != 1 || notes[0].MergeInfo.MergedFrom[0] != "note-copy" {
		t.Fatalf("survivor merged_from = %+v", notes[0].MergeInfo)
	}
	trashed, err := store.ListTrash(ctx, project.ID)
	if err != nil || len(trashed) != 1 || trashed[0].ID != "note-copy" {
		t.Fatalf("trash = %+v, %v", trashed, err)
	}
	trashNotesPath := filepath.Join(store.Root(), "projects", project.ID, trashDir, notesDir)
	trashFiles, _ := recordPaths(trashNotesPath)
	if len(trashFiles) != 1 {
		t.Fatalf("trash note files = %v", trashFiles)
	}
	_, trashNotes, err := readNotesFile(trashFiles[0])
	if err != nil || len(trashNotes) != 1 {
		t.Fatalf("trash notes = %+v, %v", trashNotes, err)
	}
	if trashNotes[0].ID != "note-copy" || trashNotes[0].MergeInfo.MergedInto != "note-survivor" || trashNotes[0].Validity.ValidTo.IsZero() {
		t.Fatalf("tombstone note = %+v", trashNotes[0])
	}
	manifest := lastManifest(t, store)
	if rolled, err := store.RollbackMerge(ctx, manifest); err != nil || rolled.AlreadyRolledBack {
		t.Fatalf("rollback = %+v, %v", rolled, err)
	}
	sameSnapshot(t, before, snapshotProject(t, store, project.ID))
}

// lastManifest returns the newest manifest reference of a store.
func lastManifest(t *testing.T, store *Store) string {
	t.Helper()
	files := globFiles(t, filepath.Join(store.Root(), mergesDir, "*.json"))
	if len(files) == 0 {
		t.Fatal("no manifest written")
	}
	return filepath.ToSlash(files[len(files)-1])
}

func TestMergeRefusesInvalidRequests(t *testing.T) {
	fixture := newMergeFixture(t)
	ctx := context.Background()
	_, err := fixture.store.AppendNote(ctx, domain.Note{
		ID: "note-other", ProjectID: fixture.project.ID, Title: "Other", Body: "different kind", CreatedAt: testClock,
	})
	if err != nil {
		t.Fatal(err)
	}

	kindMismatch, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{"note-other"},
	})
	if err != nil {
		t.Fatalf("PlanMerge kind mismatch: %v", err)
	}
	if len(kindMismatch.Conflicts) == 0 || !strings.Contains(strings.Join(kindMismatch.Conflicts, " "), "one kind") {
		t.Fatalf("kind mismatch conflicts = %v", kindMismatch.Conflicts)
	}
	if _, err := fixture.store.ApplyMerge(ctx, kindMismatch); err == nil {
		t.Fatal("apply accepted a plan with conflicts")
	}

	self, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{fixture.survivor},
	})
	if err != nil {
		t.Fatalf("PlanMerge self: %v", err)
	}
	if len(self.Conflicts) == 0 || !strings.Contains(strings.Join(self.Conflicts, " "), "survivor cannot also be a duplicate") {
		t.Fatalf("self merge conflicts = %v", self.Conflicts)
	}

	missing, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{"decision-missing"},
	})
	if err != nil {
		t.Fatalf("PlanMerge missing: %v", err)
	}
	if len(missing.Conflicts) == 0 || !strings.Contains(strings.Join(missing.Conflicts, " "), "does not exist") {
		t.Fatalf("missing conflicts = %v", missing.Conflicts)
	}
}

func TestMergeRefusesADuplicateMergedElsewhere(t *testing.T) {
	fixture := newMergeFixture(t)
	ctx := context.Background()
	other, err := fixture.store.AppendDecision(ctx, domain.Decision{
		ID: "decision-other", ProjectID: fixture.project.ID, Title: "Another survivor",
		Status: domain.DecisionAccepted, CreatedAt: testClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = other
	plan, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{fixture.duplicate},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ApplyMerge(ctx, plan); err != nil {
		t.Fatalf("first merge: %v", err)
	}
	second, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: "decision-other", Duplicates: []string{fixture.duplicate},
	})
	if err != nil {
		t.Fatalf("PlanMerge: %v", err)
	}
	joined := strings.Join(second.Conflicts, " ")
	if !strings.Contains(joined, "already merged into "+fixture.survivor) {
		t.Fatalf("conflicts = %v", second.Conflicts)
	}
}

func TestApplyMergeRefusesDrift(t *testing.T) {
	fixture := newMergeFixture(t)
	ctx := context.Background()
	plan, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{fixture.duplicate},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Touch the note the plan would rewrite, as a concurrent writer would.
	notesPath := filepath.Join(fixture.store.Root(), "projects", fixture.project.ID, notesDir)
	noteFiles, _ := recordPaths(notesPath)
	if len(noteFiles) != 1 {
		t.Fatalf("note files = %v", noteFiles)
	}
	raw, err := os.ReadFile(noteFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(raw), "the cache plan", "the cache plan, edited", 1)
	if changed == string(raw) {
		t.Fatal("test edit did not change the note")
	}
	if err := os.WriteFile(noteFiles[0], []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	afterEdit := snapshotProject(t, fixture.store, fixture.project.ID)
	if _, err := fixture.store.ApplyMerge(ctx, plan); err == nil || !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("drifted plan apply err = %v", err)
	}
	sameSnapshot(t, afterEdit, snapshotProject(t, fixture.store, fixture.project.ID))
}

func TestRollbackRefusesANewerEdit(t *testing.T) {
	fixture := newMergeFixture(t)
	ctx := context.Background()
	plan, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{fixture.duplicate},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := fixture.store.ApplyMerge(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	survivorPath := recordFilePath(t, fixture.store, fixture.project.ID, "decision", fixture.survivor)
	raw, err := os.ReadFile(survivorPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "one store for the cache", "one store for the cache, re-edited", 1)
	if edited == string(raw) {
		t.Fatal("test edit did not change the decision")
	}
	if err := os.WriteFile(survivorPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	rolled, err := fixture.store.RollbackMerge(ctx, outcome.Manifest)
	if err == nil || !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("rollback over a newer edit err = %v", err)
	}
	if len(rolled.Conflicts) == 0 {
		t.Fatal("rollback reported no conflicts")
	}
	// The newer edit survives.
	current, err := os.ReadFile(survivorPath)
	if err != nil || string(current) != edited {
		t.Fatalf("rollback overwrote the newer edit: %v", err)
	}
}
