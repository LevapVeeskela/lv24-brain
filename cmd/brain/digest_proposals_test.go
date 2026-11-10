package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func setupDigestSourceBrain(t *testing.T, projects []string) (string, string, *fsstore.Store) {
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
	connection := domain.Connection{ID: "gh", Provider: "github", Projects: append([]string(nil), projects...)}
	if err := connections.Create(context.Background(), connection); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir, store
}

func addDigestItem(t *testing.T, store *fsstore.Store, projectID string, item domain.Item) {
	t.Helper()
	item.Ref.ConnectionID = "gh"
	item.ProjectID = projectID
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

func proposalFileCount(t *testing.T, dataDir, projectID string) int {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dataDir, "proposals", projectID, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	return len(paths)
}

func TestDigestSourcePreviewAndWrite(t *testing.T) {
	cfgPath, dataDir, store := setupDigestSourceBrain(t, []string{"demo"})

	closed := domain.Item{Ref: domain.ItemRef{ExternalID: "issue-closed"}, Kind: domain.ItemIssue, Title: "Serialize writes", State: domain.ItemStateClosed, Body: "Fixed by serializing writes."}
	addDigestItem(t, store, "demo", closed)
	addDigestItem(t, store, "demo", domain.Item{Ref: domain.ItemRef{ExternalID: "issue-decision"}, Kind: domain.ItemIssue, Title: "Adopt SQLite", Labels: []string{"architecture"}})
	addDigestItem(t, store, "demo", domain.Item{Ref: domain.ItemRef{ExternalID: "issue-incident"}, Kind: domain.ItemIssue, Title: "API outage", Labels: []string{"incident", "production"}})
	addDigestItem(t, store, "demo", domain.Item{Ref: domain.ItemRef{ExternalID: "pr-merged"}, Kind: domain.ItemPullRequest, Title: "Merge parser fix", State: domain.ItemStateMerged, Body: "Parser fix."})
	addDigestItem(t, store, "demo", domain.Item{Ref: domain.ItemRef{ExternalID: "commit-1"}, Kind: domain.ItemCommit, Title: "fix: parser"})
	addDigestItem(t, store, "", domain.Item{Ref: domain.ItemRef{ExternalID: "issue-unmapped"}, Kind: domain.ItemIssue, Title: "Unmapped"})
	old := domain.Item{Ref: domain.ItemRef{ExternalID: "commit-old"}, Kind: domain.ItemCommit, Title: "old commit"}
	old.UpdatedAt = time.Now().UTC().AddDate(0, 0, -40)
	addDigestItem(t, store, "demo", old)

	code, stdout, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh")
	if code != 0 {
		t.Fatalf("preview exit %d: %s", code, stderr)
	}
	for _, want := range []string{
		"brain digest --source gh — demo (preview)",
		"scanned: 7", "created: 5", "updated: 0", "unchanged: 0", "skipped: 2",
		"truncated: false", "preview: no proposal files written",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("preview stdout missing %q:\n%s", want, stdout)
		}
	}
	if got := proposalFileCount(t, dataDir, "demo"); got != 0 {
		t.Fatalf("preview wrote %d proposal files", got)
	}

	code, stdout, stderr = runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--write")
	if code != 0 {
		t.Fatalf("write exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "(write)") || !strings.Contains(stdout, "written: 5") {
		t.Fatalf("write stdout = %s", stdout)
	}
	if got := proposalFileCount(t, dataDir, "demo"); got != 5 {
		t.Fatalf("write created %d proposal files, want 5", got)
	}

	code, stdout, stderr = runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--write")
	if code != 0 {
		t.Fatalf("rerun exit %d: %s", code, stderr)
	}
	for _, want := range []string{"created: 0", "updated: 0", "unchanged: 5", "written: 0"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("rerun stdout missing %q:\n%s", want, stdout)
		}
	}
	if got := proposalFileCount(t, dataDir, "demo"); got != 5 {
		t.Fatalf("rerun duplicated proposals: %d files", got)
	}
}

func TestDigestSourceLimitRulesAndFlags(t *testing.T) {
	cfgPath, dataDir, store := setupDigestSourceBrain(t, []string{"demo"})
	addDigestItem(t, store, "demo", domain.Item{Ref: domain.ItemRef{ExternalID: "issue-decision"}, Kind: domain.ItemIssue, Title: "Adopt SQLite", Labels: []string{"decision"}})
	addDigestItem(t, store, "demo", domain.Item{Ref: domain.ItemRef{ExternalID: "commit-1"}, Kind: domain.ItemCommit, Title: "fix: parser"})

	code, stdout, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--limit", "1")
	if code != 0 {
		t.Fatalf("limit exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "scanned: 1") || !strings.Contains(stdout, "truncated: true") {
		t.Fatalf("limit stdout = %s", stdout)
	}

	code, stdout, stderr = runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--rule", "commit")
	if code != 0 {
		t.Fatalf("rule exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "created: 1") || !strings.Contains(stdout, "commit") ||
		strings.Contains(stdout, "closed-issue") || strings.Contains(stdout, "decision-label") {
		t.Fatalf("rule stdout = %s", stdout)
	}
	if got := proposalFileCount(t, dataDir, "demo"); got != 0 {
		t.Fatalf("rule preview wrote %d files", got)
	}

	for _, args := range [][]string{
		{"digest", "--config", cfgPath, "--source", "gh", "--rule", "nope"},
		{"digest", "--config", cfgPath, "--source", "gh", "--limit", "0"},
		{"digest", "--config", cfgPath, "--source", "gh", "--limit", "-1"},
		{"digest", "--config", cfgPath, "--source", "gh", "--write", "--dry-run"},
		{"digest", "--config", cfgPath, "--source", "gh", "--since", "soon"},
		{"digest", "--config", cfgPath, "--all"},
		{"digest", "--config", cfgPath, "--source", ""},
	} {
		if code, _, _ := runForTest(t, args...); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}

	oldCfg, _, oldStore := setupDigestSourceBrain(t, []string{"demo"})
	old := domain.Item{Ref: domain.ItemRef{ExternalID: "commit-old"}, Kind: domain.ItemCommit, Title: "old commit"}
	old.UpdatedAt = time.Now().UTC().AddDate(0, 0, -40)
	addDigestItem(t, oldStore, "demo", old)
	code, stdout, stderr = runForTest(t, "digest", "--config", oldCfg, "--source", "gh")
	if code != 0 || !strings.Contains(stdout, "created: 0") {
		t.Fatalf("windowed old item: exit %d %s", code, stderr)
	}
	code, stdout, stderr = runForTest(t, "digest", "--config", oldCfg, "--source", "gh", "--all")
	if code != 0 || !strings.Contains(stdout, "created: 1") || !strings.Contains(stdout, "window: all") {
		t.Fatalf("--all old item: exit %d %s", code, stderr)
	}
}

func TestDigestSourceProjectResolution(t *testing.T) {
	cfgPath, _, store := setupDigestSourceBrain(t, []string{"demo", "other"})
	addDigestItem(t, store, "demo", domain.Item{Ref: domain.ItemRef{ExternalID: "issue-demo"}, Kind: domain.ItemIssue, Title: "Demo issue", Labels: []string{"decision"}})
	addDigestItem(t, store, "other", domain.Item{Ref: domain.ItemRef{ExternalID: "issue-other"}, Kind: domain.ItemIssue, Title: "Other issue", Labels: []string{"decision"}})

	code, _, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh")
	if code != 2 || !strings.Contains(stderr, "pass --project") {
		t.Fatalf("ambiguous project: exit %d: %s", code, stderr)
	}

	code, stdout, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--project", "demo")
	if code != 0 {
		t.Fatalf("scoped exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "— demo") || !strings.Contains(stdout, "created: 1") || !strings.Contains(stdout, "skipped: 1") {
		t.Fatalf("scoped stdout = %s", stdout)
	}

	code, _, stderr = runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--project", "missing")
	if code != 2 || !strings.Contains(stderr, "not a registered project") {
		t.Fatalf("unknown project: exit %d: %s", code, stderr)
	}

	serverCfg, _ := connectionConfig(t, config.ModeServer)
	code, _, stderr = runForTest(t, "digest", "--config", serverCfg, "--source", "gh")
	if code != 2 || !strings.Contains(stderr, "local or git") {
		t.Fatalf("server mode: exit %d: %s", code, stderr)
	}
}

func TestDigestSourceJSON(t *testing.T) {
	cfgPath, _, store := setupDigestSourceBrain(t, []string{"demo"})
	addDigestItem(t, store, "demo", domain.Item{Ref: domain.ItemRef{ExternalID: "issue-closed"}, Kind: domain.ItemIssue, Title: "Serialize writes", State: domain.ItemStateClosed, Body: "Fixed."})
	addDigestItem(t, store, "demo", domain.Item{Ref: domain.ItemRef{ExternalID: "commit-1"}, Kind: domain.ItemCommit, Title: "fix: parser"})

	code, stdout, stderr := runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--json")
	if code != 0 {
		t.Fatalf("json exit %d: %s", code, stderr)
	}
	var envelope struct {
		Schema int    `json:"schema"`
		Mode   string `json:"mode"`
		Data   struct {
			Mode       string  `json:"mode"`
			Project    string  `json:"project"`
			Connection string  `json:"connection"`
			Window     string  `json:"window"`
			Since      *string `json:"since"`
			All        bool    `json:"all"`
			Write      bool    `json:"write"`
			DryRun     bool    `json:"dry_run"`
			Counts     struct {
				Scanned   int  `json:"scanned"`
				Created   int  `json:"created"`
				Updated   int  `json:"updated"`
				Unchanged int  `json:"unchanged"`
				Skipped   int  `json:"skipped"`
				Truncated bool `json:"truncated"`
			} `json:"counts"`
			Proposals []struct {
				ID        string `json:"id"`
				Kind      string `json:"kind"`
				Rule      string `json:"rule"`
				Title     string `json:"title"`
				Action    string `json:"action"`
				Status    string `json:"status"`
				ItemKey   string `json:"item_key"`
				URL       string `json:"url"`
				Citations []struct {
					ConnectionID string  `json:"connection_id"`
					ItemKey      string  `json:"item_key"`
					ObservedAt   *string `json:"observed_at"`
				} `json:"citations"`
			} `json:"proposals"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if envelope.Schema != 1 || envelope.Mode != "proposals" || envelope.Data.Mode != "proposals" ||
		envelope.Data.Project != "demo" ||
		envelope.Data.Connection != "gh" || envelope.Data.Window != "30d" || envelope.Data.Since == nil ||
		envelope.Data.All || envelope.Data.Write || envelope.Data.DryRun {
		t.Fatalf("envelope = %+v", envelope.Data)
	}
	if envelope.Data.Counts.Scanned != 2 || envelope.Data.Counts.Created != 2 || len(envelope.Data.Proposals) != 2 {
		t.Fatalf("counts/proposals = %+v / %d", envelope.Data.Counts, len(envelope.Data.Proposals))
	}
	for _, proposal := range envelope.Data.Proposals {
		if proposal.Action != "created" || proposal.Status != "pending" || proposal.ID == "" ||
			proposal.ItemKey == "" || proposal.URL == "" || len(proposal.Citations) == 0 ||
			proposal.Citations[0].ConnectionID != "gh" || proposal.Citations[0].ObservedAt == nil {
			t.Fatalf("proposal = %+v", proposal)
		}
	}

	code, stdout, stderr = runForTest(t, "digest", "--config", cfgPath, "--source", "gh", "--write", "--json")
	if code != 0 {
		t.Fatalf("write json exit %d: %s", code, stderr)
	}
	envelope.Data.Proposals = nil
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decode write %q: %v", stdout, err)
	}
	if !envelope.Data.Write || envelope.Data.Counts.Created != 2 {
		t.Fatalf("write envelope = %+v", envelope.Data)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfgPath), "proposals", "demo")); err != nil {
		t.Fatalf("write did not create proposals: %v", err)
	}
}

func TestDigestWithoutSourceIsUnchanged(t *testing.T) {
	cfgPath := setupCatchupBrain(t)
	code, stdout, stderr := runForTest(t, "digest", "--config", cfgPath)
	if code != 0 || !strings.Contains(stdout, "brain digest — last 24h") {
		t.Fatalf("plain digest: exit %d: %s / %s", code, stdout, stderr)
	}
}
