package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestProjectionContentHashIsIdentityFree proves the hash depends on the text
// only: the same record indexed on two clones (different project, path and id)
// yields the same value, while a text change diverges.
func TestProjectionContentHashIsIdentityFree(t *testing.T) {
	left := ports.Document{
		ID:        "alpha/note-2026-11-18-deploy-01j2x3y4z5a6b7c8d9e0f1g2h3",
		ProjectID: "alpha",
		Kind:      "note",
		Title:     "Deploy  Plan",
		Body:      "Ship   the\nsearch index",
	}
	right := ports.Document{
		ID:        "beta/note-2026-11-18-other-01j2x3y4z5a6b7c8d9e0f1g2h3",
		ProjectID: "beta",
		Kind:      "note",
		Title:     "deploy plan",
		Body:      "ship the search index",
	}
	leftHash := projectDocument(left).ContentHash
	rightHash := projectDocument(right).ContentHash
	if leftHash != rightHash {
		t.Fatalf("same content hashed differently: %q != %q", leftHash, rightHash)
	}
	if len(leftHash) != 64 {
		t.Fatalf("content hash = %q, want a sha256 hex digest", leftHash)
	}
	edited := right
	edited.Body = "ship the search index tonight"
	if projectDocument(edited).ContentHash == leftHash {
		t.Fatal("edited body kept the hash")
	}
	rekinded := right
	rekinded.Kind = "decision"
	if projectDocument(rekinded).ContentHash == leftHash {
		t.Fatal("different kind kept the hash")
	}
}

// TestProjectionStub pins the derived stub flag: generated label+id titles and
// "nothing happened" bodies are stubs, real notes are not.
func TestProjectionStub(t *testing.T) {
	cases := []struct {
		name  string
		title string
		body  string
		want  bool
	}{
		{"generated label and id", "note-2026-11-18-deploy-index-01j2x3y4z5a6b7c8d9e0f1g2h3", "", true},
		{"generated decision label", "decision-2026-11-18-pick-storage", "", true},
		{"nothing new", "Deploy plan", "Nothing new.", true},
		{"session ended", "Deploy plan", "SESSION ENDED", true},
		{"no changes", "Deploy plan", "no changes\n", true},
		{"real note", "Index v10", "Added content_hash and stub columns to the projection.", false},
		{"real note that mentions changes", "Index v10", "no changes to the schema, only a rename", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := projectDocument(ports.Document{
				Kind:  "note",
				Title: testCase.title,
				Body:  testCase.body,
			}).Stub
			if got != testCase.want {
				t.Fatalf("stub = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestReindexStoresIndexMetaAndStatus proves a successful reindex writes the
// generation and the git marker, the status agrees, and a new commit without a
// reindex turns the index stale.
func TestReindexStoresIndexMetaAndStatus(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	gitTestRun(t, dataDir, "init", "--quiet", "-b", "main")
	gitTestWrite(t, dataDir, "README.md", "data\n")
	gitTestRun(t, dataDir, "add", ".")
	gitTestRun(t, dataDir, "commit", "--quiet", "-m", "initial")
	head := gitTestRun(t, dataDir, "rev-parse", "--short", "HEAD")

	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutProject(ctx, domain.Project{ID: "p", Name: "P"}); err != nil {
		t.Fatal(err)
	}
	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()

	application := New(
		config.BrainConfig{Mode: config.ModeLocal, DataDir: dataDir},
		Dependencies{Store: store, Index: index},
	)
	if _, err := application.Reindex(ctx, IndexOptions{}); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	meta, err := index.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	generation := meta[indexMetaGeneration]
	if len(generation) != 16 {
		t.Fatalf("index_generation = %q, want a 16-character digest", generation)
	}
	indexedFrom := meta[indexMetaIndexedFrom]
	if !strings.HasPrefix(indexedFrom, head) {
		t.Fatalf("indexed_from = %q, want it to start with HEAD %q", indexedFrom, head)
	}

	status, err := application.IndexStatus(ctx)
	if err != nil {
		t.Fatalf("IndexStatus: %v", err)
	}
	if status.Stale {
		t.Fatalf("fresh reindex reported stale: %+v", status)
	}
	if status.Head != head || status.IndexedFrom != indexedFrom || status.Generation != generation {
		t.Fatalf("status = %+v, want head %q and marker %q", status, head, indexedFrom)
	}
	if status.SchemaVersion != "10" {
		t.Fatalf("schema version = %q, want 10", status.SchemaVersion)
	}

	gitTestWrite(t, dataDir, "README.md", "data v2\n")
	gitTestRun(t, dataDir, "add", ".")
	gitTestRun(t, dataDir, "commit", "--quiet", "-m", "change without reindex")
	status, err = application.IndexStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stale {
		t.Fatalf("status after a commit = %+v, want stale", status)
	}
	if status.Head == status.IndexedFrom || status.Head == head {
		t.Fatalf("status head = %q, indexed_from = %q; want the new HEAD", status.Head, status.IndexedFrom)
	}
}

func gitTestRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{
		"-C", dir,
		"-c", "user.name=test",
		"-c", "user.email=test@example.invalid",
	}, args...)...)
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitTestWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
