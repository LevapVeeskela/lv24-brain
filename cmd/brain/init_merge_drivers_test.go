package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitRegistersMergeDriversInExistingRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))

	dataDir := filepath.Join(t.TempDir(), "data")
	if err := exec.Command("git", "init", "--quiet", "-b", "main", dataDir).Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	code, _, stderr := runForTest(t, "init", "--mode", "git", "--data-dir", dataDir, "--config", cfgPath)
	if code != 0 {
		t.Fatalf("init exit code = %d: %s", code, stderr)
	}
	cmd := exec.Command("git", "-C", dataDir, "config", "--local", "--get", "merge.lv24-state.driver")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("merge.lv24-state.driver is not registered: %v (%s)", err, out)
	}
	if !strings.Contains(string(out), "merge-driver state") {
		t.Fatalf("driver = %q", out)
	}
}
