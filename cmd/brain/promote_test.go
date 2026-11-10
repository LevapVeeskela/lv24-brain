package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func setupPromoteBrain(t *testing.T, publish domain.PublishPolicy) (string, *fsstore.Store) {
	t.Helper()
	cfgPath := setupCatchupBrain(t)
	dataDir := filepath.Dir(cfgPath)
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	connections, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	connection := domain.Connection{ID: "gh", Provider: "github", Projects: []string{"demo"}, Publish: publish}
	if err := connections.Create(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	return cfgPath, store
}

func addPromoteCLIItem(t *testing.T, store *fsstore.Store, item domain.Item) {
	t.Helper()
	item.Ref.ConnectionID = "gh"
	item.ProjectID = "demo"
	if item.ObservedAt.IsZero() {
		item.ObservedAt = time.Now().UTC()
	}
	if item.URL == "" {
		item.URL = "https://example.test/items/" + item.Ref.ExternalID
	}
	item.Hash = item.ComputeHash()
	if _, err := store.Items().Upsert(context.Background(), item); err != nil {
		t.Fatal(err)
	}
}

func promoteClosedCLIItem(externalID, title string) domain.Item {
	return domain.Item{
		Ref:   domain.ItemRef{ExternalID: externalID},
		Kind:  domain.ItemIssue,
		Title: title,
		State: domain.ItemStateClosed,
		Body:  "The work is complete.",
	}
}

func pendingCLIProposals(t *testing.T, store *fsstore.Store) []domain.Proposal {
	t.Helper()
	proposals, err := store.List(context.Background(), "demo", ports.ProposalListOptions{
		Statuses: []domain.ProposalStatus{domain.ProposalPending, domain.ProposalStale},
	})
	if err != nil {
		t.Fatal(err)
	}
	return proposals
}

func TestPromoteCLIHumanAndJSON(t *testing.T) {
	cfgPath, store := setupPromoteBrain(t, domain.PublishShared)
	addPromoteCLIItem(t, store, promoteClosedCLIItem("issue-closed", "Serialize writes"))
	if code, _, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--write"); code != 0 {
		t.Fatalf("digest: exit %d: %s", code, stderr)
	}
	decisionID := domain.ProposalID("demo", "gh", "issue-closed")

	code, stdout, stderr := runForTest(t, "promote", "--config", cfgPath, decisionID)
	if code != 0 {
		t.Fatalf("promote exit %d: %s", code, stderr)
	}
	for _, want := range []string{"promoted:", decisionID, "decision "} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("promote stdout missing %q:\n%s", want, stdout)
		}
	}
	decisions, err := store.ListDecisions(context.Background(), "demo")
	if err != nil || len(decisions) != 1 || decisions[0].Source == nil ||
		decisions[0].Source.ProposalID != decisionID {
		t.Fatalf("decisions = %+v, %v", decisions, err)
	}
	recordID := decisions[0].ID

	code, stdout, stderr = runForTest(t, "promote", "--config", cfgPath, decisionID)
	if code != 0 || !strings.Contains(stdout, "already promoted") || !strings.Contains(stdout, recordID) {
		t.Fatalf("idempotent promote: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if decisions, _ := store.ListDecisions(context.Background(), "demo"); len(decisions) != 1 {
		t.Fatalf("rerun duplicated decisions: %d", len(decisions))
	}

	addPromoteCLIItem(t, store, domain.Item{
		Ref: domain.ItemRef{ExternalID: "issue-incident"}, Kind: domain.ItemIssue,
		Title: "API outage", Labels: []string{"incident", "production"}, Body: "500s for ten minutes.",
	})
	if code, _, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--write"); code != 0 {
		t.Fatalf("second digest: exit %d: %s", code, stderr)
	}
	attemptID := domain.ProposalID("demo", "gh", "issue-incident")
	code, stdout, stderr = runForTest(t, "promote", "--config", cfgPath, attemptID, "--as", "decision", "--status", "accepted", "--title", "Accept the outage", "--json")
	if code != 0 {
		t.Fatalf("json promote exit %d: %s", code, stderr)
	}
	var envelope struct {
		Schema int `json:"schema"`
		Data   struct {
			ProposalID string `json:"proposal_id"`
			Status     string `json:"status"`
			Already    bool   `json:"already"`
			Record     struct {
				Kind  string `json:"kind"`
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"record"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if envelope.Schema != 1 || envelope.Data.ProposalID != attemptID ||
		envelope.Data.Status != "promoted" || envelope.Data.Already ||
		envelope.Data.Record.Kind != "decision" || envelope.Data.Record.ID == "" ||
		envelope.Data.Record.Title != "Accept the outage" {
		t.Fatalf("promote json = %+v", envelope.Data)
	}
	decisions, _ = store.ListDecisions(context.Background(), "demo")
	if len(decisions) != 2 {
		t.Fatalf("decisions after --as = %d", len(decisions))
	}

	code, stdout, stderr = runForTest(t, "promote", "--config", cfgPath, decisionID, "--json")
	if code != 0 || !strings.Contains(stdout, `"already": true`) {
		t.Fatalf("idempotent json: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestPromoteCLIDismissDryRunAndExitCodes(t *testing.T) {
	cfgPath, store := setupPromoteBrain(t, domain.PublishShared)
	addPromoteCLIItem(t, store, promoteClosedCLIItem("issue-a", "Serialize writes"))
	addPromoteCLIItem(t, store, domain.Item{Ref: domain.ItemRef{ExternalID: "commit-b"}, Kind: domain.ItemCommit, Title: "fix: parser"})
	if code, _, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--write"); code != 0 {
		t.Fatalf("digest: exit %d: %s", code, stderr)
	}
	issueID := domain.ProposalID("demo", "gh", "issue-a")
	commitID := domain.ProposalID("demo", "gh", "commit-b")

	code, stdout, stderr := runForTest(t, "promote", "--config", cfgPath, issueID, "--dry-run")
	if code != 0 || !strings.Contains(stdout, "dry-run") || !strings.Contains(stdout, "would promote") {
		t.Fatalf("dry-run: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if decisions, _ := store.ListDecisions(context.Background(), "demo"); len(decisions) != 0 {
		t.Fatalf("dry-run wrote %d decisions", len(decisions))
	}
	if stored, _ := store.Get(context.Background(), "demo", issueID); stored.StatusOrPending() != domain.ProposalPending {
		t.Fatalf("dry-run status = %s", stored.Status)
	}

	code, stdout, stderr = runForTest(t, "promote", "--config", cfgPath, commitID, "--dismiss")
	if code != 0 || !strings.Contains(stdout, "dismissed:") {
		t.Fatalf("dismiss: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if stored, _ := store.Get(context.Background(), "demo", commitID); stored.Status != domain.ProposalDismissed {
		t.Fatalf("dismiss status = %s", stored.Status)
	}
	code, stdout, _ = runForTest(t, "promote", "--config", cfgPath, commitID, "--dismiss")
	if code != 0 || !strings.Contains(stdout, "already dismissed") {
		t.Fatalf("idempotent dismiss: exit %d stdout %q", code, stdout)
	}
	if code, _, _ := runForTest(t, "promote", "--config", cfgPath, commitID); code != 1 {
		t.Fatalf("promote dismissed exit = %d, want 1", code)
	}
	if code, _, _ := runForTest(t, "promote", "--config", cfgPath, domain.ProposalID("demo", "gh", "missing")); code != 1 {
		t.Fatalf("missing proposal exit = %d, want 1", code)
	}

	for _, args := range [][]string{
		{"promote", "--config", cfgPath, issueID, "--as", "handoff"},
		{"promote", "--config", cfgPath, issueID, "--status", "rejected"},
		{"promote", "--config", cfgPath, issueID, "--dismiss", "--title", "x"},
		{"promote", "--config", cfgPath},
		{"promote", "--config", cfgPath, issueID, "extra"},
	} {
		if code, _, _ := runForTest(t, args...); code != 2 {
			t.Fatalf("%v exit = %d, want 2", args, code)
		}
	}
}

func TestPromoteCLILocalOnlyAndStale(t *testing.T) {
	localCfg, localStore := setupPromoteBrain(t, domain.PublishLocalOnly)
	addPromoteCLIItem(t, localStore, promoteClosedCLIItem("issue-local", "Serialize writes"))
	if code, _, stderr := runForTest(t, "digest", "--config", localCfg, "--source", "gh", "--write"); code != 0 {
		t.Fatalf("digest local: exit %d: %s", code, stderr)
	}
	localID := domain.ProposalID("demo", "gh", "issue-local")
	code, _, stderr := runForTest(t, "promote", "--config", localCfg, localID)
	if code != 2 || !strings.Contains(stderr, "--yes") {
		t.Fatalf("local-only promote: exit %d stderr %q", code, stderr)
	}
	if code, _, stderr := runForTest(t, "promote", "--config", localCfg, localID, "--yes"); code != 0 {
		t.Fatalf("local-only --yes: exit %d: %s", code, stderr)
	}

	cfgPath, store := setupPromoteBrain(t, domain.PublishShared)
	addPromoteCLIItem(t, store, promoteClosedCLIItem("issue-stale", "Serialize writes"))
	if code, _, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--write"); code != 0 {
		t.Fatalf("digest stale: exit %d: %s", code, stderr)
	}
	staleID := domain.ProposalID("demo", "gh", "issue-stale")
	changed := promoteClosedCLIItem("issue-stale", "Serialize writes everywhere")
	changed.Body = "The scope changed."
	addPromoteCLIItem(t, store, changed)
	code, _, stderr = runForTest(t, "promote", "--config", cfgPath, staleID)
	if code != 1 || !strings.Contains(stderr, "--force") {
		t.Fatalf("stale promote: exit %d stderr %q", code, stderr)
	}
	if code, _, stderr := runForTest(t, "promote", "--config", cfgPath, staleID, "--force"); code != 0 {
		t.Fatalf("stale --force: exit %d: %s", code, stderr)
	}

	orphanCfg, orphanStore := setupPromoteBrain(t, domain.PublishShared)
	addPromoteCLIItem(t, orphanStore, promoteClosedCLIItem("issue-orphan", "Serialize writes"))
	if code, _, stderr := runForTest(t, "digest", "--config", orphanCfg, "--source", "gh", "--write"); code != 0 {
		t.Fatalf("digest orphan: exit %d: %s", code, stderr)
	}
	orphanID := domain.ProposalID("demo", "gh", "issue-orphan")
	if _, err := orphanStore.Items().Delete(context.Background(), domain.ItemRef{ConnectionID: "gh", ExternalID: "issue-orphan"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runForTest(t, "promote", "--config", orphanCfg, orphanID); code != 1 {
		t.Fatalf("orphan promote exit = %d, want 1", code)
	}
	if code, _, stderr := runForTest(t, "promote", "--config", orphanCfg, orphanID, "--force"); code != 0 {
		t.Fatalf("orphan --force: exit %d: %s", code, stderr)
	}
}
