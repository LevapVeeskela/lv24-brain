package sqliteindex

import (
	"context"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// newProjectionIndex opens an in-memory index and writes every document with
// its derived projection columns, which a plain Upsert leaves empty.
func newProjectionIndex(t *testing.T, docs []ports.Document, hashes []string, stubs []bool) *Index {
	t.Helper()
	index, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	if err := index.UpsertProjectionBatch(context.Background(), docs, hashes, stubs); err != nil {
		t.Fatalf("UpsertProjectionBatch: %v", err)
	}
	return index
}

// IDX-4: a stub stays in the index and in counts, but search hides it until the
// caller asks for it.
func TestSearchHidesStubsByDefault(t *testing.T) {
	docs := []ports.Document{
		testDocument("proj/note-real", "proj", "needle facts"),
		testDocument("proj/note-stub", "proj", "needle stub"),
	}
	index := newProjectionIndex(t, docs, []string{"hash-real", "hash-stub"}, []bool{false, true})

	hits := mustSearch(t, index, ports.Query{Text: "needle"})
	if len(hits) != 1 || hits[0].ID != "note-real" {
		stub, ok := findHit(hits, "note-stub")
		t.Fatalf("default search = %v (stub present %v), want only the real note", rankIDs(hits), ok && stub.ID != "")
	}
	with := mustSearch(t, index, ports.Query{Text: "needle", IncludeStubs: true})
	if len(with) != 2 {
		t.Fatalf("IncludeStubs search = %d hits, want 2", len(with))
	}
	if _, ok := findHit(with, "note-stub"); !ok {
		t.Fatalf("IncludeStubs search = %v, want the stub note", rankIDs(with))
	}
}

// findHit returns the hit with id and whether it is in the page.
func findHit(hits []ports.Hit, id string) (ports.Hit, bool) {
	for _, hit := range hits {
		if hit.ID == id {
			return hit, true
		}
	}
	return ports.Hit{}, false
}

// TermFrequencies counts the documents of one project that contain a term, the
// input the support gate reads, and reports the project size.
func TestTermFrequenciesCountsProjectDocuments(t *testing.T) {
	docs := []ports.Document{
		testDocument("proj/note-one", "proj", "needle here"),
		testDocument("proj/note-two", "proj", "needle there"),
		testDocument("other/note-three", "other", "needle everywhere"),
	}
	index := newProjectionIndex(t, docs, []string{"h1", "h2", "h3"}, []bool{false, false, false})

	frequencies, total, err := index.TermFrequencies(context.Background(), "proj", []string{"needle", "absent"})
	if err != nil {
		t.Fatalf("TermFrequencies: %v", err)
	}
	if total != 2 {
		t.Fatalf("project total = %d, want 2", total)
	}
	if frequencies["needle"] != 2 || frequencies["absent"] != 0 {
		t.Fatalf("frequencies = %v, want needle 2 and absent 0", frequencies)
	}

	all, allTotal, err := index.TermFrequencies(context.Background(), "", []string{"needle"})
	if err != nil {
		t.Fatalf("TermFrequencies(all): %v", err)
	}
	if allTotal != 3 || all["needle"] != 3 {
		t.Fatalf("whole index = %v / %d, want needle 3 over 3 documents", all, allTotal)
	}
}

// ContentHash travels from the projection into the hit so search can collapse
// the same record written twice.
func TestSearchReturnsContentHash(t *testing.T) {
	docs := []ports.Document{testDocument("proj/note-one", "proj", "hash probe")}
	index := newProjectionIndex(t, docs, []string{"hash-one"}, []bool{false})
	hits := mustSearch(t, index, ports.Query{Text: "hash probe"})
	if len(hits) != 1 || hits[0].ContentHash != "hash-one" {
		t.Fatalf("hits = %+v, want the projected content hash", hits)
	}
}
