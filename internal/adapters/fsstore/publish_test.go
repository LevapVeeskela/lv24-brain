package fsstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestConcurrentDecisionsPublishCompleteFiles(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	const writers = 16

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.AppendDecision(ctx, domain.Decision{
				ProjectID: project.ID,
				Title:     "Same title",
				Context:   "same day, same slug",
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("AppendDecision: %v", err)
	}

	decisionsDir := filepath.Join(store.Root(), "projects", project.ID, "decisions")
	files := globFiles(t, filepath.Join(decisionsDir, "*.md"))
	if len(files) != writers {
		t.Fatalf("decision files = %d, want %d", len(files), writers)
	}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() == 0 {
			t.Fatalf("published decision %s is empty", file)
		}
	}
	if leftovers := globFiles(t, filepath.Join(decisionsDir, tempFilePrefix+"*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
	decisions, err := store.ListDecisions(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != writers {
		t.Fatalf("listed decisions = %d, want %d", len(decisions), writers)
	}
	problems, err := store.Problems(ctx, project.ID)
	if err != nil {
		t.Fatalf("Problems: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
}

func TestPublishFallsBackWhenHardLinksUnsupported(t *testing.T) {
	old := hardLink
	hardLink = func(string, string) error { return errors.New("hard links not supported") }
	t.Cleanup(func() { hardLink = old })

	store := newTestStore(t)
	project := seedProject(t, store)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := store.AppendAttempt(ctx, domain.Attempt{
			ProjectID:  project.ID,
			Title:      "Retry",
			Hypothesis: "h",
			Result:     "r",
			Reason:     "x",
		}); err != nil {
			t.Fatalf("AppendAttempt %d: %v", i, err)
		}
	}
	attemptsDir := filepath.Join(store.Root(), "projects", project.ID, "attempts")
	files := globFiles(t, filepath.Join(attemptsDir, "*.md"))
	if len(files) != 2 {
		t.Fatalf("attempt files = %v, want 2", files)
	}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() == 0 {
			t.Fatalf("fallback published empty file %s", file)
		}
	}
	attempts, err := store.ListAttempts(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("listed attempts = %d, want 2", len(attempts))
	}
}
