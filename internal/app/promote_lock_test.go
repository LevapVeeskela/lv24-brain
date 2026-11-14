package app_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestConcurrentPromoteSameProposalWritesOneRecord closes the residual race in
// Promote: two callers that both pass the idempotency scan could each create a
// curated record for one pending proposal. With the per-proposal lock exactly
// one caller writes the record and every other caller reports it as already
// promoted with the same record id.
func TestConcurrentPromoteSameProposalWritesOneRecord(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-race", "Promote one proposal once"))
	fixture.digest(t)
	proposal := fixture.proposal(t, "issue-race")

	const callers = 50
	results := make([]app.PromoteResult, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID})
		}(i)
	}
	close(start)
	wg.Wait()

	decisions, err := fixture.store.ListDecisions(ctx, "demo")
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("decisions = %d, want exactly 1", len(decisions))
	}
	recordID := decisions[0].ID
	created := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("promote %d: %v", i, errs[i])
		}
		if results[i].Record.ID != recordID {
			t.Fatalf("promote %d record = %q, want %q", i, results[i].Record.ID, recordID)
		}
		if !results[i].Already {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("fresh promotions = %d, want exactly 1", created)
	}
	stored := fixture.proposal(t, "issue-race")
	if stored.Status != domain.ProposalPromoted || stored.PromotedRecordID != recordID {
		t.Fatalf("stored proposal = %+v, want promoted with %s", stored, recordID)
	}
}

// TestConcurrentPromoteDifferentProposalsAllSucceed verifies the lock is scoped
// to one (project, proposal): unrelated promotions still run in parallel and
// every one of them writes its own record.
func TestConcurrentPromoteDifferentProposalsAllSucceed(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)

	const count = 50
	for i := 0; i < count; i++ {
		fixture.add(t, promoteClosedIssue(
			fmt.Sprintf("issue-parallel-%03d", i),
			fmt.Sprintf("Promote independent proposal %03d", i),
		))
	}
	fixture.digest(t)
	pending := fixture.pending(t)
	if len(pending) != count {
		t.Fatalf("pending proposals = %d, want %d", len(pending), count)
	}

	errs := make([]error, count)
	results := make([]app.PromoteResult, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, proposal := range pending {
		wg.Add(1)
		go func(i int, proposalID string) {
			defer wg.Done()
			<-start
			results[i], errs[i] = fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposalID})
		}(i, proposal.ID)
	}
	close(start)
	wg.Wait()

	seen := make(map[string]struct{}, count)
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("promote %d: %v", i, errs[i])
		}
		if results[i].Already {
			t.Fatalf("promote %d was unexpectedly idempotent", i)
		}
		if _, duplicate := seen[results[i].Record.ID]; duplicate {
			t.Fatalf("duplicate promoted record id %q", results[i].Record.ID)
		}
		seen[results[i].Record.ID] = struct{}{}
	}
	decisions, err := fixture.store.ListDecisions(ctx, "demo")
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != count {
		t.Fatalf("decisions = %d, want %d", len(decisions), count)
	}
}

// TestConcurrentPromoteAndDismissKeepOneTerminalState races a promotion against
// a dismissal of the same pending proposal. The loser must observe the terminal
// status instead of overwriting it, and a promoted proposal must own exactly one
// curated record.
func TestConcurrentPromoteAndDismissKeepOneTerminalState(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)

	const count = 25
	for i := 0; i < count; i++ {
		fixture.add(t, promoteClosedIssue(
			fmt.Sprintf("issue-dismiss-%03d", i),
			fmt.Sprintf("Race promote and dismiss %03d", i),
		))
	}
	fixture.digest(t)
	pending := fixture.pending(t)
	if len(pending) != count {
		t.Fatalf("pending proposals = %d, want %d", len(pending), count)
	}

	records := make(map[string]int, count)
	for _, proposal := range pending {
		var promoteResult, dismissResult app.PromoteResult
		var promoteErr, dismissErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			promoteResult, promoteErr = fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID})
		}()
		go func() {
			defer wg.Done()
			<-start
			dismissResult, dismissErr = fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: proposal.ID, Dismiss: true})
		}()
		close(start)
		wg.Wait()

		if promoteErr == nil && dismissErr == nil && !promoteResult.Already && !dismissResult.Dismissed && !dismissResult.Already {
			t.Fatalf("proposal %s: neither caller reported a terminal state", proposal.ID)
		}
		stored, err := fixture.store.Get(ctx, "demo", proposal.ID)
		if err != nil {
			t.Fatalf("get proposal %s: %v", proposal.ID, err)
		}
		if !stored.Status.Terminal() {
			t.Fatalf("proposal %s status = %q, want a terminal status", proposal.ID, stored.Status)
		}
		if dismissResult.Dismissed && stored.Status != domain.ProposalDismissed {
			t.Fatalf("dismissal reported for proposal %s but status = %q", proposal.ID, stored.Status)
		}
		if promoteErr == nil && !promoteResult.Already && promoteResult.Record.ID != "" &&
			stored.Status != domain.ProposalPromoted {
			t.Fatalf("promotion reported for proposal %s but status = %q", proposal.ID, stored.Status)
		}
		if stored.Status == domain.ProposalPromoted && stored.PromotedRecordID != "" {
			records[stored.PromotedRecordID]++
		}
	}

	decisions, err := fixture.store.ListDecisions(ctx, "demo")
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	perProposal := make(map[string]int, count)
	for _, decision := range decisions {
		if decision.Source == nil {
			continue
		}
		perProposal[decision.Source.ProposalID]++
	}
	for _, proposal := range pending {
		stored, err := fixture.store.Get(ctx, "demo", proposal.ID)
		if err != nil {
			t.Fatalf("get proposal %s: %v", proposal.ID, err)
		}
		switch stored.Status {
		case domain.ProposalPromoted:
			if perProposal[proposal.ID] != 1 {
				t.Fatalf("promoted proposal %s has %d curated records, want 1", proposal.ID, perProposal[proposal.ID])
			}
		case domain.ProposalDismissed:
			if perProposal[proposal.ID] != 0 {
				t.Fatalf("dismissed proposal %s has %d curated records, want 0", proposal.ID, perProposal[proposal.ID])
			}
		}
	}
	for recordID, uses := range records {
		if uses != 1 {
			t.Fatalf("promoted record %s is claimed by %d proposals, want 1", recordID, uses)
		}
	}
}
