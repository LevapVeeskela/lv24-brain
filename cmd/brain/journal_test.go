package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestJournalAndEventCommitCLI(t *testing.T) {
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
	if code, _, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "demo", "--title", "Journaled", "--body", "x"); code != 0 {
		t.Fatalf("note: %s", stderr)
	}

	repo := filepath.Join(root, "demo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "first commit"},
		{"remote", "add", "origin", "https://user:secret-token@example.test/acme/demo.git"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}

	// The project is resolved from the repository directory name.
	code, stdout, stderr := runForTest(t, "event", "commit", "--config", cfgPath, "--repo", repo)
	if code != 0 {
		t.Fatalf("event commit exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "commit  demo") || !strings.Contains(stdout, "first commit") {
		t.Fatalf("event commit stdout = %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "journal", "--config", cfgPath, "--project", "demo")
	if code != 0 {
		t.Fatalf("journal exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "commit") || !strings.Contains(lines[1], "note  demo  Journaled") {
		t.Fatalf("journal stdout = %q", stdout)
	}
	files, _ := filepath.Glob(filepath.Join(dataDir, "journal", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("journal files = %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), "secret-token") {
		t.Fatalf("remote credentials leaked into the journal: %s", raw)
	}

	if code, _, _ := runForTest(t, "event", "commit", "--config", cfgPath, "--repo", root); code == 0 {
		t.Fatal("event commit outside a git repository succeeded")
	}
	if code, _, _ := runForTest(t, "event", "push"); code != 2 {
		t.Fatal("unknown event kind accepted")
	}
}

func TestRedactRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:tok@github.com/a/b.git": "https://github.com/a/b.git",
		"https://github.com/a/b.git":          "https://github.com/a/b.git",
		"git@github.com:a/b.git":              "git@github.com:a/b.git",
		"https://github.com/a/b@v1":           "https://github.com/a/b@v1",
		"":                                    "",
	} {
		if got := redactRemote(in); got != want {
			t.Errorf("redactRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProjectForRepo(t *testing.T) {
	root := t.TempDir()
	projects := []domain.Project{
		{ID: "lv24", Paths: []string{filepath.Join(root, "city")}},
		{ID: "demo"},
	}
	if got := projectForRepo(projects, filepath.Join(root, "city")); got != "lv24" {
		t.Fatalf("by path = %q", got)
	}
	if got := projectForRepo(projects, filepath.Join(root, "Demo")); got != "demo" {
		t.Fatalf("by name = %q", got)
	}
	if got := projectForRepo(projects, filepath.Join(root, "other")); got != "" {
		t.Fatalf("unknown = %q", got)
	}
}
