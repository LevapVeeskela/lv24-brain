package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestDiscoverUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runDiscover(nil, &stdout, &stderr); code != 2 {
		t.Fatalf("missing --root: exit %d, want 2 (stderr %q)", code, stderr.String())
	}
	stderr.Reset()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if code := runDiscover([]string{"--root", missing}, &stdout, &stderr); code != 2 {
		t.Fatalf("missing directory: exit %d, want 2 (stderr %q)", code, stderr.String())
	}
	stderr.Reset()
	if code := runDiscover([]string{"--root", t.TempDir(), "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unexpected argument: exit %d, want 2", code)
	}
}

func TestDiscoverSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Product":        "product",
		"My App_v2.0":    "my-app-v2-0",
		"  weird  name ": "weird-name",
		"a--b":           "a-b",
		"ünï":            "n",
		"---":            "",
	} {
		if got := discoverSlug(in); got != want {
			t.Errorf("discoverSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func discoverGitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func discoverGitInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	discoverGitRun(t, dir, "init", "-q", "-b", "main")
}

func TestDiscoverRegistersOnlyNewRepositories(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	dataDir := filepath.Join(base, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}

	// repoA: a plain repository already registered under another id.
	repoA := filepath.Join(root, "repoA")
	discoverGitInit(t, repoA)
	// product/main: repository one level down; remote carries credentials.
	productRepo := filepath.Join(root, "product", "main")
	discoverGitInit(t, productRepo)
	discoverGitRun(t, productRepo, "remote", "add", "origin", "https://u:tok@example.test/x.git")
	discoverGitRun(t, productRepo, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "first")
	// origin/HEAD points at trunk, which differs from the checked-out branch.
	discoverGitRun(t, productRepo, "update-ref", "refs/remotes/origin/trunk", "HEAD")
	discoverGitRun(t, productRepo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	// wt: worktree-like directory whose .git is a file.
	wt := filepath.Join(root, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: ../elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Skipped: plain folder, hidden, and default-excluded directories.
	if err := os.MkdirAll(filepath.Join(root, "plainFolder"), 0o755); err != nil {
		t.Fatal(err)
	}
	discoverGitInit(t, filepath.Join(root, ".hidden"))
	discoverGitInit(t, filepath.Join(root, "node_modules"))

	// Registry: repoA is known by path; "wt" id is taken by another path.
	if code, _, stderr := runForTest(t, "project", "set", "legacy-repo-a", "--path", repoA, "--config", cfgPath); code != 0 {
		t.Fatalf("project set legacy: %s", stderr)
	}
	elsewhere := filepath.Join(base, "elsewhere")
	if code, _, stderr := runForTest(t, "project", "set", "wt", "--path", elsewhere, "--config", cfgPath); code != 0 {
		t.Fatalf("project set wt: %s", stderr)
	}

	listProjects := func() []domain.Project {
		t.Helper()
		code, stdout, stderr := runForTest(t, "project", "list", "--json", "--config", cfgPath)
		if code != 0 {
			t.Fatalf("project list: %s", stderr)
		}
		var projects []domain.Project
		if err := json.Unmarshal([]byte(stdout), &projects); err != nil {
			t.Fatalf("project list json: %v (%s)", err, stdout)
		}
		return projects
	}

	// Dry run: reports and changes nothing.
	var stdout, stderr bytes.Buffer
	if code := runDiscover([]string{"--root", root, "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("dry run exit %d: %s", code, stderr.String())
	}
	wantDry := strings.Join([]string{
		"new product " + productRepo,
		"known repoa " + repoA,
		"id-conflict wt " + wt + " (id used by another path)",
		"dry run: 1 new project(s); rerun with --apply to register",
		"",
	}, "\n")
	if stdout.String() != wantDry {
		t.Fatalf("dry run stdout = %q, want %q", stdout.String(), wantDry)
	}
	if n := len(listProjects()); n != 2 {
		t.Fatalf("dry run changed registry: %d projects, want 2", n)
	}

	// Apply: registers only product.
	stdout.Reset()
	stderr.Reset()
	if code := runDiscover([]string{"--root", root, "--apply", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("apply exit %d: %s", code, stderr.String())
	}
	wantApply := strings.Join([]string{
		"registered product",
		"known repoa " + repoA,
		"id-conflict wt " + wt + " (id used by another path)",
		"",
	}, "\n")
	if stdout.String() != wantApply {
		t.Fatalf("apply stdout = %q, want %q", stdout.String(), wantApply)
	}
	code, shown, stderrText := runForTest(t, "project", "show", "product", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project show product: %s", stderrText)
	}
	if strings.Contains(shown, "tok") {
		t.Fatalf("stored project leaks credentials: %s", shown)
	}
	var product domain.Project
	if err := json.Unmarshal([]byte(shown), &product); err != nil {
		t.Fatalf("project show json: %v", err)
	}
	if product.Name != "product" || product.Kind != domain.ProjectKindRepository ||
		product.RepoURL != "https://example.test/x.git" || product.DefaultBranch != "trunk" ||
		len(product.Paths) != 1 || product.Paths[0] != productRepo {
		t.Fatalf("product = %+v", product)
	}
	if n := len(listProjects()); n != 3 {
		t.Fatalf("apply registry has %d projects, want 3", n)
	}
	for _, p := range listProjects() {
		if p.ID == "wt" && len(p.Paths) == 1 && p.Paths[0] != elsewhere {
			t.Fatalf("existing wt project was modified: %+v", p)
		}
	}

	// Second apply: nothing new, nothing registered.
	stdout.Reset()
	stderr.Reset()
	if code := runDiscover([]string{"--root", root, "--apply", "--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("second apply exit %d: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "registered") || strings.Contains(stdout.String(), "new ") {
		t.Fatalf("second apply registered again: %q", stdout.String())
	}
	if n := len(listProjects()); n != 3 {
		t.Fatalf("second apply changed registry: %d projects, want 3", n)
	}
}
