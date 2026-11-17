package fsstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// mergeLockFiles lists the per-file locks left under root.
func mergeLockFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".lock") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk lock files: %v", err)
	}
	return out
}

// TestMergeApplyRefusesStalePlanWithoutWriting covers a writer that changed a
// planned file between PlanMerge and ApplyMerge: the typed stale_plan error is
// returned, no record and no manifest is written.
func TestMergeApplyRefusesStalePlanWithoutWriting(t *testing.T) {
	fixture := newMergeFixture(t)
	ctx := context.Background()
	plan, err := fixture.store.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{fixture.duplicate},
	})
	if err != nil {
		t.Fatalf("PlanMerge: %v", err)
	}
	notesPath := filepath.Join(fixture.store.Root(), "projects", fixture.project.ID, notesDir)
	noteFiles, _ := recordPaths(notesPath)
	if len(noteFiles) != 1 {
		t.Fatalf("note files = %v", noteFiles)
	}
	raw, err := os.ReadFile(noteFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "the cache plan", "the cache plan, edited", 1)
	if edited == string(raw) {
		t.Fatal("test edit did not change the note")
	}
	if err := os.WriteFile(noteFiles[0], []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	afterEdit := snapshotProject(t, fixture.store, fixture.project.ID)

	_, err = fixture.store.ApplyMerge(ctx, plan)
	var stale *StalePlanError
	if !errors.As(err, &stale) {
		t.Fatalf("ApplyMerge error = %v, want *StalePlanError", err)
	}
	if stale.Code() != "stale_plan" {
		t.Fatalf("stale code = %q, want stale_plan", stale.Code())
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale plan does not unwrap to ErrConflict: %v", err)
	}
	sameSnapshot(t, afterEdit, snapshotProject(t, fixture.store, fixture.project.ID))
	if manifests := globFiles(t, filepath.Join(fixture.store.Root(), mergesDir, "*.json")); len(manifests) != 0 {
		t.Fatalf("a stale apply wrote manifests: %v", manifests)
	}
	if locks := mergeLockFiles(t, fixture.store.Root()); len(locks) != 0 {
		t.Fatalf("a stale apply left locks: %v", locks)
	}
}

// TestMergeApplyReverifiesFingerprintUnderTheLock covers the window after the
// plan was built but before the per-file locks are taken: the stamp check must
// see the change and refuse the write.
func TestMergeApplyReverifiesFingerprintUnderTheLock(t *testing.T) {
	fixture := newMergeFixture(t)
	ctx := context.Background()
	work, err := fixture.store.planMerge(ctx, ports.MergeRequest{
		ProjectID: fixture.project.ID, Survivor: fixture.survivor, Duplicates: []string{fixture.duplicate},
	})
	if err != nil {
		t.Fatalf("planMerge: %v", err)
	}
	stamps, err := planMergeStamps(work.files)
	if err != nil {
		t.Fatalf("planMergeStamps: %v", err)
	}
	survivorPath := recordFilePath(t, fixture.store, fixture.project.ID, "decision", fixture.survivor)
	raw, err := os.ReadFile(survivorPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(survivorPath, append(raw, []byte("\nconcurrent edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	err = fixture.store.verifyMergeStamps(stamps)
	var stale *StalePlanError
	if !errors.As(err, &stale) {
		t.Fatalf("verifyMergeStamps error = %v, want *StalePlanError", err)
	}
	survivorRel, err := filepath.Rel(fixture.store.Root(), survivorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(stale.Paths, ","), filepath.ToSlash(survivorRel)) {
		t.Fatalf("stale paths = %v, want %s", stale.Paths, survivorRel)
	}
}

// TestMergeStampsIncludeSizeAndMtime proves the identity is not hash-only: a
// rewrite with identical bytes but a newer mtime is still a stale plan.
func TestMergeStampsIncludeSizeAndMtime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.md")
	if err := os.WriteFile(path, []byte("goal: same bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp, err := captureMergeStamp(path)
	if err != nil {
		t.Fatalf("captureMergeStamp: %v", err)
	}
	if !stamp.Exists || stamp.SHA256 == "" || stamp.Size != int64(len("goal: same bytes\n")) {
		t.Fatalf("stamp = %+v", stamp)
	}
	// Rewrite the same bytes: the hash and size match, the mtime may advance.
	if err := os.WriteFile(path, []byte("goal: same bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	current, err := captureMergeStamp(path)
	if err != nil {
		t.Fatalf("captureMergeStamp after rewrite: %v", err)
	}
	if current.SHA256 != stamp.SHA256 || current.Size != stamp.Size {
		t.Fatalf("rewrite changed the hash identity: %+v -> %+v", stamp, current)
	}
	stamps := []mergeTargetStamp{{path: path, label: "state.md", stamp: stamp}}
	err = (&Store{}).verifyMergeStamps(stamps)
	if stamp.ModTime.Equal(current.ModTime) {
		if err != nil {
			t.Fatalf("an unchanged file was refused: %v", err)
		}
		return
	}
	var stale *StalePlanError
	if !errors.As(err, &stale) {
		t.Fatalf("a rewrite with a newer mtime was accepted: %v", err)
	}
}

// TestMergeApplyAndRollbackLeaveNoLocks runs a full merge and rollback and
// checks every per-file lock was released.
func TestMergeApplyAndRollbackLeaveNoLocks(t *testing.T) {
	fixture := newMergeFixture(t)
	ctx := context.Background()
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
	if locks := mergeLockFiles(t, fixture.store.Root()); len(locks) != 0 {
		t.Fatalf("apply left locks: %v", locks)
	}
	manifest, err := readMergeManifest(filepath.Join(fixture.store.Root(), filepath.FromSlash(outcome.Manifest)))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, file := range manifest.Files {
		if file.After.SHA256 == "" || file.After.Size == 0 || file.After.ModTime.IsZero() {
			t.Fatalf("manifest file has no applied stamp: %+v", file)
		}
	}
	if _, err := fixture.store.RollbackMerge(ctx, outcome.Manifest); err != nil {
		t.Fatalf("RollbackMerge: %v", err)
	}
	if locks := mergeLockFiles(t, fixture.store.Root()); len(locks) != 0 {
		t.Fatalf("rollback left locks: %v", locks)
	}
}

// TestMergeRollbackRefusesAChangedFileWithStalePlan covers the rollback guard:
// a file edited after the merge is reported as stale_plan and is not restored.
func TestMergeRollbackRefusesAChangedFileWithStalePlan(t *testing.T) {
	fixture := newMergeFixture(t)
	ctx := context.Background()
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
	survivorPath := recordFilePath(t, fixture.store, fixture.project.ID, "decision", fixture.survivor)
	raw, err := os.ReadFile(survivorPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := append(raw, []byte("\nedited after the merge\n")...)
	if err := os.WriteFile(survivorPath, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	rolled, err := fixture.store.RollbackMerge(ctx, outcome.Manifest)
	var stale *StalePlanError
	if !errors.As(err, &stale) {
		t.Fatalf("RollbackMerge error = %v, want *StalePlanError", err)
	}
	if stale.Code() != "stale_plan" || !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("rollback stale error = %v", err)
	}
	if len(rolled.Conflicts) == 0 {
		t.Fatal("rollback reported no conflicts")
	}
	current, err := os.ReadFile(survivorPath)
	if err != nil || string(current) != string(edited) {
		t.Fatalf("rollback overwrote the newer edit: %v", err)
	}
	if locks := mergeLockFiles(t, fixture.store.Root()); len(locks) != 0 {
		t.Fatalf("refused rollback left locks: %v", locks)
	}
}
