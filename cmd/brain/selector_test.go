package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentWorkflowFromRepositoryDirectory covers how agents call the engine
// directly (without brain.ps1): from inside a registered checkout, with
// positional text, flags after positionals, automatic git context, and the
// combined open output.
func TestAgentWorkflowFromRepositoryDirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	repo := filepath.Join(root, "work", "alpha")
	if err := os.MkdirAll(filepath.Join(repo, "graphify-out"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "seed"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	for _, id := range []string{"alpha", "beta"} {
		args := []string{"project", "set", id, "--config", cfgPath, "--kind", "repository"}
		if id == "alpha" {
			args = append(args, "--path", repo)
		}
		if code, _, stderr := runForTest(t, args...); code != 0 {
			t.Fatalf("project set %s: %s", id, stderr)
		}
	}
	t.Chdir(filepath.Join(repo))

	code, stdout, stderr := runForTest(t, "note", "Fixed the cache\nlonger body line", "--config", cfgPath)
	if code != 0 || !strings.HasPrefix(stdout, "note note-alpha-") {
		t.Fatalf("positional note: %d %q %s", code, stdout, stderr)
	}
	if code, stdout, stderr = runForTest(t, "decision", "Adopt Go engine", "--config", cfgPath); code != 0 || !strings.Contains(stdout, "decision-alpha-") {
		t.Fatalf("positional decision: %d %q %s", code, stdout, stderr)
	}
	if code, _, _ = runForTest(t, "decision", "A", "--title", "B", "--config", cfgPath); code != 2 {
		t.Fatalf("positional + --title exit = %d, want 2", code)
	}
	if code, stdout, stderr = runForTest(t, "handoff", "Session done", "--config", cfgPath); code != 0 {
		t.Fatalf("handoff: %d %q %s", code, stdout, stderr)
	}
	handoffs, _ := filepath.Glob(filepath.Join(dataDir, "projects", "alpha", "handoffs", "*.md"))
	if len(handoffs) != 1 {
		t.Fatalf("handoffs = %v", handoffs)
	}
	raw, _ := os.ReadFile(handoffs[0])
	if !strings.Contains(string(raw), "branch=main") || !strings.Contains(string(raw), "- ") {
		t.Fatalf("handoff lacks automatic git context:\n%s", raw)
	}

	code, stdout, stderr = runForTest(t, "open", ".", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("open .: %d %s", code, stderr)
	}
	for _, want := range []string{"# alpha — bootstrap", "Adopt Go engine", "Session done", "## Git", "branch: main", "## Graphs", "- graphify: "} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("open . missing %q:\n%s", want, stdout)
		}
	}
	if code, stdout, _ = runForTest(t, "bootstrap", "beta", "--config", cfgPath); code != 0 || !strings.Contains(stdout, "# beta — bootstrap") {
		t.Fatalf("bootstrap beta: %d %q", code, stdout)
	}
}

func TestConfigUseMakesFlaglessCommandsWork(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	userDir := t.TempDir()
	t.Setenv("APPDATA", userDir)         // Windows UserConfigDir
	t.Setenv("XDG_CONFIG_HOME", userDir) // Linux
	t.Setenv("HOME", userDir)            // macOS
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	t.Chdir(t.TempDir())
	if code, _, _ := runForTest(t, "note", "--project", "p", "x"); code == 0 {
		t.Fatal("note without any config succeeded")
	}
	if code, stdout, stderr := runForTest(t, "config", "use", filepath.Join(dataDir, "brain.yaml")); code != 0 || !strings.Contains(stdout, "default config:") {
		t.Fatalf("config use: %d %q %s", code, stdout, stderr)
	}
	if code, stdout, stderr := runForTest(t, "note", "--project", "p", "flagless"); code != 0 || !strings.HasPrefix(stdout, "note note-p-") {
		t.Fatalf("flagless note: %d %q %s", code, stdout, stderr)
	}
	if code, stdout, _ := runForTest(t, "config", "show"); code != 0 || !strings.Contains(stdout, "user default") {
		t.Fatalf("config show: %d %q", code, stdout)
	}
	if code, _, _ := runForTest(t, "config", "use", filepath.Join(root, "missing.yaml")); code == 0 {
		t.Fatal("config use accepted a missing file")
	}
	if code, _, _ := runForTest(t, "config", "clear"); code != 0 {
		t.Fatal("config clear failed")
	}
}

func TestProjectSetAddPathKeepsExistingPaths(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	existing := t.TempDir()
	other := filepath.Join(t.TempDir(), "elsewhere-on-another-machine")
	if code, _, stderr := runForTest(t, "project", "set", "p", "--path", other, "--config", cfgPath); code != 0 {
		t.Fatalf("set: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "project", "set", "p", "--add-path", existing, "--add-path", other, "--config", cfgPath); code != 0 {
		t.Fatalf("add-path: %s", stderr)
	}
	code, stdout, _ := runForTest(t, "project", "show", "p", "--json", "--config", cfgPath)
	if code != 0 || !strings.Contains(stdout, "elsewhere-on-another-machine") || strings.Count(stdout, "elsewhere-on-another-machine") != 1 {
		t.Fatalf("paths after add-path = %s", stdout)
	}
	// Opening by id uses the path that exists on this machine, not the
	// first one listed.
	code, stdout, _ = runForTest(t, "open", "p", "--config", cfgPath)
	if code != 0 || strings.Contains(stdout, "elsewhere-on-another-machine") {
		t.Fatalf("open p = %d %s", code, stdout)
	}
	if code, _, _ := runForTest(t, "project", "set", "p", "--add-path", existing, "--clear-paths", "--config", cfgPath); code != 2 {
		t.Fatal("--add-path with --clear-paths accepted")
	}
}
