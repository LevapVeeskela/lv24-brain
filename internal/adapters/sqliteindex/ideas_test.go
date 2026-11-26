package sqliteindex

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestSchemaV11AddsIdeaColumns pins the v11 shape: the idea scheduling and
// priority columns on documents, in addition to the v10 projection columns.
func TestSchemaV11AddsIdeaColumns(t *testing.T) {
	index := newTestIndex(t)
	ctx := context.Background()

	var version int
	if err := index.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 11 {
		t.Fatalf("user_version = %d, want 11", version)
	}
	columns := tableColumns(t, index, "documents")
	for name, want := range map[string]string{"priority": "TEXT", "due_at": "INTEGER", "due_date": "TEXT"} {
		if got := columns[name]; got != want {
			t.Fatalf("documents.%s = %q, want %q (columns %v)", name, got, want, columns)
		}
	}
	meta, err := index.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta[metaSchemaVersion] != "11" {
		t.Fatalf("meta schema_version = %q, want 11", meta[metaSchemaVersion])
	}
}

// TestSchemaUpgradeFromV10RebuildsOnce keeps the drop-and-rebuild behavior for
// an index written by 0.27: the new idea columns cannot be added in place, so
// the caller reindexes from Markdown.
func TestSchemaUpgradeFromV10RebuildsOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	index, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Upsert(ctx, testDocument("p/note-1", "p", "old row")); err != nil {
		t.Fatal(err)
	}
	if _, err := index.db.ExecContext(ctx, "PRAGMA user_version = 10"); err != nil {
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
		t.Fatal("v10 index did not ask for a reindex")
	}
	ids, err := reopened.IDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids after rebuild = %v, want none", ids)
	}
	meta, err := reopened.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta[metaSchemaVersion] != "11" {
		t.Fatalf("meta after rebuild = %v", meta)
	}
}

// TestIdeaProjectionRoundTrip stores the idea scheduling fields and returns
// them on the search row: status, priority and both due shapes.
func TestIdeaProjectionRoundTrip(t *testing.T) {
	index := newTestIndex(t)
	ctx := context.Background()
	due := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	timed := ports.Document{
		ID: "p/idea-2026-11-27-timed", ProjectID: "p", Kind: "idea",
		Title: "Dark calendar theme", Body: "contrast and keyboard",
		Status: "open", Priority: "high", DueAt: due, UpdatedAt: baseTime,
	}
	allDay := ports.Document{
		ID: "p/idea-2026-11-27-allday", ProjectID: "p", Kind: "idea",
		Title: "Calendar export", Body: "ics file",
		Status: "doing", Priority: "low", DueDate: "2026-11-28", UpdatedAt: baseTime,
	}
	mustUpsert(t, index, timed, allDay)

	hits, err := index.Search(ctx, ports.Query{Text: "calendar", Kinds: []string{"idea"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %+v, want both ideas", hits)
	}
	byID := map[string]ports.Hit{}
	for _, hit := range hits {
		byID[hit.ID] = hit
	}
	got, ok := byID["idea-2026-11-27-timed"]
	if !ok {
		t.Fatalf("timed idea missing from %+v", hits)
	}
	if got.Kind != "idea" || got.Status != "open" || got.Priority != "high" {
		t.Fatalf("timed row = %+v", got)
	}
	if !got.DueAt.Equal(due) || got.DueDate != "" {
		t.Fatalf("timed due = %v / %q, want %v and no date", got.DueAt, got.DueDate, due)
	}
	got, ok = byID["idea-2026-11-27-allday"]
	if !ok {
		t.Fatalf("all-day idea missing from %+v", hits)
	}
	if got.Status != "doing" || got.Priority != "low" || got.DueDate != "2026-11-28" || !got.DueAt.IsZero() {
		t.Fatalf("all-day row = %+v", got)
	}
}

// TestNonIdeaDocumentsKeepDefaultPriority keeps the schema default meaningful:
// records that have no priority are indexed as normal, so a priority filter can
// never drop them by accident.
func TestNonIdeaDocumentsKeepDefaultPriority(t *testing.T) {
	index := newTestIndex(t)
	ctx := context.Background()
	mustUpsert(t, index, testDocument("p/note-1", "p", "plain body"))
	var priority string
	if err := index.db.QueryRowContext(ctx, `SELECT priority FROM documents WHERE id = ?`, "p/note-1").Scan(&priority); err != nil {
		t.Fatal(err)
	}
	if priority != "normal" {
		t.Fatalf("priority = %q, want normal", priority)
	}
}
