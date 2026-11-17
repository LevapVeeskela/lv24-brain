package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitstore"
	"github.com/levapveeskela/lv24-brain/internal/config"
)

func gitOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))
}

func TestSyncPreflightSelfHealsAttributesAndDrivers(t *testing.T) {
	gitOrSkip(t)
	dataDir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dataDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.test")
	// Simulate a second machine: only the journal line, no drivers registered.
	if err := os.WriteFile(filepath.Join(dataDir, ".gitattributes"), []byte("journal/*.jsonl merge=union\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, driver := range gitstore.MergeDrivers() {
		cmd := exec.Command("git", "config", "--local", "--unset-all", "merge."+driver.Name+".driver")
		cmd.Dir = dataDir
		_ = cmd.Run()
	}
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := gitstore.Open(dataDir, gitstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = dataDir
	a := New(cfg, Dependencies{Store: store, Sync: provider, MergeDrivers: provider})

	if err := a.syncPreflight(context.Background()); err != nil {
		t.Fatalf("syncPreflight: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"* text=auto eol=lf",
		"projects/*/state.md merge=lv24-state",
		"registry.json merge=lv24-registry",
		"projects/*/notes/*.md merge=lv24-notes",
		"connectors/*/items/*.md merge=lv24-items",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf(".gitattributes misses %q:\n%s", want, raw)
		}
	}
	missing, err := provider.MissingMergeDrivers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("drivers still missing after preflight: %v", missing)
	}
	// Re-running is harmless and does not duplicate lines.
	if err := a.syncPreflight(context.Background()); err != nil {
		t.Fatalf("second syncPreflight: %v", err)
	}
	raw, _ = os.ReadFile(filepath.Join(dataDir, ".gitattributes"))
	if got := strings.Count(string(raw), "projects/*/state.md merge=lv24-state"); got != 1 {
		t.Fatalf("state attribute appears %d times", got)
	}
}

func TestSyncPreflightRejectsNestedDataDir(t *testing.T) {
	gitOrSkip(t)
	root := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(root, "init", "--quiet", "-b", "main")
	nested := filepath.Join(root, "data")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	provider, err := gitstore.Open(nested, gitstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = nested
	a := New(cfg, Dependencies{Sync: provider})
	if err := a.syncPreflight(context.Background()); err == nil {
		t.Fatal("nested data directory was accepted by preflight")
	}
}
