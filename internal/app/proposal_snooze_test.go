package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestSnoozeProposalHidesFromListsReviewAndDue(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-snoozed", "Serialize writes"))
	fixture.add(t, domain.Item{Ref: domain.ItemRef{ExternalID: "commit-b"}, Kind: domain.ItemCommit, Title: "fix: parser"})
	fixture.digest(t)
	stored := fixture.proposal(t, "issue-snoozed")
	until := fixture.clock.Add(16 * 24 * time.Hour)

	result, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{
		ProjectID: "demo", ProposalID: stored.ID, Until: until,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Snoozed || !result.Proposal.SnoozedUntil.Equal(until) {
		t.Fatalf("snooze result = %+v", result)
	}

	defaultPage, err := fixture.app.ListProposals(ctx, app.ProposalQuery{ProjectID: "demo", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if defaultPage.Total != 1 {
		t.Fatalf("default list total = %d, want 1", defaultPage.Total)
	}
	for _, item := range defaultPage.Items {
		if item.ID == stored.ID {
			t.Fatalf("default list returned the snoozed proposal: %+v", item)
		}
	}
	allPage, err := fixture.app.ListProposals(ctx, app.ProposalQuery{ProjectID: "demo", Limit: 50, IncludeSnoozed: true})
	if err != nil {
		t.Fatal(err)
	}
	if allPage.Total != 2 {
		t.Fatalf("include_snoozed total = %d, want 2", allPage.Total)
	}
	found := false
	for _, item := range allPage.Items {
		if item.ID != stored.ID {
			continue
		}
		found = true
		if !item.Snoozed || !item.SnoozedUntil.Equal(until) {
			t.Fatalf("snoozed row = %+v", item)
		}
	}
	if !found {
		t.Fatal("include_snoozed did not return the snoozed proposal")
	}
	filteredPage, err := fixture.app.ListProposals(ctx, app.ProposalQuery{
		ProjectID: "demo", Limit: 50,
		Statuses: []domain.ProposalStatus{domain.ProposalPending, domain.ProposalStale},
	})
	if err != nil {
		t.Fatal(err)
	}
	if filteredPage.Total != 2 {
		t.Fatalf("status-filtered total = %d, want 2", filteredPage.Total)
	}

	pending, pendingCount, err := fixture.app.PendingProposals(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if pendingCount != 1 || len(pending) != 1 || pending[0].ID == stored.ID {
		t.Fatalf("pending = %d/%d %+v", len(pending), pendingCount, pending)
	}
	if count, err := fixture.app.SnoozedProposalCount(ctx, "demo"); err != nil || count != 1 {
		t.Fatalf("snoozed count = %d, %v", count, err)
	}

	review, err := fixture.app.Review(ctx, app.ReviewOptions{ProjectID: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if review.PendingProposalCount != 1 || review.SnoozedProposalCount != 1 || len(review.Proposals) != 1 {
		t.Fatalf("review counts = %d pending/%d snoozed/%d items",
			review.PendingProposalCount, review.SnoozedProposalCount, len(review.Proposals))
	}
	if !strings.Contains(review.Summary, "## Proposals (1 pending, 1 snoozed)") {
		t.Fatalf("review summary missing snoozed count:\n%s", review.Summary)
	}

	// The snoozed candidate is not due for review while it is hidden.
	fixture.clock = fixture.clock.Add(15 * 24 * time.Hour)
	due, err := fixture.app.DueReviews(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range due {
		if item.Kind == "proposal" && item.ID == stored.ID {
			t.Fatalf("snoozed proposal reported due: %+v", item)
		}
	}

	// It returns on its own once the snooze comes due.
	fixture.clock = fixture.clock.Add(2 * 24 * time.Hour)
	due, err = fixture.app.DueReviews(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, item := range due {
		if item.Kind == "proposal" && item.ID == stored.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("due proposal missing after the snooze lapsed: %+v", due)
	}
	// Once due, the candidate counts as pending again.
	if count, err := fixture.app.SnoozedProposalCount(ctx, "demo"); err != nil || count != 0 {
		t.Fatalf("snoozed count after due = %d, %v", count, err)
	}
}

func TestSnoozeProposalValidationIdempotencyAndTerminal(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-snoozed", "Serialize writes"))
	fixture.digest(t)
	stored := fixture.proposal(t, "issue-snoozed")
	until := fixture.clock.Add(7 * 24 * time.Hour)

	first, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{ProjectID: "demo", ProposalID: stored.ID, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{ProjectID: "demo", ProposalID: stored.ID, Until: until})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Snoozed || !second.Proposal.UpdatedAt.Equal(first.Proposal.UpdatedAt) {
		t.Fatalf("repeated snooze wrote again: %+v vs %+v", first.Proposal.UpdatedAt, second.Proposal.UpdatedAt)
	}
	// A retry whose response was lost still succeeds with the revision read
	// before the first attempt: the desired state already holds.
	if retry, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{
		ProjectID: "demo", ProposalID: stored.ID, Until: until, ExpectedUpdatedAt: stored.UpdatedAt,
	}); err != nil || !retry.Snoozed {
		t.Fatalf("retried snooze = %+v, %v", retry, err)
	}

	if _, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{
		ProjectID: "demo", ProposalID: stored.ID, Until: fixture.clock.Add(-time.Hour),
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("past snooze = %v, want validation", err)
	}
	_, err = fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{
		ProjectID: "demo", ProposalID: stored.ID, Until: until.Add(time.Hour),
		ExpectedUpdatedAt: stored.UpdatedAt.Add(-time.Hour),
	})
	var conflict *domain.RevisionConflictError
	if !errors.As(err, &conflict) || conflict.ID != stored.ID {
		t.Fatalf("stale snooze CAS = %v, want RevisionConflictError", err)
	}

	cleared, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{ProjectID: "demo", ProposalID: stored.ID})
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Snoozed || !cleared.Proposal.SnoozedUntil.IsZero() {
		t.Fatalf("cleared proposal = %+v", cleared.Proposal)
	}
	again, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{ProjectID: "demo", ProposalID: stored.ID})
	if err != nil || !again.Proposal.UpdatedAt.Equal(cleared.Proposal.UpdatedAt) {
		t.Fatalf("repeated clear = %+v, %v", again.Proposal.UpdatedAt, err)
	}

	// A digest refresh keeps the user's snooze.
	if _, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{ProjectID: "demo", ProposalID: stored.ID, Until: until}); err != nil {
		t.Fatal(err)
	}
	changed := promoteClosedIssue("issue-snoozed", "Serialize writes everywhere")
	changed.Body = "The scope changed."
	fixture.add(t, changed)
	fixture.digest(t)
	refreshed := fixture.proposal(t, "issue-snoozed")
	if !refreshed.SnoozedUntil.Equal(until) {
		t.Fatalf("digest refresh cleared the snooze: %+v", refreshed)
	}

	if _, err := fixture.app.Promote(ctx, app.PromoteOptions{ProposalID: stored.ID, Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{
		ProjectID: "demo", ProposalID: stored.ID, Until: until,
	}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("snooze after promotion = %v, want validation", err)
	}
}

func TestSnoozedProposalIsSkippedByFindingsUntilDue(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-stale", "Serialize writes"))
	fixture.digest(t)
	stored := fixture.proposal(t, "issue-stale")
	until := fixture.clock.Add(7 * 24 * time.Hour)

	changed := promoteClosedIssue("issue-stale", "Serialize writes everywhere")
	changed.Body = "The scope changed."
	fixture.add(t, changed)

	findings, err := fixture.app.ProposalFindings(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("expected a proposal-stale finding before snoozing")
	}
	if _, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{ProjectID: "demo", ProposalID: stored.ID, Until: until}); err != nil {
		t.Fatal(err)
	}
	findings, err = fixture.app.ProposalFindings(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("snoozed proposal still reported: %+v", findings)
	}
	if _, err := fixture.app.SnoozeProposal(ctx, app.SnoozeOptions{ProjectID: "demo", ProposalID: stored.ID}); err != nil {
		t.Fatal(err)
	}
	findings, err = fixture.app.ProposalFindings(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("unsnoozed proposal is not reported by findings")
	}
}
