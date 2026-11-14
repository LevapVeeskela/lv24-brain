package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// retiredDiscoverFixture initializes a brain and retires one temp checkout, so
// the tests carry no workspace-specific path.
func retiredDiscoverFixture(t *testing.T) (cfgPath, root, retired string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	root = filepath.Join(base, "workspace")
	dataDir := filepath.Join(base, "brain")
	cfgPath = filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	retired = filepath.Join(root, "lv24-old-checkout")
	discoverGitInit(t, filepath.Join(retired, "main"))
	discoverGitInit(t, filepath.Join(root, "kept"))

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.RetiredPaths = []config.RetiredPath{{Path: retired, ReplacedBy: "harvest"}}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return cfgPath, root, retired
}

func TestDiscoverSkipsRetiredCheckout(t *testing.T) {
	cfgPath, root, retired := retiredDiscoverFixture(t)

	var stdout, stderr bytes.Buffer
	if code := runDiscover([]string{"--root", root, "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("discover exit %d: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "retired lv24-old-checkout "+retired) {
		t.Fatalf("discover stdout = %q, want a retired line for %s", out, retired)
	}
	if strings.Contains(out, "new lv24-old-checkout") || strings.Contains(out, "registered lv24-old-checkout") {
		t.Fatalf("discover offered the retired checkout: %q", out)
	}
	if !strings.Contains(out, "new kept") {
		t.Fatalf("discover missed the active repository: %q", out)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runDiscover([]string{"--root", root, "--apply", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("discover --apply exit %d: %s", code, stderr.String())
	}
	var projects []domain.Project
	code, listed, listErr := runForTest(t, "project", "list", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project list: %s", listErr)
	}
	if err := json.Unmarshal([]byte(listed), &projects); err != nil {
		t.Fatalf("project list json: %v (%s)", err, listed)
	}
	for _, project := range projects {
		if project.ID == "lv24-old-checkout" {
			t.Fatalf("discover registered the retired checkout: %+v", project)
		}
	}
	if len(projects) != 1 || projects[0].ID != "kept" {
		t.Fatalf("projects = %+v, want only kept", projects)
	}
}

func TestProjectSetRefusesRetiredPath(t *testing.T) {
	cfgPath, _, retired := retiredDiscoverFixture(t)
	for _, subcommand := range []string{"set", "add"} {
		code, _, stderr := runForTest(t, "project", subcommand, "legacy-"+subcommand, "--path", filepath.Join(retired, "main"), "--config", cfgPath)
		if code == 0 {
			t.Fatalf("project %s accepted a retired path: %s", subcommand, stderr)
		}
		if !strings.Contains(stderr, "retired") || !strings.Contains(stderr, "harvest") {
			t.Fatalf("project %s stderr = %q, want a retirement error naming the replacement", subcommand, stderr)
		}
		if code, _, showErr := runForTest(t, "project", "show", "legacy-"+subcommand, "--config", cfgPath); code == 0 {
			t.Fatalf("refused project %s still registered the project: %s", subcommand, showErr)
		}
	}
}

func TestOpenWarnsWhenResolvingRetiredPath(t *testing.T) {
	cfgPath, _, retired := retiredDiscoverFixture(t)
	// Register the replacement through the CLI (a path outside the retired tree).
	if code, _, stderr := runForTest(t, "project", "set", "harvest", "--path", filepath.Join(t.TempDir(), "harvest"), "--config", cfgPath); code != 0 {
		t.Fatalf("project set harvest: %s", stderr)
	}
	code, stdout, stderr := runForTest(t, "open", filepath.Join(retired, "main"), "--config", cfgPath)
	if code != 0 {
		t.Fatalf("open retired path: %s", stderr)
	}
	if !strings.Contains(stderr, "warning:") || !strings.Contains(stderr, "retired") {
		t.Fatalf("open stderr = %q, want a retirement warning", stderr)
	}
	if !strings.Contains(stdout, "harvest") {
		t.Fatalf("open stdout = %q, want the replacement project", stdout)
	}
}
