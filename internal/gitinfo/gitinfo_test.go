package gitinfo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactRemote(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:tok@github.com/a/b.git": "https://github.com/a/b.git",
		"https://github.com/a/b.git":          "https://github.com/a/b.git",
		"git@github.com:a/b.git":              "git@github.com:a/b.git",
		"https://github.com/a/b@v1":           "https://github.com/a/b@v1",
		"https://tok@gitlab.example":          "https://gitlab.example",
		"":                                    "",
	} {
		if got := RedactRemote(in); got != want {
			t.Errorf("RedactRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadSummary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if got := Read(context.Background(), t.TempDir()); got.IsRepo {
		t.Fatalf("plain directory reported as repo: %+v", got)
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "--allow-empty", "-m", "first"},
		{"remote", "add", "origin", "https://u:secret@example.test/x.git"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := Read(context.Background(), repo)
	if !summary.IsRepo || summary.Branch != "main" || summary.Head == "" || summary.Dirty != 1 ||
		summary.Remote != "https://example.test/x.git" || summary.Subject != "first" || len(summary.Log) != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	context := summary.Context()
	if !strings.HasPrefix(context, "branch=main head="+summary.Head+" dirty=1 remote=https://example.test/x.git\n- ") || strings.Contains(context, "secret") {
		t.Fatalf("context = %q", context)
	}
}

func TestFindGraphs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, ".codegraph"),
		filepath.Join(root, "main", "graphify-out"),
		filepath.Join(root, "node_modules", "graphify-out"),
		filepath.Join(root, "a", "b", ".codegraph"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	graphs := FindGraphs([]string{root, "", filepath.Join(root, "missing")})
	if len(graphs) != 2 {
		t.Fatalf("graphs = %+v", graphs)
	}
	kinds := map[string]string{}
	for _, graph := range graphs {
		kinds[graph.Kind] = graph.Path
	}
	if kinds["codegraph"] != filepath.Join(root, ".codegraph") || kinds["graphify"] != filepath.Join(root, "main", "graphify-out") {
		t.Fatalf("graphs = %+v", graphs)
	}
}
