package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func proposalKindSet(findings []app.Finding) map[app.FindingKind]int {
	kinds := map[app.FindingKind]int{}
	for _, finding := range findings {
		kinds[finding.Kind]++
	}
	return kinds
}

func TestReviewListsPendingProposals(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-closed", "Serialize writes"))
	fixture.add(t, promoteIssueItem("issue-incident", "API outage", "incident"))
	fixture.add(t, domain.Item{Ref: domain.ItemRef{ExternalID: "pr-merged"}, Kind: domain.ItemPullRequest, Title: "Merge parser fix", State: domain.ItemStateMerged})
	fixture.digest(t)

	result, err := fixture.app.Review(ctx, app.ReviewOptions{ProjectID: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if result.PendingProposalCount != 3 || len(result.Proposals) != 3 {
		t.Fatalf("review proposals = %d/%d", len(result.Proposals), result.PendingProposalCount)
	}
	if !strings.Contains(result.Summary, "## Proposals (3 pending)") {
		t.Fatalf("review summary missing proposal section:\n%s", result.Summary)
	}
	if !strings.Contains(result.Summary, "Serialize writes") {
		t.Fatalf("review summary missing proposal title:\n%s", result.Summary)
	}
	if findings, err := fixture.app.ProposalFindings(ctx, "demo"); err != nil || len(findings) != 0 {
		t.Fatalf("fresh proposal findings = %+v, %v", findings, err)
	}
}

func TestReviewCapsProposalListAtTwenty(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	for i := 0; i < 21; i++ {
		fixture.add(t, domain.Item{
			Ref:   domain.ItemRef{ExternalID: "commit-" + string(rune('a'+i))},
			Kind:  domain.ItemCommit,
			Title: "commit " + string(rune('a'+i)),
		})
	}
	fixture.digest(t)
	result, err := fixture.app.Review(ctx, app.ReviewOptions{ProjectID: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if result.PendingProposalCount != 21 || len(result.Proposals) != 20 {
		t.Fatalf("review proposals = %d/%d, want 20/21", len(result.Proposals), result.PendingProposalCount)
	}
	if !strings.Contains(result.Summary, "1 more") {
		t.Fatalf("review summary missing cap note:\n%s", result.Summary)
	}
}

func TestProposalFindingsStaleOrphanContradicted(t *testing.T) {
	ctx := context.Background()

	staleFixture := newPromoteFixture(t, domain.PublishShared)
	staleFixture.add(t, promoteClosedIssue("issue-stale", "Serialize writes"))
	staleFixture.digest(t)
	changed := promoteClosedIssue("issue-stale", "Serialize writes everywhere")
	changed.Body = "The scope changed."
	staleFixture.add(t, changed)
	stale, err := staleFixture.app.ProposalFindings(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if kinds := proposalKindSet(stale); kinds[app.FindingProposalStale] != 1 {
		t.Fatalf("stale findings = %+v", stale)
	}
	curated, err := staleFixture.app.Findings(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if kinds := proposalKindSet(curated); kinds[app.FindingProposalStale] != 0 {
		t.Fatalf("curated Findings paid proposal reads: %+v", curated)
	}

	orphanFixture := newPromoteFixture(t, domain.PublishShared)
	orphanFixture.add(t, promoteClosedIssue("issue-orphan", "Serialize writes"))
	orphanFixture.digest(t)
	if _, err := orphanFixture.items.Delete(ctx, domain.ItemRef{ConnectionID: "gh", ExternalID: "issue-orphan"}, orphanFixture.clock); err != nil {
		t.Fatal(err)
	}
	orphan, err := orphanFixture.app.ProposalFindings(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if kinds := proposalKindSet(orphan); kinds[app.FindingProposalOrphan] != 1 {
		t.Fatalf("orphan findings = %+v", orphan)
	}

	contradictedFixture := newPromoteFixture(t, domain.PublishShared)
	contradictedFixture.add(t, promoteClosedIssue("issue-contradicted", "Serialize writes"))
	contradictedFixture.digest(t)
	contradictedFixture.clock = contradictedFixture.clock.Add(time.Hour)
	if _, err := contradictedFixture.app.Decision(ctx, app.DecisionInput{
		ProjectID: "demo", Title: "Serialize writes", Status: domain.DecisionAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	contradicted, err := contradictedFixture.app.ProposalFindings(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if kinds := proposalKindSet(contradicted); kinds[app.FindingProposalContradicted] != 1 {
		t.Fatalf("contradicted findings = %+v", contradicted)
	}
}

func TestReviewFixNeverTouchesProposals(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-stale", "Serialize writes"))
	fixture.digest(t)
	changed := promoteClosedIssue("issue-stale", "Serialize writes everywhere")
	changed.Body = "The scope changed."
	fixture.add(t, changed)
	proposal := fixture.proposal(t, "issue-stale")

	if _, err := fixture.app.FixFindings(ctx, "demo", []app.FindingKind{app.FindingProposalStale}); err != nil {
		t.Fatal(err)
	}
	stored, err := fixture.store.Get(ctx, "demo", proposal.ID)
	if err != nil || stored.StatusOrPending() != domain.ProposalPending {
		t.Fatalf("fix changed proposal = %+v, %v", stored, err)
	}
	if decisions, _ := fixture.store.ListDecisions(ctx, "demo"); len(decisions) != 0 {
		t.Fatalf("fix promoted %d decisions", len(decisions))
	}
	notes, err := fixture.store.ListNotes(ctx, "demo")
	if err != nil || len(notes) != 0 {
		t.Fatalf("fix wrote notes = %+v, %v", notes, err)
	}
}

func TestContextCountsPendingProposalsOnly(t *testing.T) {
	ctx := context.Background()
	fixture := newPromoteFixture(t, domain.PublishShared)
	fixture.add(t, promoteClosedIssue("issue-closed", "Serialize writes"))
	fixture.add(t, promoteIssueItem("issue-decision", "Adopt SQLite", "architecture"))
	fixture.digest(t)

	result, err := fixture.app.Context(ctx, app.ContextOptions{ProjectID: "demo", Query: "parser"})
	if err != nil {
		t.Fatal(err)
	}
	if result.PendingProposals == nil || *result.PendingProposals != 2 {
		t.Fatalf("pending proposals = %v", result.PendingProposals)
	}
	if !strings.Contains(result.Summary, "pending_proposals: 2") {
		t.Fatalf("context summary missing count:\n%s", result.Summary)
	}
	if strings.Contains(result.Summary, "Serialize writes") {
		t.Fatalf("context leaked a proposal body:\n%s", result.Summary)
	}

	withoutProposals := app.New(config.Default(), app.Dependencies{
		Store:       fixture.store,
		Connections: fixture.connections,
		Items:       fixture.items,
		Now:         func() time.Time { return fixture.clock },
	})
	plain, err := withoutProposals.Context(ctx, app.ContextOptions{ProjectID: "demo", Query: "parser"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.PendingProposals != nil || strings.Contains(plain.Summary, "pending_proposals") {
		t.Fatalf("context without proposal store = %v\n%s", plain.PendingProposals, plain.Summary)
	}
}
