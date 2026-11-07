package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestReviewFixCLI: a next action left behind the latest handoff is reported,
// `review --fix` applies it, and a second review no longer reports it.
func TestReviewFixCLI(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "note", "--create", "--config", cfg, "--project", "demo", "--title", "Start", "--body", "x"); code != 0 {
		t.Fatalf("note: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "state", "set", "demo", "--next", "old step", "--config", cfg); code != 0 {
		t.Fatalf("state set: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "handoff", "--project", "demo", "--summary", "did it", "--next", "new step", "--no-git", "--config", cfg); code != 0 {
		t.Fatalf("handoff: %s", stderr)
	}

	code, stdout, stderr := runForTest(t, "review", "demo", "--config", cfg)
	if code != 0 {
		t.Fatalf("review exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "[next-behind-handoff]") || !strings.Contains(stdout, "run `brain review --fix` to apply the fixable findings") {
		t.Fatalf("review stdout = %q", stdout)
	}

	_, data := runJSONForTest(t, "review", "demo", "--json", "--config", cfg)
	findings, ok := data["findings"].([]any)
	if !ok || len(findings) == 0 {
		t.Fatalf("review findings = %+v", data["findings"])
	}
	first := findings[0].(map[string]any)
	if first["kind"] != "next-behind-handoff" || first["fixable"] != true {
		t.Fatalf("first finding = %+v", first)
	}

	code, stdout, stderr = runForTest(t, "review", "demo", "--fix", "--config", cfg)
	if code != 0 {
		t.Fatalf("review --fix exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "## Applied") {
		t.Fatalf("review --fix stdout = %q", stdout)
	}

	code, stdout, _ = runForTest(t, "state", "get", "demo", "--json", "--config", cfg)
	if code != 0 || !strings.Contains(stdout, `"nextAction": "new step"`) {
		t.Fatalf("state after fix: %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "review", "demo", "--config", cfg)
	if code != 0 || strings.Contains(stdout, "next-behind-handoff") {
		t.Fatalf("second review: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestReviewFixKindRejectsUnknownKind(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	if code, _, _ := runForTest(t, "review", "--fix-kind", "bogus", "--config", cfg); code == 0 {
		t.Fatal("review --fix-kind bogus exited 0")
	}
}
