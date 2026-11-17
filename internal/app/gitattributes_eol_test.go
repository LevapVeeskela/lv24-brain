package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
)

// TestGitattributesForcesLFWithAutocrlf models a fresh Windows clone where
// core.autocrlf=true. The managed * text=auto eol=lf rule must keep LF in the
// index, so two machines never produce whole-file CRLF conflicts.
func TestGitattributesForcesLFWithAutocrlf(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))
	dataDir := t.TempDir()
	cfg := config.Default()
	if _, err := app.New(cfg, app.Dependencies{}).Init(context.Background(), app.InitOptions{
		ConfigPath: filepath.Join(dataDir, "brain.yaml"), Mode: config.ModeGit, DataDir: dataDir,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dataDir, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "--quiet", "-b", "main")
	git("config", "--local", "core.autocrlf", "true")
	path := filepath.Join(dataDir, "projects", "demo", "state.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("goal: from windows\r\nnext: done\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "--all")
	blob := git("show", ":projects/demo/state.md")
	if strings.Contains(blob, "\r") {
		t.Fatalf("CRLF leaked into the index blob: %q", blob)
	}
	if !strings.Contains(blob, "goal: from windows\n") {
		t.Fatalf("blob lost content: %q", blob)
	}
}
