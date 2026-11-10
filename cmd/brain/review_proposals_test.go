package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestReviewCLIListsProposalsAndFindings(t *testing.T) {
	cfgPath, store := setupPromoteBrain(t, domain.PublishShared)
	addPromoteCLIItem(t, store, promoteClosedCLIItem("issue-closed", "Serialize writes"))
	addPromoteCLIItem(t, store, domain.Item{
		Ref: domain.ItemRef{ExternalID: "issue-incident"}, Kind: domain.ItemIssue,
		Title: "API outage", Labels: []string{"incident"}, Body: "500s.",
	})
	addPromoteCLIItem(t, store, domain.Item{
		Ref: domain.ItemRef{ExternalID: "pr-merged"}, Kind: domain.ItemPullRequest,
		Title: "Merge parser fix", State: domain.ItemStateMerged, Body: "Parser fix.",
	})
	if code, _, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--write"); code != 0 {
		t.Fatalf("digest: exit %d: %s", code, stderr)
	}

	code, stdout, stderr := runForTest(t, "review", "--config", cfgPath, "--project", "demo")
	if code != 0 {
		t.Fatalf("review exit %d: %s", code, stderr)
	}
	proposalsAt := strings.Index(stdout, "## Proposals (3 pending)")
	findingsAt := strings.Index(stdout, "## Findings")
	if proposalsAt < 0 || findingsAt < 0 || proposalsAt > findingsAt {
		t.Fatalf("review sections out of order (proposals %d, findings %d):\n%s", proposalsAt, findingsAt, stdout)
	}

	_, data := runJSONForTest(t, "review", "--json", "--config", cfgPath, "--project", "demo")
	proposals, ok := data["proposals"].([]any)
	if !ok || len(proposals) != 3 {
		t.Fatalf("review proposals = %#v", data["proposals"])
	}
	first, _ := proposals[0].(map[string]any)
	for _, key := range []string{"id", "kind", "title", "rule", "confidence", "status", "connection_id"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("proposal json missing %q: %#v", key, first)
		}
	}

	// Make one proposal stale, one orphaned and one contradicted.
	changed := promoteClosedCLIItem("issue-closed", "Serialize writes everywhere")
	changed.Body = "The scope changed."
	addPromoteCLIItem(t, store, changed)
	if _, err := store.Items().Delete(context.Background(), domain.ItemRef{ConnectionID: "gh", ExternalID: "pr-merged"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runForTest(t, "decision", "--config", cfgPath, "--project", "demo",
		"--title", "API outage", "--status", "accepted", "--context", "Incident review"); code != 0 {
		t.Fatalf("decision: exit %d: %s", code, stderr)
	}

	_, data = runJSONForTest(t, "review", "--json", "--config", cfgPath, "--project", "demo")
	findings, _ := data["findings"].([]any)
	kinds := map[string]int{}
	for _, raw := range findings {
		finding, _ := raw.(map[string]any)
		kind, _ := finding["kind"].(string)
		kinds[kind]++
	}
	for _, kind := range []string{"proposal-stale", "proposal-orphan", "proposal-contradicted"} {
		if kinds[kind] != 1 {
			t.Fatalf("finding %s = %d, findings = %#v", kind, kinds[kind], findings)
		}
	}

	before := proposalStatuses(t, store)
	if code, _, stderr := runForTest(t, "review", "--config", cfgPath, "--project", "demo", "--fix"); code != 0 {
		t.Fatalf("review --fix exit %d: %s", code, stderr)
	}
	after := proposalStatuses(t, store)
	if len(before) != len(after) {
		t.Fatalf("review --fix changed proposal count: %d -> %d", len(before), len(after))
	}
	for id, status := range before {
		if after[id] != status {
			t.Fatalf("review --fix changed proposal %s: %s -> %s", id, status, after[id])
		}
	}
	if status := after[domain.ProposalID("demo", "gh", "issue-closed")]; status != domain.ProposalPending {
		t.Fatalf("stale proposal status = %s, want pending", status)
	}
}

func TestContextCLICountsPendingProposals(t *testing.T) {
	cfgPath, store := setupPromoteBrain(t, domain.PublishShared)
	addPromoteCLIItem(t, store, promoteClosedCLIItem("issue-closed", "Serialize writes"))
	addPromoteCLIItem(t, store, domain.Item{
		Ref: domain.ItemRef{ExternalID: "issue-incident"}, Kind: domain.ItemIssue,
		Title: "API outage", Labels: []string{"incident"},
	})
	if code, _, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--write"); code != 0 {
		t.Fatalf("digest: exit %d: %s", code, stderr)
	}
	code, stdout, stderr := runForTest(t, "context", "--config", cfgPath, "--project", "demo", "parser")
	if code != 0 {
		t.Fatalf("context exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "pending_proposals: 2") {
		t.Fatalf("context stdout missing pending count:\n%s", stdout)
	}
	if strings.Contains(stdout, "Serialize writes") || strings.Contains(stdout, "API outage") {
		t.Fatalf("context leaked a proposal title:\n%s", stdout)
	}
}

func proposalStatuses(t *testing.T, store *fsstore.Store) map[string]domain.ProposalStatus {
	t.Helper()
	proposals, err := store.List(context.Background(), "demo", ports.ProposalListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]domain.ProposalStatus, len(proposals))
	for _, proposal := range proposals {
		statuses[proposal.ID] = proposal.StatusOrPending()
	}
	return statuses
}
