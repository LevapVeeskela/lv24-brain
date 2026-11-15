package sqliteindex

import (
	"context"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func filterDocument(id, kind, body, subtype, scope, topic string, occurrences int) ports.Document {
	doc := testDocument(id, "lv24", body)
	doc.Kind = kind
	doc.Subtype = subtype
	doc.Scope = scope
	doc.TopicKey = topic
	doc.Occurrences = occurrences
	return doc
}

func TestSearchFiltersByClassification(t *testing.T) {
	index := newTestIndex(t)
	ctx := context.Background()
	docs := []ports.Document{
		filterDocument("lv24/note-arch", "note", "auth model retry", "architecture", "project", "architecture/auth", 2),
		filterDocument("lv24/note-bug", "note", "auth model retry", "bugfix", "personal", "bug/auth", 1),
		filterDocument("lv24/decision-auth", "decision", "auth model retry", "architecture", "global", "architecture/auth", 3),
	}
	for _, doc := range docs {
		if err := index.Upsert(ctx, doc); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	tests := []struct {
		name  string
		query ports.Query
		want  []string
	}{
		{
			name:  "default keeps every scope and subtype",
			query: ports.Query{Text: "auth", Limit: 10},
			want:  []string{"lv24/decision-auth", "lv24/note-arch", "lv24/note-bug"},
		},
		{
			name:  "subtype",
			query: ports.Query{Text: "auth", Limit: 10, Subtype: "architecture"},
			want:  []string{"lv24/decision-auth", "lv24/note-arch"},
		},
		{
			name:  "scope",
			query: ports.Query{Text: "auth", Limit: 10, Scope: "personal"},
			want:  []string{"lv24/note-bug"},
		},
		{
			name:  "topic",
			query: ports.Query{Text: "auth", Limit: 10, TopicKey: "architecture/auth"},
			want:  []string{"lv24/decision-auth", "lv24/note-arch"},
		},
		{
			name:  "combined",
			query: ports.Query{Text: "auth", Limit: 10, Subtype: "architecture", Scope: "project", TopicKey: "architecture/auth"},
			want:  []string{"lv24/note-arch"},
		},
		{
			name:  "unmatched scope",
			query: ports.Query{Text: "auth", Limit: 10, Scope: "global", Subtype: "bugfix"},
			want:  []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			hits, err := index.Search(ctx, test.query)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(hits) != len(test.want) {
				t.Fatalf("hits = %d (%v), want %d (%v)", len(hits), filterHitIDs(hits), len(test.want), test.want)
			}
			for i, want := range test.want {
				if hits[i].ID != recordIDForTest(want) {
					t.Fatalf("hit %d = %q, want %q (all: %v)", i, hits[i].ID, want, filterHitIDs(hits))
				}
			}
		})
	}
}

func TestHitCarriesClassification(t *testing.T) {
	index := newTestIndex(t)
	ctx := context.Background()
	doc := filterDocument("lv24/note-arch", "note", "auth model", "architecture", "personal", "architecture/auth", 4)
	if err := index.Upsert(ctx, doc); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	hits, err := index.Search(ctx, ports.Query{Text: "auth", Limit: 5})
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search = %d, %v", len(hits), err)
	}
	hit := hits[0]
	if hit.Subtype != "architecture" || hit.Scope != "personal" || hit.TopicKey != "architecture/auth" || hit.Occurrences != 4 {
		t.Fatalf("hit classification = %+v", hit)
	}
}

func TestIndexSchemaHasClassificationColumns(t *testing.T) {
	index := newTestIndex(t)
	rows, err := index.db.Query(`PRAGMA table_info(documents)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name       string
			kind       string
			notNull    int
			defaultV   any
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultV, &primaryKey); err != nil {
			t.Fatalf("scan: %v", err)
		}
		columns[name] = true
	}
	for _, want := range []string{"subtype", "scope", "topic_key", "occurrences"} {
		if !columns[want] {
			t.Fatalf("documents has no %q column: %v", want, columns)
		}
	}
}

func filterHitIDs(hits []ports.Hit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

// recordIDForTest mirrors the project-scoped key the index stores; Search
// returns the record id without the project scope.
func recordIDForTest(key string) string {
	return key[len("lv24/"):]
}
