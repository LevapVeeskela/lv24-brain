package sqliteindex

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestFreshIndexNeedsReindexOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index", "brain.db")
	index, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !index.NeedsReindex() {
		t.Fatal("fresh index does not ask for a reindex")
	}
	if err := index.Upsert(context.Background(), ports.Document{ID: "p/note-1", ProjectID: "p", Kind: "note", Title: "t", Body: "b", UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.NeedsReindex() {
		t.Fatal("current index asks for a reindex on reopen")
	}
	ids, err := reopened.IDs(context.Background())
	if err != nil || len(ids) != 1 {
		t.Fatalf("documents kept across reopen = %v, %v", ids, err)
	}
}

func TestSearchReturnsRecordIDWithoutProjectScope(t *testing.T) {
	index, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	ctx := context.Background()
	for _, projectID := range []string{"alpha", "beta"} {
		doc := ports.Document{ID: projectID + "/note-1", ProjectID: projectID, Kind: "note", Title: "Shared", Body: "mango", UpdatedAt: time.Now()}
		if err := index.Upsert(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := index.Search(ctx, ports.Query{Text: "mango", ProjectID: "beta"})
	if err != nil || len(hits) != 1 || hits[0].ID != "note-1" || hits[0].ProjectID != "beta" {
		t.Fatalf("hits = %+v, %v; want note-1 in beta", hits, err)
	}
}
