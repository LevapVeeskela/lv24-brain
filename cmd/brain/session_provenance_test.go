package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/config"
)

// TestSessionEndWritesAgentProvenance checks the automatic session-end capture:
// the handoff and the decisions captured from the final message are agent
// writes with stated trust. With security.quarantine_agent they stay
// unreviewed; otherwise they are reviewed so the next open still sees them.
func TestSessionEndWritesAgentProvenance(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	project := func(id string) string {
		checkout := filepath.Join(root, id)
		if err := os.MkdirAll(checkout, 0o755); err != nil {
			t.Fatal(err)
		}
		if code, _, stderr := runForTest(t, "project", "set", id, "--add-path", checkout, "--config", cfgPath); code != 0 {
			t.Fatalf("project set %s: %s", id, stderr)
		}
		return checkout
	}
	trustedCheckout := project("demo")
	quarantinedCheckout := project("strict")

	// Default: reviewed agent handoff + captured decision, trust stated.
	hook := func(cwd, message string) string {
		raw, err := json.Marshal(map[string]string{"cwd": cwd, "last_assistant_message": message})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	message := "Done: shipped the parser\nDecision: adopt automatic provenance capture"
	if code, _, stderr := runWithInputForTest(t, hook(trustedCheckout, message),
		"session", "end", "--hook", "claude", "--config", cfgPath); code != 0 {
		t.Fatalf("session end: %s", stderr)
	}
	assertProvenanceFile(t, filepath.Join(dataDir, "projects", "demo", "handoffs"), "origin: agent", "trust: stated")
	assertNoReviewedFalse(t, filepath.Join(dataDir, "projects", "demo", "handoffs"))
	assertProvenanceFile(t, filepath.Join(dataDir, "projects", "demo", "decisions"), "origin: agent", "trust: stated")
	assertNoReviewedFalse(t, filepath.Join(dataDir, "projects", "demo", "decisions"))

	// security.quarantine_agent: the same capture is unreviewed. The OpenCode
	// hook has no final message, so a note supplies the activity digest.
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	loaded.Security.QuarantineAgent = true
	if err := config.Save(cfgPath, loaded); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	if code, _, stderr := runForTest(t, "note", "--create", "--config", cfgPath, "--project", "strict", "--title", "Started", "--body", "work"); code != 0 {
		t.Fatalf("note: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "session", "end", "--hook", "opencode", "--cwd", quarantinedCheckout, "--config", cfgPath); code != 0 {
		t.Fatalf("opencode session end: %s", stderr)
	}
	assertProvenanceFile(t, filepath.Join(dataDir, "projects", "strict", "handoffs"), "origin: agent", "trust: stated", "reviewed: false")
}

// assertProvenanceFile checks that the single Markdown file in dir contains
// every wanted line.
func assertProvenanceFile(t *testing.T, dir string, want ...string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("files in %s = %v, %v", dir, paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range want {
		if !strings.Contains(string(raw), line) {
			t.Fatalf("%s misses %q:\n%s", paths[0], line, raw)
		}
	}
}

func assertNoReviewedFalse(t *testing.T, dir string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no files in %s: %v", dir, err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "reviewed: false") {
			t.Fatalf("%s is unreviewed by default:\n%s", path, raw)
		}
	}
}
