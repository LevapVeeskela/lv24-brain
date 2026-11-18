package sqliteindex

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestSchemaV10AddsProjectionColumnsAndMeta pins the v10 shape: the derived
// projection columns on documents and the meta table that carries the index
// generation, indexed_from and schema version.
func TestSchemaV10AddsProjectionColumnsAndMeta(t *testing.T) {
	index := newTestIndex(t)
	ctx := context.Background()

	var version int
	if err := index.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 10 {
		t.Fatalf("user_version = %d, want 10", version)
	}
	columns := tableColumns(t, index, "documents")
	for _, want := range []string{"content_hash", "stub"} {
		if _, ok := columns[want]; !ok {
			t.Fatalf("documents columns = %v, want %q", columns, want)
		}
	}
	if columns["content_hash"] != "TEXT" || columns["stub"] != "INTEGER" {
		t.Fatalf("projection column types = %q, %q", columns["content_hash"], columns["stub"])
	}
	meta, err := index.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta[metaSchemaVersion] != "10" {
		t.Fatalf("meta schema_version = %q, want 10", meta[metaSchemaVersion])
	}
}

// TestMetaRoundTrip keeps the metadata the reindex writes and reports the
// schema version from the same table.
func TestMetaRoundTrip(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	index, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.SetMeta(ctx, map[string]string{
		metaGeneration:  "generation-1",
		metaIndexedFrom: "abc1234+dirty",
	}); err != nil {
		t.Fatal(err)
	}
	if err := index.SetMeta(ctx, map[string]string{metaGeneration: "generation-2"}); err != nil {
		t.Fatal(err)
	}
	meta, err := index.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta[metaGeneration] != "generation-2" || meta[metaIndexedFrom] != "abc1234+dirty" || meta[metaSchemaVersion] != "10" {
		t.Fatalf("meta = %v", meta)
	}
	if err := index.SetMeta(ctx, map[string]string{"": "bad"}); err == nil {
		t.Fatal("empty meta key accepted")
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	meta, err = reopened.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta[metaGeneration] != "generation-2" || meta[metaIndexedFrom] != "abc1234+dirty" {
		t.Fatalf("meta after reopen = %v", meta)
	}
}

// TestProjectionColumnsStored verifies both upsert paths: the projected write
// stores the hash and stub verbatim, the plain write leaves them empty.
func TestProjectionColumnsStored(t *testing.T) {
	index := newTestIndex(t)
	ctx := context.Background()
	doc := testDocument("p/note-1", "p", "same body")
	doc.Title = "Deploy plan"
	if err := index.UpsertProjection(ctx, doc, "hash-1", true); err != nil {
		t.Fatal(err)
	}
	plain := testDocument("p/note-2", "p", "plain body")
	if err := index.Upsert(ctx, plain); err != nil {
		t.Fatal(err)
	}
	got := readProjection(t, index, doc.ID)
	if got.contentHash != "hash-1" || !got.stub {
		t.Fatalf("projected row = %+v", got)
	}
	if got := readProjection(t, index, plain.ID); got.contentHash != "" || got.stub {
		t.Fatalf("plain row = %+v, want empty projection", got)
	}
	if err := index.UpsertProjection(ctx, doc, "", false); err == nil {
		t.Fatal("empty content hash accepted")
	}

	batch := []ports.Document{
		testDocument("p/note-3", "p", "first"),
		testDocument("p/note-4", "p", "second"),
	}
	if err := index.UpsertProjectionBatch(ctx, batch, []string{"hash-3"}, []bool{false}); err == nil {
		t.Fatal("mismatched projection batch accepted")
	}
	if err := index.UpsertProjectionBatch(ctx, batch, []string{"hash-3", "hash-4"}, []bool{false, true}); err != nil {
		t.Fatal(err)
	}
	if got := readProjection(t, index, batch[1].ID); got.contentHash != "hash-4" || !got.stub {
		t.Fatalf("batch row = %+v", got)
	}
}

// TestSchemaUpgradeFromV9RebuildsOnce keeps the migration behavior of the
// earlier versions: a version mismatch drops the derived tables and asks the
// caller for a reindex.
func TestSchemaUpgradeFromV9RebuildsOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	index, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Upsert(ctx, testDocument("p/note-1", "p", "old row")); err != nil {
		t.Fatal(err)
	}
	if _, err := index.db.ExecContext(ctx, "PRAGMA user_version = 9"); err != nil {
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
		t.Fatal("v9 index did not ask for a reindex")
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
	if meta[metaSchemaVersion] != "10" {
		t.Fatalf("meta after rebuild = %v", meta)
	}
}

// tableColumns maps a table's column names to their declared types.
func tableColumns(t *testing.T, index *Index, table string) map[string]string {
	t.Helper()
	rows, err := index.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns := map[string]string{}
	for rows.Next() {
		var (
			cid     int
			name    string
			kind    string
			notNull int
			def     any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &kind, &notNull, &def, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = kind
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

type storedProjection struct {
	contentHash string
	stub        bool
}

func readProjection(t *testing.T, index *Index, id string) storedProjection {
	t.Helper()
	var (
		got  storedProjection
		stub int
	)
	if err := index.db.QueryRow(
		`SELECT content_hash, stub FROM documents WHERE id = ?`, id).Scan(&got.contentHash, &stub); err != nil {
		t.Fatal(err)
	}
	got.stub = stub == 1
	return got
}
