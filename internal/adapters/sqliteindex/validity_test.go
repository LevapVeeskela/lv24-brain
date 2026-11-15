package sqliteindex

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func openValidityIndex(t *testing.T) *Index {
	t.Helper()
	index, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	return index
}

func upsertValidityDoc(t *testing.T, index *Index, doc ports.Document) {
	t.Helper()
	if err := index.Upsert(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
}

func validityHitIDs(hits []ports.Hit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

func TestSearchValidityDefaultAndAsOf(t *testing.T) {
	index := openValidityIndex(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	upsertValidityDoc(t, index, ports.Document{
		ID: "p/decision-1", ProjectID: "p", Kind: "decision", Title: "Use JSON files",
		Body: "storage decision", UpdatedAt: base, ValidFrom: base,
		ValidTo: base.Add(48 * time.Hour), SupersededBy: "decision-2",
	})
	upsertValidityDoc(t, index, ports.Document{
		ID: "p/decision-2", ProjectID: "p", Kind: "decision", Title: "Use SQLite",
		Body: "storage decision", UpdatedAt: base.Add(48 * time.Hour),
		ValidFrom: base.Add(48 * time.Hour), Supersedes: "decision-1",
	})

	now := base.Add(72 * time.Hour)
	hits, err := index.Search(ctx, ports.Query{Text: "storage", ProjectID: "p", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if got := validityHitIDs(hits); len(got) != 1 || got[0] != "decision-2" {
		t.Fatalf("default hits = %v, want only the current generation", got)
	}

	assertIDs := func(name string, query ports.Query, want ...string) {
		t.Helper()
		hits, err := index.Search(ctx, query)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := validityHitIDs(hits)
		if len(got) != len(want) {
			t.Fatalf("%s: hits = %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: hits = %v, want %v", name, got, want)
			}
		}
	}
	assertIDs("before from", ports.Query{Text: "storage", ProjectID: "p", AsOf: base.Add(-time.Nanosecond)})
	assertIDs("at first from", ports.Query{Text: "storage", ProjectID: "p", AsOf: base}, "decision-1")
	assertIDs("just before second from", ports.Query{Text: "storage", ProjectID: "p", AsOf: base.Add(48*time.Hour - time.Nanosecond)}, "decision-1")
	assertIDs("at second from", ports.Query{Text: "storage", ProjectID: "p", AsOf: base.Add(48 * time.Hour)}, "decision-2")
	assertIDs("at first end", ports.Query{Text: "storage", ProjectID: "p", AsOf: base.Add(48 * time.Hour)}, "decision-2")
	assertIDs("include superseded", ports.Query{Text: "storage", ProjectID: "p", Now: now, IncludeSuperseded: true}, "decision-2", "decision-1")

	hits, err = index.Search(ctx, ports.Query{Text: "storage", ProjectID: "p", AsOf: base, Now: now})
	if err != nil || len(hits) != 1 {
		t.Fatalf("as-of hits = %+v, %v", hits, err)
	}
	if !hits[0].ValidFrom.Equal(base) || !hits[0].ValidTo.Equal(base.Add(48*time.Hour)) || hits[0].SupersededBy != "decision-2" {
		t.Fatalf("hit validity = %+v", hits[0])
	}
}

func TestSchemaVersionMismatchRebuildsValidityColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.db")
	index, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	upsertValidityDoc(t, index, ports.Document{
		ID: "p/note-1", ProjectID: "p", Kind: "note", Title: "t", Body: "b", UpdatedAt: time.Now(),
	})
	// Simulate an index written by the previous schema version.
	if _, err := index.db.ExecContext(context.Background(), "PRAGMA user_version = 2"); err != nil {
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
	if !reopened.NeedsReindex() {
		t.Fatal("old schema did not request a reindex")
	}
	ids, err := reopened.IDs(context.Background())
	if err != nil || len(ids) != 0 {
		t.Fatalf("rebuilt index kept rows: %v, %v", ids, err)
	}
	var columns int
	err = reopened.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM pragma_table_info('documents') WHERE name IN ('valid_from','valid_to','superseded_by','supersedes')`).Scan(&columns)
	if err != nil || columns != 4 {
		t.Fatalf("validity columns = %d, %v; want 4", columns, err)
	}
	for _, name := range []string{"documents_valid_idx", "documents_superseded_idx"} {
		var found int
		if err := reopened.db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&found); err != nil || found != 1 {
			t.Fatalf("index %s = %d, %v", name, found, err)
		}
	}
}

func TestValidityUpsertUpdatesExistingRow(t *testing.T) {
	index := openValidityIndex(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	doc := ports.Document{ID: "p/decision-1", ProjectID: "p", Kind: "decision", Title: "Use SQLite", Body: "storage", UpdatedAt: base, ValidFrom: base}
	upsertValidityDoc(t, index, doc)
	hits, err := index.Search(ctx, ports.Query{Text: "storage", ProjectID: "p", Now: base.Add(time.Hour)})
	if err != nil || len(hits) != 1 || !hits[0].ValidTo.IsZero() {
		t.Fatalf("initial hits = %+v, %v", hits, err)
	}
	doc.ValidTo = base.Add(2 * time.Hour)
	doc.SupersededBy = "decision-2"
	upsertValidityDoc(t, index, doc)
	hits, err = index.Search(ctx, ports.Query{Text: "storage", ProjectID: "p", Now: base.Add(3 * time.Hour)})
	if err != nil || len(hits) != 0 {
		t.Fatalf("superseded hits = %+v, %v; want none by default", hits, err)
	}
	hits, err = index.Search(ctx, ports.Query{Text: "storage", ProjectID: "p", Now: base.Add(3 * time.Hour), IncludeSuperseded: true})
	if err != nil || len(hits) != 1 || !hits[0].ValidTo.Equal(doc.ValidTo) || hits[0].SupersededBy != "decision-2" {
		t.Fatalf("all-time hits = %+v, %v", hits, err)
	}
}
