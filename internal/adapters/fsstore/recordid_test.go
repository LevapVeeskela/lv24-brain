package fsstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// withoutRevisionHash skips the whole-tree revision hash. Publishing thousands
// of records would otherwise rehash every earlier file on each write and turn
// the test into a quadratic disk benchmark.
func withoutRevisionHash(t *testing.T) {
	t.Helper()
	previous := revisionAfterWrite
	revisionAfterWrite = func(*Store, context.Context) (domain.SyncRevision, error) {
		return domain.SyncRevision{Kind: domain.SyncKindGit, Value: "test"}, nil
	}
	t.Cleanup(func() { revisionAfterWrite = previous })
}

func testULID(t *testing.T) string {
	t.Helper()
	ulid, err := domain.NewULID(testClock)
	if err != nil {
		t.Fatalf("NewULID: %v", err)
	}
	return ulid
}

func TestAppendDecisionUsesRecordIDAsFileName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	id := domain.NewRecordID("decision", testClock, "pick-storage", testULID(t))

	if _, err := store.AppendDecision(ctx, domain.Decision{
		ID:        id,
		ProjectID: project.ID,
		Title:     "Pick storage",
		Status:    domain.DecisionProposed,
		Context:   "context",
		CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}

	dir := filepath.Join(store.Root(), "projects", project.ID, decisionsDir)
	paths := globFiles(t, filepath.Join(dir, "*.md"))
	if len(paths) != 1 {
		t.Fatalf("decision files = %v, want one", paths)
	}
	if got := filepath.Base(paths[0]); got != id+".md" {
		t.Fatalf("file name = %q, want %q", got, id+".md")
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 || decisions[0].ID != id {
		t.Fatalf("decisions = %+v, want one with id %q", decisions, id)
	}
}

func TestAppendDecisionTargetExistsNamesExistingRecord(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	id := domain.NewRecordID("decision", testClock, "same-id", testULID(t))
	first := domain.Decision{
		ID: id, ProjectID: project.ID, Title: "Same id", Status: domain.DecisionProposed,
		Context: "first", CreatedAt: testClock,
	}
	if _, err := store.AppendDecision(ctx, first); err != nil {
		t.Fatalf("first AppendDecision: %v", err)
	}
	second := first
	second.Context = "second"
	_, err := store.AppendDecision(ctx, second)
	if !errors.Is(err, domain.ErrTargetExists) {
		t.Fatalf("second AppendDecision error = %v, want ErrTargetExists", err)
	}
	// Record restore callers have always treated a taken name as a conflict.
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second AppendDecision error = %v, want ErrConflict too", err)
	}
	var exists *domain.TargetExistsError
	if !errors.As(err, &exists) || exists.ID != id {
		t.Fatalf("error %v does not name the existing record %q", err, id)
	}
	if paths := globFiles(t, filepath.Join(store.Root(), "projects", project.ID, decisionsDir, "*.md")); len(paths) != 1 {
		t.Fatalf("decision files = %v, want one (no suffix copy)", paths)
	}
}

func TestAppendDecisionLegacyNameKeepsSuffix(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	for i, id := range []string{"dec-one", "dec-two"} {
		if _, err := store.AppendDecision(ctx, domain.Decision{
			ID: id, ProjectID: project.ID, Title: "Same title", Status: domain.DecisionProposed,
			Context: fmt.Sprintf("context %d", i), CreatedAt: testClock,
		}); err != nil {
			t.Fatalf("AppendDecision %s: %v", id, err)
		}
	}
	names := []string{}
	for _, path := range globFiles(t, filepath.Join(store.Root(), "projects", project.ID, decisionsDir, "*.md")) {
		names = append(names, filepath.Base(path))
	}
	want := []string{"2026-11-05-same-title-2.md", "2026-11-05-same-title.md"}
	if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] {
		t.Fatalf("legacy names = %v, want %v", names, want)
	}
}

// TestConcurrentDecisionWritesCreateUniqueFiles reproduces the record-id race:
// every writer uses the same wall-clock second, so a second-granularity id
// would collide. The ULID part must keep both the ids and the files unique.
func TestConcurrentDecisionWritesCreateUniqueFiles(t *testing.T) {
	withoutRevisionHash(t)
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)

	const writers = 1000
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		title := fmt.Sprintf("Concurrent decision %04d", i)
		ulid, err := domain.NewULID(testClock)
		if err != nil {
			t.Fatalf("NewULID: %v", err)
		}
		id := domain.NewRecordID("decision", testClock, domain.Slugify(title), ulid)
		wg.Add(1)
		go func(i int, id, title string) {
			defer wg.Done()
			_, err := store.AppendDecision(ctx, domain.Decision{
				ID:        id,
				ProjectID: project.ID,
				Title:     title,
				Status:    domain.DecisionProposed,
				Context:   fmt.Sprintf("written by writer %d", i),
				CreatedAt: testClock,
			})
			if err != nil {
				errs <- fmt.Errorf("writer %d: %w", i, err)
			}
		}(i, id, title)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AppendDecision: %v", err)
	}

	dir := filepath.Join(store.Root(), "projects", project.ID, decisionsDir)
	paths := globFiles(t, filepath.Join(dir, "*.md"))
	if len(paths) != writers {
		t.Fatalf("decision files = %d, want %d", len(paths), writers)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != writers {
		t.Fatalf("listed decisions = %d, want %d", len(decisions), writers)
	}
	byName := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		byName[filepath.Base(path)] = struct{}{}
	}
	seen := make(map[string]struct{}, writers)
	for _, decision := range decisions {
		if !domain.IsRecordID("decision", decision.ID) {
			t.Fatalf("legacy-shaped id %q under concurrency", decision.ID)
		}
		if _, duplicate := seen[decision.ID]; duplicate {
			t.Fatalf("duplicate record id %q", decision.ID)
		}
		seen[decision.ID] = struct{}{}
		if _, ok := byName[decision.ID+".md"]; !ok {
			t.Fatalf("record %q has no file named after it", decision.ID)
		}
	}
}

// TestRestoreRecordDoesNotOverwriteConcurrentlyPublishedRecord forces the
// interleaving the old check-then-rename allowed: a restore and a create race
// for the same live name. Exactly one must win; the other must report the
// conflict, and the trashed bytes must never disappear.
func TestRestoreRecordDoesNotOverwriteConcurrentlyPublishedRecord(t *testing.T) {
	withoutRevisionHash(t)
	ctx := context.Background()
	const iterations = 1000
	store := newTestStore(t)
	project := seedProject(t, store)
	liveDir := filepath.Join(store.Root(), "projects", project.ID, decisionsDir)
	trashKindDir := filepath.Join(store.Root(), "projects", project.ID, trashDir, decisionsDir)
	for i := 0; i < iterations; i++ {
		id := domain.NewRecordID("decision", testClock, "restore-race", testULID(t))
		original := domain.Decision{
			ID: id, ProjectID: project.ID, Title: "Restore race", Status: domain.DecisionProposed,
			Context: "original trashed content", CreatedAt: testClock,
		}
		if _, err := store.AppendDecision(ctx, original); err != nil {
			t.Fatalf("iteration %d: AppendDecision: %v", i, err)
		}
		if err := store.TrashRecord(ctx, project.ID, ports.RecordDecision, id); err != nil {
			t.Fatalf("iteration %d: TrashRecord: %v", i, err)
		}
		concurrent := original
		concurrent.Context = "concurrently published content"

		var (
			wg         sync.WaitGroup
			restoreErr error
			publishErr error
			start      = make(chan struct{})
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			restoreErr = store.RestoreRecord(ctx, project.ID, ports.RecordDecision, id)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, publishErr = store.AppendDecision(ctx, concurrent)
		}()
		close(start)
		wg.Wait()

		live, err := store.ListDecisions(ctx, project.ID)
		if err != nil {
			t.Fatalf("iteration %d: ListDecisions: %v", i, err)
		}
		trashed, err := store.ListTrash(ctx, project.ID)
		if err != nil {
			t.Fatalf("iteration %d: ListTrash: %v", i, err)
		}
		switch {
		case restoreErr == nil:
			if publishErr == nil {
				t.Fatalf("iteration %d: restore and publish both succeeded", i)
			}
			if !errors.Is(publishErr, domain.ErrTargetExists) {
				t.Fatalf("iteration %d: publish error = %v, want ErrTargetExists", i, publishErr)
			}
			if len(live) != 1 || live[0].Context != original.Context {
				t.Fatalf("iteration %d: live = %+v, want the restored original", i, live)
			}
			if len(trashed) != 0 {
				t.Fatalf("iteration %d: trash still holds the restored record: %+v", i, trashed)
			}
		default:
			if !errors.Is(restoreErr, domain.ErrTargetExists) {
				t.Fatalf("iteration %d: restore error = %v, want ErrTargetExists", i, restoreErr)
			}
			if publishErr != nil {
				t.Fatalf("iteration %d: restore and publish both failed: %v / %v", i, restoreErr, publishErr)
			}
			if len(live) != 1 || live[0].Context != concurrent.Context {
				t.Fatalf("iteration %d: live = %+v, want the concurrent record", i, live)
			}
			if len(trashed) != 1 || trashed[0].ID != id {
				t.Fatalf("iteration %d: trashed = %+v, want the original still in trash", i, trashed)
			}
			trashPath, err := findRecordPath(trashKindDir, ports.RecordDecision, id)
			if err != nil {
				t.Fatalf("iteration %d: original lost from trash: %v", i, err)
			}
			kept, err := readDecisionFile(trashPath)
			if err != nil {
				t.Fatalf("iteration %d: read trashed original: %v", i, err)
			}
			if kept.Context != original.Context {
				t.Fatalf("iteration %d: trashed original context = %q, want %q", i, kept.Context, original.Context)
			}
		}
		// The record is live exactly once: a restore never leaves two live
		// records and never makes the only copy disappear.
		if len(live) != 1 {
			t.Fatalf("iteration %d: %d live records, want exactly one", i, len(live))
		}
		// Keep the directories small so listing cost does not grow with the
		// iteration count.
		_ = os.Remove(filepath.Join(liveDir, id+".md"))
		_ = os.Remove(filepath.Join(trashKindDir, id+".md"))
	}
}

// TestReaderAcceptsLegacyAndNewFileNames pins the compatibility contract: a
// data directory written by an older engine and a new ULID-named file list
// together.
func TestReaderAcceptsLegacyAndNewFileNames(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ID: "decision-legacy", ProjectID: project.ID, Title: "Legacy naming",
		Status: domain.DecisionProposed, Context: "old", CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("legacy AppendDecision: %v", err)
	}
	newID := domain.NewRecordID("decision", testClock, "new-naming", testULID(t))
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ID: newID, ProjectID: project.ID, Title: "New naming",
		Status: domain.DecisionProposed, Context: "new", CreatedAt: testClock.Add(time.Second),
	}); err != nil {
		t.Fatalf("new AppendDecision: %v", err)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 2 {
		t.Fatalf("decisions = %+v, want legacy and new", decisions)
	}
	ids := map[string]bool{}
	for _, decision := range decisions {
		ids[decision.ID] = true
	}
	if !ids["decision-legacy"] || !ids[newID] {
		t.Fatalf("listed ids = %v, want both %q and %q", ids, "decision-legacy", newID)
	}
}
