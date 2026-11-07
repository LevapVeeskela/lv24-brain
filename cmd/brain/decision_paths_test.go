package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecisionCLIPathsFlag(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	recordCLIOK(t, "init", "--mode", "local", "--data-dir", dataDir)
	recordCLIOK(t, "decision", "--create", "--config", cfg, "--project", "demo",
		"--title", "Keep wire format", "--paths", "internal/wire/, cmd/brain/main.go ,")

	files, err := filepath.Glob(filepath.Join(dataDir, "projects", "demo", "decisions", "*.md"))
	if err != nil || len(files) != 1 {
		t.Fatalf("decision files = %v (err %v)", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "## Paths\n\n- internal/wire/\n- cmd/brain/main.go\n") {
		t.Fatalf("paths not persisted:\n%s", raw)
	}
}

func TestDecisionCLIRejectsEscapingPath(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := filepath.Join(dataDir, "brain.yaml")
	recordCLIOK(t, "init", "--mode", "local", "--data-dir", dataDir)
	code, _, stderr := recordCLI(t, "decision", "--create", "--config", cfg, "--project", "demo",
		"--title", "Escape", "--paths", "../secrets.env")
	if code == 0 || !strings.Contains(stderr, "decision.paths") {
		t.Fatalf("exit %d, stderr %q; want a decision.paths validation error", code, stderr)
	}
}
