package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// publishGit runs one git command in dir with an isolated identity.
func publishGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "core.autocrlf=false", "-c", "gc.auto=0"}
	cmd := exec.Command("git", append(base, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// publishFixture is a git-mode data directory with one catalogue connection
// whose raw zone is already tracked by an engine before 0.15.
func publishFixture(t *testing.T) (cfgPath, dataDir string, item domain.Item, itemPath string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	cfgPath, dataDir = connectionConfig(t, config.ModeGit)
	if _, err := app.New(config.Default(), app.Dependencies{}).Init(context.Background(), app.InitOptions{
		ConfigPath: cfgPath, Mode: config.ModeGit, DataDir: dataDir,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	publishGit(t, dataDir, "init", "-q", "-b", "main")
	publishGit(t, dataDir, "add", "-A")
	publishGit(t, dataDir, "commit", "-q", "-m", "seed")

	ctx := context.Background()
	catalogue, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatalf("NewConnectionStore: %v", err)
	}
	connection := domain.Connection{ID: "gh-acme", Provider: "github", Account: "acme", Settings: map[string]string{"repos": "a/b"}}
	if err := catalogue.Create(ctx, connection); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	item = domain.Item{
		Ref:        domain.ItemRef{ConnectionID: "gh-acme", ExternalID: "issue/1"},
		Kind:       domain.ItemIssue,
		Title:      "Legacy issue",
		ObservedAt: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC),
	}.WithHash()
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_, hexPart, _ := strings.Cut(item.Key(), "/")
	itemPath = filepath.Join(dataDir, "connectors", "gh-acme", "items", hexPart+".md")
	markerPath := filepath.Join(dataDir, "connectors", "gh-acme", ".gitignore")
	legacy := "# lv24-brain: shared connection; only lock and temporary files are ignored.\n*.lock\n.tmp-*\n"
	if err := os.WriteFile(markerPath, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}
	publishGit(t, dataDir, "add", "-f", itemPath, markerPath)
	publishGit(t, dataDir, "commit", "-q", "-m", "legacy raw zone")
	return cfgPath, dataDir, item, itemPath
}

// TestConnectionPublishLocalOnlyUntracks: the command records the choice, writes
// the marker, untracks the raw zone and reports what moved. It is idempotent.
func TestConnectionPublishLocalOnlyUntracks(t *testing.T) {
	cfgPath, dataDir, _, itemPath := publishFixture(t)
	code, out, errOut := runForTest(t, "connection", "publish", "gh-acme", "local-only", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("publish: %d %s", code, errOut)
	}
	for _, want := range []string{"gh-acme publish=local-only", "raw-zone files tracked by git: 1 -> 0", "git history is not rewritten"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	catalogue, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatalf("NewConnectionStore: %v", err)
	}
	connection, err := catalogue.Get(context.Background(), "gh-acme")
	if err != nil || connection.Publish != domain.PublishLocalOnly {
		t.Fatalf("catalogue publish = %q, %v", connection.Publish, err)
	}
	if tracked := strings.TrimSpace(publishGit(t, dataDir, "ls-files", "--", "connectors")); tracked != "" {
		t.Fatalf("still tracked:\n%s", tracked)
	}
	if _, err := os.Stat(itemPath); err != nil {
		t.Fatalf("untrack removed the file: %v", err)
	}

	code, out, errOut = runForTest(t, "connection", "publish", "gh-acme", "local-only", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("second publish: %d %s", code, errOut)
	}
	if !strings.Contains(out, "raw-zone files tracked by git: 0 -> 0") || !strings.Contains(out, "Nothing was tracked.") {
		t.Fatalf("second run output:\n%s", out)
	}
}

// TestConnectionPublishSharedKeepsItems: switching back to shared records the
// policy and keeps items tracked.
func TestConnectionPublishSharedKeepsItems(t *testing.T) {
	cfgPath, dataDir, _, itemPath := publishFixture(t)
	code, out, errOut := runForTest(t, "connection", "publish", "gh-acme", "shared", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("publish: %d %s", code, errOut)
	}
	report := connectionJSON[struct {
		ID            string               `json:"id"`
		Publish       domain.PublishPolicy `json:"publish"`
		TrackedBefore int                  `json:"trackedBefore"`
		TrackedAfter  int                  `json:"trackedAfter"`
	}](t, out)
	if report.ID != "gh-acme" || report.Publish != domain.PublishShared || report.TrackedBefore != 1 || report.TrackedAfter != 1 {
		t.Fatalf("report = %+v", report)
	}
	if tracked := publishGit(t, dataDir, "ls-files", "--", "connectors/gh-acme"); !strings.Contains(tracked, filepath.ToSlash(itemPath[len(dataDir)+1:])) {
		t.Fatalf("shared item was untracked:\n%s", tracked)
	}
}

// TestConnectionPublishRejectsBadInput: an unknown policy is a usage error and
// an unknown connection is a runtime error; neither changes anything.
func TestConnectionPublishRejectsBadInput(t *testing.T) {
	cfgPath, dataDir, _, _ := publishFixture(t)
	if code, _, _ := runForTest(t, "connection", "publish", "gh-acme", "public", "--config", cfgPath); code != 2 {
		t.Fatalf("bad policy exit = %d, want 2", code)
	}
	if code, _, errOut := runForTest(t, "connection", "publish", "missing", "shared", "--config", cfgPath); code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("unknown connection exit = %d: %s", code, errOut)
	}
	catalogue, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatalf("NewConnectionStore: %v", err)
	}
	connection, err := catalogue.Get(context.Background(), "gh-acme")
	if err != nil || connection.Publish != "" {
		t.Fatalf("rejected commands changed the catalogue: %q, %v", connection.Publish, err)
	}
	if tracked := strings.TrimSpace(publishGit(t, dataDir, "ls-files", "--", "connectors")); tracked == "" {
		t.Fatal("rejected commands untracked the raw zone")
	}
}
