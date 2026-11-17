package sqliteindex

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// benchDocs builds a corpus shaped like the real one: records with a relation
// each, so the batch path is measured with its relation writes.
func benchDocs(count int) []ports.Document {
	docs := make([]ports.Document, 0, count)
	for i := 0; i < count; i++ {
		doc := testDocument(fmt.Sprintf("proj/note-%05d", i), "proj",
			"benchmark body about sqlite reindex and context packing")
		doc.Relations = []ports.RelationLink{{Target: "dec-1", Relation: "relates_to", Status: "accepted", VerdictBy: "agent"}}
		docs = append(docs, doc)
	}
	return docs
}

func benchIndex(b *testing.B) *Index {
	b.Helper()
	index, err := Open(filepath.Join(b.TempDir(), "index.db"))
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = index.Close() })
	return index
}

// BenchmarkUpsertPerDocument measures the historical reindex shape: one
// transaction per document.
func BenchmarkUpsertPerDocument(b *testing.B) {
	docs := benchDocs(1000)
	index := benchIndex(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, doc := range docs {
			if err := index.Upsert(context.Background(), doc); err != nil {
				b.Fatalf("Upsert: %v", err)
			}
		}
	}
}

// BenchmarkUpsertBatch measures the batch reindex shape: one transaction and
// one prepared statement set per 1000 documents.
func BenchmarkUpsertBatch(b *testing.B) {
	docs := benchDocs(1000)
	index := benchIndex(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := index.UpsertBatch(context.Background(), docs); err != nil {
			b.Fatalf("UpsertBatch: %v", err)
		}
	}
}
