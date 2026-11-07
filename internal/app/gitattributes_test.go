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

// TestGitInitUnionMergesJournals: one user on two machines appends to the
// same journal/<user>.jsonl; the .gitattributes written by init must let a
// pull merge both appends instead of stopping on a conflict.
func TestGitInitUnionMergesJournals(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "a")
	cfg := config.Default()
	if _, err := app.New(cfg, app.Dependencies{}).Init(context.Background(), app.InitOptions{
		ConfigPath: filepath.Join(dataDir, "brain.yaml"), Mode: config.ModeGit, DataDir: dataDir,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, ".gitattributes"))
	if err != nil || !strings.Contains(string(raw), "journal/*.jsonl merge=union") {
		t.Fatalf(".gitattributes = %q, %v", raw, err)
	}

	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.test"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	journal := func(dir, line string) {
		t.Helper()
		path := filepath.Join(dir, "journal", "u.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(line + "\n")
		_ = f.Close()
	}
	remote := filepath.Join(root, "remote.git")
	git(root, "init", "-q", "--bare", "-b", "main", remote)
	git(dataDir, "init", "-q", "-b", "main")
	journal(dataDir, `{"ts":"2026-11-07T00:00:00Z","event":"seed"}`)
	git(dataDir, "add", "-A")
	git(dataDir, "commit", "-q", "-m", "seed")
	git(dataDir, "remote", "add", "origin", remote)
	git(dataDir, "push", "-q", "origin", "main")

	other := filepath.Join(root, "b")
	git(root, "clone", "-q", remote, other)
	journal(other, `{"ts":"2026-11-07T00:01:00Z","event":"from-b"}`)
	git(other, "commit", "-q", "-am", "b")
	git(other, "push", "-q", "origin", "main")

	journal(dataDir, `{"ts":"2026-11-07T00:02:00Z","event":"from-a"}`)
	git(dataDir, "commit", "-q", "-am", "a")
	git(dataDir, "pull", "-q", "--rebase", "origin", "main")

	merged, err := os.ReadFile(filepath.Join(dataDir, "journal", "u.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"seed", "from-a", "from-b"} {
		if !strings.Contains(string(merged), want) {
			t.Fatalf("merged journal lacks %q:\n%s", want, merged)
		}
	}
	if strings.Contains(string(merged), "<<<<<<<") {
		t.Fatalf("conflict markers in journal:\n%s", merged)
	}
}
