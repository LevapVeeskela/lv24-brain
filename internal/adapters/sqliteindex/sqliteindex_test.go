package sqliteindex

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var baseTime = time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC)

func newTestIndex(t *testing.T) *Index {
	t.Helper()
	index, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return index
}

func testDocument(id, projectID, body string) ports.Document {
	return ports.Document{
		ID:        id,
		ProjectID: projectID,
		Kind:      "note",
		Title:     "Title " + id,
		Body:      body,
		UpdatedAt: baseTime,
	}
}

func mustUpsert(t *testing.T, index *Index, docs ...ports.Document) {
	t.Helper()
	for _, doc := range docs {
		if err := index.Upsert(context.Background(), doc); err != nil {
			t.Fatalf("Upsert(%q): %v", doc.ID, err)
		}
	}
}

func mustSearch(t *testing.T, index *Index, query ports.Query) []ports.Hit {
	t.Helper()
	hits, err := index.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search(%q): %v", query.Text, err)
	}
	return hits
}

func TestOpenCreatesDirectoryAndSetsPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "index.db")
	index, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := index.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	var journalMode string
	if err := index.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var busy int
	if err := index.db.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if want := int(busyTimeout.Milliseconds()); busy != want {
		t.Fatalf("busy_timeout = %d, want %d", busy, want)
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open("   "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Open error = %v, want validation error", err)
	}
}

func TestUpsertAndSearch(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("doc-1", "brain", "SQLite FTS5 powers the local brain index"))

	hits := mustSearch(t, index, ports.Query{Text: "sqlite"})
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	hit := hits[0]
	if hit.ID != "doc-1" || hit.ProjectID != "brain" || hit.Kind != "note" {
		t.Fatalf("unexpected hit identity: %+v", hit)
	}
	if hit.Title != "Title doc-1" {
		t.Fatalf("Title = %q", hit.Title)
	}
	if !strings.Contains(strings.ToLower(hit.Snippet), "sqlite") {
		t.Fatalf("Snippet = %q, want the matched term", hit.Snippet)
	}
	if hit.Score <= 0 {
		t.Fatalf("Score = %v, want positive", hit.Score)
	}
}

func TestUpsertReplacesDocument(t *testing.T) {
	index := newTestIndex(t)
	replacement := testDocument("doc-1", "brain", "A completely rewritten body about graphs")
	replacement.Title = "Updated title"
	replacement.UpdatedAt = baseTime.Add(time.Hour)

	mustUpsert(t, index, testDocument("doc-1", "brain", "The original sqlite wording"))
	mustUpsert(t, index, replacement)

	if hits := mustSearch(t, index, ports.Query{Text: "sqlite"}); len(hits) != 0 {
		t.Fatalf("stale content still matches: %+v", hits)
	}
	hits := mustSearch(t, index, ports.Query{Text: "graphs"})
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if hits[0].Title != "Updated title" {
		t.Fatalf("Title = %q, want updated title", hits[0].Title)
	}
}

func TestDeleteRemovesDocument(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("doc-1", "brain", "ephemeral sqlite note"))

	if err := index.Delete(context.Background(), "doc-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if hits := mustSearch(t, index, ports.Query{Text: "sqlite"}); len(hits) != 0 {
		t.Fatalf("deleted document still matches: %+v", hits)
	}
	if err := index.Delete(context.Background(), "doc-1"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestSearchProjectFilter(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		testDocument("alpha-1", "alpha", "shared marker text"),
		testDocument("beta-1", "beta", "shared marker text"),
	)

	all := mustSearch(t, index, ports.Query{Text: "shared"})
	if len(all) != 2 {
		t.Fatalf("unfiltered hits = %d, want 2", len(all))
	}
	filtered := mustSearch(t, index, ports.Query{Text: "shared", ProjectID: "beta"})
	if len(filtered) != 1 {
		t.Fatalf("filtered hits = %d, want 1", len(filtered))
	}
	if filtered[0].ProjectID != "beta" || filtered[0].ID != "beta-1" {
		t.Fatalf("unexpected filtered hit: %+v", filtered[0])
	}
}

func kindDocument(id, projectID, kind string) ports.Document {
	doc := testDocument(id, projectID, "kindmarker shared text")
	doc.Kind = kind
	return doc
}

func hitIDs(hits []ports.Hit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

func TestSearchKindsFilter(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		kindDocument("note-1", "alpha", "note"),
		kindDocument("decision-1", "alpha", "decision"),
		kindDocument("dump-1", "alpha", "dump"),
	)

	hits := mustSearch(t, index, ports.Query{Text: "kindmarker", Kinds: []string{"note", "decision"}})
	if got := hitIDs(hits); len(got) != 2 || got[0] != "decision-1" || got[1] != "note-1" {
		t.Fatalf("Kinds hits = %v, want [decision-1 note-1]", got)
	}
	none := mustSearch(t, index, ports.Query{Text: "kindmarker", Kinds: []string{"bogus"}})
	if len(none) != 0 {
		t.Fatalf("unknown kind hits = %v, want none", hitIDs(none))
	}
}

func TestSearchExcludeKindsFilter(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		kindDocument("note-1", "alpha", "note"),
		kindDocument("dump-1", "alpha", "dump"),
	)

	hits := mustSearch(t, index, ports.Query{Text: "kindmarker", ExcludeKinds: []string{"dump"}})
	if got := hitIDs(hits); len(got) != 1 || got[0] != "note-1" {
		t.Fatalf("ExcludeKinds hits = %v, want [note-1]", got)
	}
}

func TestSearchKindsWithProjectFilter(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		kindDocument("alpha-note", "alpha", "note"),
		kindDocument("alpha-dump", "alpha", "dump"),
		kindDocument("beta-note", "beta", "note"),
		kindDocument("beta-dump", "beta", "dump"),
	)

	included := mustSearch(t, index, ports.Query{Text: "kindmarker", ProjectID: "beta", Kinds: []string{"dump"}})
	if got := hitIDs(included); len(got) != 1 || got[0] != "beta-dump" {
		t.Fatalf("Kinds+project hits = %v, want [beta-dump]", got)
	}
	excluded := mustSearch(t, index, ports.Query{
		Text:         "kindmarker",
		ProjectID:    "alpha",
		Kinds:        []string{"note", "dump"},
		ExcludeKinds: []string{"dump"},
	})
	if got := hitIDs(excluded); len(got) != 1 || got[0] != "alpha-note" {
		t.Fatalf("Kinds+Exclude+project hits = %v, want [alpha-note]", got)
	}
}

func TestSearchLimitDefaultsAndExplicitLimit(t *testing.T) {
	index := newTestIndex(t)
	docs := make([]ports.Document, 0, 25)
	for i := 0; i < 25; i++ {
		docs = append(docs, testDocument("doc-"+string(rune('a'+i)), "brain", "shared token body"))
	}
	mustUpsert(t, index, docs...)

	if hits := mustSearch(t, index, ports.Query{Text: "shared"}); len(hits) != DefaultLimit {
		t.Fatalf("default hits = %d, want %d", len(hits), DefaultLimit)
	}
	if hits := mustSearch(t, index, ports.Query{Text: "shared", Limit: 3}); len(hits) != 3 {
		t.Fatalf("limited hits = %d, want 3", len(hits))
	}
}

func TestSearchOrderingIsDeterministic(t *testing.T) {
	index := newTestIndex(t)
	newer := testDocument("doc-newer", "brain", "identical duplicate marker")
	newer.UpdatedAt = baseTime.Add(time.Hour)
	older := testDocument("doc-older", "brain", "identical duplicate marker")
	older.UpdatedAt = baseTime
	mustUpsert(t, index, older, newer)

	query := ports.Query{Text: "duplicate"}
	first := mustSearch(t, index, query)
	second := mustSearch(t, index, query)
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("hits = %d/%d, want 2/2", len(first), len(second))
	}
	if first[0].ID != "doc-newer" || first[1].ID != "doc-older" {
		t.Fatalf("order = %q, %q; want newest first", first[0].ID, first[1].ID)
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("repeat query order changed at %d: %q vs %q", i, first[i].ID, second[i].ID)
		}
	}
}

func TestSearchCyrillicAndASCII(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index,
		testDocument("ru-1", "brain", "Планирование архитектуры сервиса"),
		testDocument("en-1", "brain", "Planning the service architecture"),
	)

	if hits := mustSearch(t, index, ports.Query{Text: "АРХИТЕКТУРЫ"}); len(hits) != 1 || hits[0].ID != "ru-1" {
		t.Fatalf("cyrillic hits = %+v, want ru-1", hits)
	}
	if hits := mustSearch(t, index, ports.Query{Text: "architecture"}); len(hits) != 1 || hits[0].ID != "en-1" {
		t.Fatalf("ascii hits = %+v, want en-1", hits)
	}
}

func TestSearchEmptyQueryReturnsNoHits(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("doc-1", "brain", "some searchable content"))

	for _, query := range []string{"", "   ", "!!!", "«» — ..."} {
		hits := mustSearch(t, index, ports.Query{Text: query})
		if hits == nil {
			t.Fatalf("Search(%q) returned nil hits", query)
		}
		if len(hits) != 0 {
			t.Fatalf("Search(%q) hits = %d, want 0", query, len(hits))
		}
	}
}

func TestSearchSnippetMarksMatchAndTruncates(t *testing.T) {
	index := newTestIndex(t)
	body := strings.Repeat("filler ", 60) + "needle" + strings.Repeat(" filler", 60)
	mustUpsert(t, index, testDocument("doc-1", "brain", body))

	hits := mustSearch(t, index, ports.Query{Text: "needle"})
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if !strings.Contains(hits[0].Snippet, "[needle]") {
		t.Fatalf("Snippet = %q, want [needle] marker", hits[0].Snippet)
	}
	if len(hits[0].Snippet) >= len(body) {
		t.Fatalf("Snippet is not truncated: %d >= %d", len(hits[0].Snippet), len(body))
	}
}

func TestSearchTitleOnlyMatchProducesSnippet(t *testing.T) {
	index := newTestIndex(t)
	doc := testDocument("doc-1", "brain", "unrelated body words")
	doc.Title = "Deployment runbook"
	mustUpsert(t, index, doc)

	hits := mustSearch(t, index, ports.Query{Text: "deployment"})
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	if !strings.Contains(strings.ToLower(hits[0].Snippet), "[deployment]") {
		t.Fatalf("Snippet = %q, want title match marker", hits[0].Snippet)
	}
}

func TestSearchRequiresAllTerms(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("doc-1", "brain", "alpha beta gamma"))

	if hits := mustSearch(t, index, ports.Query{Text: "alpha beta"}); len(hits) != 1 {
		t.Fatalf("two-term hits = %d, want 1", len(hits))
	}
	if hits := mustSearch(t, index, ports.Query{Text: "missing"}); len(hits) != 0 {
		t.Fatalf("missing-term hits = %d, want 0", len(hits))
	}
}

func TestSearchTreatsOperatorsAsText(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("doc-1", "brain", "hello world"))

	for _, query := range []string{`hello" OR "world`, `" OR 1=1`, "*", "NEAR(hello world)"} {
		if _, err := index.Search(context.Background(), ports.Query{Text: query}); err != nil {
			t.Fatalf("Search(%q) error = %v, want none", query, err)
		}
	}
	for _, query := range []string{`" OR 1=1`, "*"} {
		if hits := mustSearch(t, index, ports.Query{Text: query}); len(hits) != 0 {
			t.Fatalf("Search(%q) hits = %d, want 0", query, len(hits))
		}
	}
}

func TestSearchContextCancellation(t *testing.T) {
	index := newTestIndex(t)
	mustUpsert(t, index, testDocument("doc-1", "brain", "cancellable content"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := index.Search(ctx, ports.Query{Text: "cancellable"}); err == nil {
		t.Fatal("Search with cancelled context returned nil error")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mustUpsert(t, first, testDocument("doc-1", "brain", "durable sqlite record"))
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	hits := mustSearch(t, second, ports.Query{Text: "durable"})
	if len(hits) != 1 || hits[0].ID != "doc-1" {
		t.Fatalf("hits = %+v, want doc-1", hits)
	}
}

func TestValidationErrors(t *testing.T) {
	index := newTestIndex(t)

	if err := index.Upsert(context.Background(), ports.Document{ProjectID: "brain"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Upsert without id error = %v, want validation", err)
	}
	if err := index.Upsert(context.Background(), ports.Document{ID: "doc-1"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Upsert without project error = %v, want validation", err)
	}
	if err := index.Delete(context.Background(), ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Delete without id error = %v, want validation", err)
	}
}
