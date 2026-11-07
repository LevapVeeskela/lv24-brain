package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A new instance has no global/AGENTS.md: --hooks must still install hooks.
func TestAgentsSyncHooksWithoutSharedInstructions(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	code, stdout, stderr := runForTest(t, "agents", "sync", "--hooks", "--home", home, "--root", root, "--config", cfgPath)
	if code != 0 || !strings.Contains(stdout, "skipped (no shared instructions") || !strings.Contains(stdout, "updated "+filepath.Join(home, ".codex", "hooks.json")) {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("workspace AGENTS.md written without instructions: %v", err)
	}
	// Without --hooks the missing instructions stay an error.
	if code, _, _ := runForTest(t, "agents", "sync", "--home", home, "--root", root, "--config", cfgPath); code == 0 {
		t.Fatal("agents sync without instructions succeeded")
	}
}
