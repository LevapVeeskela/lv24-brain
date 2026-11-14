package app_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestConcurrentPromoteCreatesUniqueRecords promotes 100 proposals at once.
// Promotion used to pin a deterministic record id computed above the store, so
// two writers could publish two files with one id; every promoted record now
// gets its own ULID through the ordinary write path.
func TestConcurrentPromoteCreatesUniqueRecords(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)

	const count = 100
	for i := 0; i < count; i++ {
		fixture.add(t, promoteClosedIssue(
			fmt.Sprintf("issue-%03d", i),
			fmt.Sprintf("Serialize writes %03d", i),
		))
	}
	fixture.digest(t)
	pending := fixture.pending(t)
	if len(pending) != count {
		t.Fatalf("pending proposals = %d, want %d", len(pending), count)
	}

	errs := make([]error, count)
	var wg sync.WaitGroup
	for i, proposal := range pending {
		wg.Add(1)
		go func(i int, proposalID string) {
			defer wg.Done()
			_, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposalID})
			errs[i] = err
		}(i, proposal.ID)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("promote %d: %v", i, err)
		}
	}

	decisions, err := fixture.store.ListDecisions(ctx, "demo")
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != count {
		t.Fatalf("decisions = %d, want %d", len(decisions), count)
	}
	seen := make(map[string]struct{}, count)
	for _, decision := range decisions {
		if !domain.IsRecordID("decision", decision.ID) {
			t.Fatalf("promoted decision id = %q, want <kind>-<date>-<slug>-<ULID>", decision.ID)
		}
		if _, duplicate := seen[decision.ID]; duplicate {
			t.Fatalf("duplicate promoted decision id %q", decision.ID)
		}
		seen[decision.ID] = struct{}{}
		path := filepath.Join(fixture.dataDir, "projects", "demo", "decisions", decision.ID+".md")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("decision %q has no file named after it: %v", decision.ID, err)
		}
	}
}
