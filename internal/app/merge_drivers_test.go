package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
)

func TestGitInitWritesMergeDriverAttributes(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := config.Default()
	useCase := app.New(cfg, app.Dependencies{})
	initOptions := app.InitOptions{
		ConfigPath: filepath.Join(dataDir, "brain.yaml"),
		Mode:       config.ModeGit,
		DataDir:    dataDir,
	}
	if _, err := useCase.Init(context.Background(), initOptions); err != nil {
		t.Fatalf("Init: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, ".gitattributes"))
	if err != nil {
		t.Fatalf("read .gitattributes: %v", err)
	}
	for _, want := range []string{
		"journal/*.jsonl merge=union",
		"projects/*/state.md merge=lv24-state",
		"registry.json merge=lv24-registry",
		"projects/*/notes/*.md merge=lv24-notes",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf(".gitattributes misses %q:\n%s", want, raw)
		}
	}

	// A second init must not duplicate the managed lines.
	if _, err := useCase.Init(context.Background(), initOptions); err != nil {
		t.Fatalf("second Init: %v", err)
	}
	again, err := os.ReadFile(filepath.Join(dataDir, ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(again), "projects/*/state.md merge=lv24-state"); got != 1 {
		t.Fatalf("state driver line appears %d times:\n%s", got, again)
	}
}

func TestGitInitKeepsExistingAttributesAndAddsDriverLines(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, ".gitattributes"), []byte("*.png binary\njournal/*.jsonl merge=union\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	_, err := app.New(cfg, app.Dependencies{}).Init(context.Background(), app.InitOptions{
		ConfigPath: filepath.Join(dataDir, "brain.yaml"),
		Mode:       config.ModeGit,
		DataDir:    dataDir,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, ".gitattributes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"*.png binary", "projects/*/state.md merge=lv24-state", "registry.json merge=lv24-registry", "projects/*/notes/*.md merge=lv24-notes"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf(".gitattributes misses %q:\n%s", want, raw)
		}
	}
	if got := strings.Count(string(raw), "journal/*.jsonl merge=union"); got != 1 {
		t.Fatalf("journal line appears %d times:\n%s", got, raw)
	}
}

func TestDoctorFixRegistersMergeDrivers(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))

	dataDir := t.TempDir()
	if err := exec.Command("git", "-C", dataDir, "init", "--quiet", "-b", "main").Run(); err != nil {
		t.Fatalf("git init: %v", err)
	}
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	provider, err := gitstore.Open(dataDir, gitstore.Options{})
	if err != nil {
		t.Fatalf("gitstore.Open: %v", err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = dataDir
	report, err := app.New(cfg, app.Dependencies{Store: store, MergeDrivers: provider}).Doctor(context.Background(), app.DoctorOptions{Fix: true})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	var check *app.DoctorCheck
	for i := range report.Checks {
		if report.Checks[i].Name == "merge-drivers" {
			check = &report.Checks[i]
			break
		}
	}
	if check == nil {
		t.Fatal("doctor has no merge-drivers check")
	}
	if check.Status != "ok" {
		t.Fatalf("merge-drivers check = %+v, want ok", *check)
	}
	missing, err := provider.MissingMergeDrivers(context.Background())
	if err != nil {
		t.Fatalf("MissingMergeDrivers: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("drivers still missing after --fix: %v", missing)
	}
}
