package sqliteindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// threeDocFixture is the small corpus used to prove that the batch path writes
// exactly what the per-document path writes.
func threeDocFixture() []ports.Document {
	first := testDocument("proj/dec-1", "proj", "batch reindex decision body")
	first.Kind = "decision"
	first.Status = "accepted"
	first.Pinned = true
	first.ValidTo = baseTime.Add(48 * time.Hour)
	first.SupersededBy = "proj/dec-2"
	first.Confidence = 0.8
	first.Subtype = "architecture"
	first.Scope = "project"
	first.TopicKey = "reindex/batch"
	first.Occurrences = 3
	first.Relations = []ports.RelationLink{
		{Target: "dec-2", Relation: "supersedes", Status: "accepted", Confidence: 1, Reason: "merged", VerdictBy: "human"},
		{Target: "note-1", Relation: "relates_to", Status: "pending", VerdictBy: "agent"},
	}
	second := testDocument("proj/dec-2", "proj", "batch reindex replacement body")
	second.Kind = "decision"
	second.Status = "proposed"
	second.Supersedes = "proj/dec-1"
	third := testDocument("proj/note-1", "proj", "batch reindex note body")
	third.Relations = []ports.RelationLink{{Target: "dec-1", Relation: "relates_to", Status: "accepted", VerdictBy: "human"}}
	return []ports.Document{first, second, third}
}

// indexSnapshot is the full derived content of an index: documents, relations
// and a search result, so two indexes can be compared as data.
type indexSnapshot struct {
	documents [][]string
	relations [][]string
	hits      []string
}

func snapshotIndex(t *testing.T, index *Index) indexSnapshot {
	t.Helper()
	ctx := context.Background()
	var snapshot indexSnapshot
	rows, err := index.db.QueryContext(ctx, `SELECT id, project_id, kind, title, body, updated_at, valid_from,
		COALESCE(valid_to, -1), COALESCE(superseded_by, ''), COALESCE(supersedes, ''), status, confidence,
		subtype, scope, topic_key, occurrences, pinned FROM documents ORDER BY id`)
	if err != nil {
		t.Fatalf("list documents: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id, projectID, kind, title, body string
			supersededBy, supersedes, status string
			subtype, scope, topicKey         string
			updatedAt, validFrom, validTo    int64
			confidence                       float64
			occurrences, pinned              int
		)
		if err := rows.Scan(&id, &projectID, &kind, &title, &body, &updatedAt, &validFrom, &validTo,
			&supersededBy, &supersedes, &status, &confidence, &subtype, &scope, &topicKey,
			&occurrences, &pinned); err != nil {
			t.Fatalf("scan document: %v", err)
		}
		snapshot.documents = append(snapshot.documents, []string{
			id, projectID, kind, title, body,
			fmt.Sprint(updatedAt, validFrom, validTo, supersededBy, supersedes, status, confidence,
				subtype, scope, topicKey, occurrences, pinned),
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read documents: %v", err)
	}
	relationRows, err := index.db.QueryContext(ctx, `SELECT source_id, project_id, target_id, relation, status,
		confidence, reason, COALESCE(judged_at, -1), verdict_by FROM relations ORDER BY source_id, target_id, relation`)
	if err != nil {
		t.Fatalf("list relations: %v", err)
	}
	defer func() { _ = relationRows.Close() }()
	for relationRows.Next() {
		var (
			sourceID, projectID, targetID, relation string
			status, reason, verdictBy               string
			confidence                              float64
			judgedAt                                int64
		)
		if err := relationRows.Scan(&sourceID, &projectID, &targetID, &relation, &status, &confidence,
			&reason, &judgedAt, &verdictBy); err != nil {
			t.Fatalf("scan relation: %v", err)
		}
		snapshot.relations = append(snapshot.relations, []string{
			sourceID, projectID, targetID, relation, status,
			fmt.Sprint(confidence, reason, judgedAt, verdictBy),
		})
	}
	if err := relationRows.Err(); err != nil {
		t.Fatalf("read relations: %v", err)
	}
	hits, err := index.Search(ctx, ports.Query{Text: "batch", ProjectID: "proj", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, hit := range hits {
		snapshot.hits = append(snapshot.hits, fmt.Sprintf("%s/%s/%.6f", hit.Kind, hit.ID, hit.Score))
	}
	return snapshot
}

func TestUpsertBatchMatchesPerDocumentIndex(t *testing.T) {
	// Arrange
	ctx := context.Background()
	docs := threeDocFixture()
	perDocument := newTestIndex(t)
	batch := newTestIndex(t)

	// Act
	for _, doc := range docs {
		if err := perDocument.Upsert(ctx, doc); err != nil {
			t.Fatalf("Upsert(%q): %v", doc.ID, err)
		}
	}
	if err := batch.UpsertBatch(ctx, docs); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}

	// Assert
	want, got := snapshotIndex(t, perDocument), snapshotIndex(t, batch)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batch index differs from the per-document index:\n batch=%+v\n want=%+v", got, want)
	}
	if len(got.documents) != len(docs) || len(got.relations) != 3 {
		t.Fatalf("indexed %d documents and %d relations, want %d and 3", len(got.documents), len(got.relations), len(docs))
	}
}

func TestUpsertBatchReplacesRelationsAndRows(t *testing.T) {
	// Arrange
	ctx := context.Background()
	index := newTestIndex(t)
	docs := threeDocFixture()
	if err := index.UpsertBatch(ctx, docs); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	// A reindex writes the whole corpus again: the second run must replace the
	// relation set instead of accumulating rows.
	changed := threeDocFixture()
	changed[0].Title = "changed title"
	changed[0].Relations = nil

	// Act
	if err := index.UpsertBatch(ctx, changed); err != nil {
		t.Fatalf("second UpsertBatch: %v", err)
	}

	// Assert
	snapshot := snapshotIndex(t, index)
	if len(snapshot.documents) != 3 {
		t.Fatalf("documents = %d, want the 3 upserted ones", len(snapshot.documents))
	}
	if len(snapshot.relations) != 1 {
		t.Fatalf("relations = %v, want only the note relation left", snapshot.relations)
	}
	if snapshot.documents[0][0] == "proj/dec-1" && snapshot.documents[0][3] != "changed title" {
		t.Fatalf("title = %q, want the second run to win", snapshot.documents[0][3])
	}
}

func TestUpsertBatchValidatesBeforeWriting(t *testing.T) {
	// Arrange
	ctx := context.Background()
	index := newTestIndex(t)
	docs := threeDocFixture()
	bad := testDocument("", "proj", "no id")
	docs = append(docs, bad)

	// Act
	err := index.UpsertBatch(ctx, docs)

	// Assert
	if err == nil {
		t.Fatal("UpsertBatch accepted a document without an id")
	}
	if ids, listErr := index.IDs(ctx); listErr != nil || len(ids) != 0 {
		t.Fatalf("ids = %v (err %v), want an empty index after the rejected batch", ids, listErr)
	}
}

func TestUpsertBatchEmptyWritesNothing(t *testing.T) {
	index := newTestIndex(t)
	if err := index.UpsertBatch(context.Background(), nil); err != nil {
		t.Fatalf("UpsertBatch(nil): %v", err)
	}
	if ids, err := index.IDs(context.Background()); err != nil || len(ids) != 0 {
		t.Fatalf("ids = %v (err %v), want empty", ids, err)
	}
}

// TestOptimizeTruncatesWAL proves the reindex compaction runs: a large batch
// writes a WAL, and the checkpoint that follows truncates it to zero bytes.
func TestOptimizeTruncatesWAL(t *testing.T) {
	// Arrange
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	index, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = index.Close() }()
	body := strings.Repeat("reindex body text ", 200)
	docs := make([]ports.Document, 0, 400)
	for i := 0; i < 400; i++ {
		docs = append(docs, testDocument(fmt.Sprintf("proj/note-%03d", i), "proj", body))
	}
	if err := index.UpsertBatch(ctx, docs); err != nil {
		t.Fatalf("UpsertBatch: %v", err)
	}
	walPath := path + "-wal"
	before, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal: %v", err)
	}
	if before.Size() == 0 {
		t.Fatalf("wal is empty before the checkpoint, nothing to prove")
	}

	// Act
	if err := index.Optimize(ctx); err != nil {
		t.Fatalf("Optimize: %v", err)
	}

	// Assert
	after, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat wal after checkpoint: %v", err)
	}
	if after.Size() != 0 {
		t.Fatalf("wal = %d bytes after the checkpoint, want 0 (was %d)", after.Size(), before.Size())
	}
	// The optimize must not drop or change the documents.
	if len(snapshotIDs(t, index)) != len(docs) {
		t.Fatalf("documents = %d after optimize, want %d", len(snapshotIDs(t, index)), len(docs))
	}
	if hits, err := index.Search(ctx, ports.Query{Text: "reindex", ProjectID: "proj", Limit: 5}); err != nil || len(hits) == 0 {
		t.Fatalf("search after optimize = %d hits (err %v), want matches", len(hits), err)
	}
}

// TestOptimizeWorksOnMemoryIndex keeps the local test path usable: an
// in-memory index has no WAL file and must still accept the compaction.
func TestOptimizeWorksOnMemoryIndex(t *testing.T) {
	index, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = index.Close() }()
	mustUpsert(t, index, threeDocFixture()...)
	if err := index.Optimize(context.Background()); err != nil {
		t.Fatalf("Optimize on memory index: %v", err)
	}
}

func snapshotIDs(t *testing.T, index *Index) []string {
	t.Helper()
	ids, err := index.IDs(context.Background())
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	return ids
}
