package sqliteindex

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func relationDocument(id, projectID string, relations ...ports.RelationLink) ports.Document {
	return ports.Document{
		ID: id, ProjectID: projectID, Kind: "decision", Title: "Decision " + id,
		Body: "body", UpdatedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC),
		Relations: relations,
	}
}

func TestIndexUpsertsAndListsRelations(t *testing.T) {
	index, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	ctx := context.Background()
	judgedAt := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	doc := relationDocument("demo/decision-a", "demo",
		ports.RelationLink{Target: "decision-b", Relation: "relates_to", Status: "accepted", Confidence: 0.9, Reason: "same design", JudgedAt: judgedAt, VerdictBy: "human"},
		ports.RelationLink{Target: "decision-c", Relation: "relates_to", Status: "pending", Confidence: 0.7, Reason: "similar text"},
	)
	if err := index.Upsert(ctx, doc); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	rows, err := index.Relations(ctx, ports.RelationQuery{ProjectID: "demo"})
	if err != nil {
		t.Fatalf("Relations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].SourceID != "decision-a" || rows[0].SourceKind != "decision" || rows[0].SourceTitle != "Decision demo/decision-a" {
		t.Fatalf("source annotation = %+v", rows[0])
	}
	if rows[0].JudgedAt.IsZero() || rows[0].VerdictBy != "human" {
		t.Fatalf("judgment metadata = %+v", rows[0])
	}
	pending, err := index.Relations(ctx, ports.RelationQuery{ProjectID: "demo", Statuses: []string{"pending"}})
	if err != nil || len(pending) != 1 || pending[0].TargetID != "decision-c" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	backlinks, err := index.Relations(ctx, ports.RelationQuery{ProjectID: "demo", TargetID: "decision-b"})
	if err != nil || len(backlinks) != 1 || backlinks[0].SourceID != "decision-a" {
		t.Fatalf("backlinks = %+v, %v", backlinks, err)
	}
	outgoing, err := index.Relations(ctx, ports.RelationQuery{ProjectID: "demo", SourceID: "decision-a"})
	if err != nil || len(outgoing) != 2 {
		t.Fatalf("outgoing = %+v, %v", outgoing, err)
	}
	limited, err := index.Relations(ctx, ports.RelationQuery{ProjectID: "demo", Limit: 1})
	if err != nil || len(limited) != 1 {
		t.Fatalf("limited = %+v, %v", limited, err)
	}
	verbs, err := index.Relations(ctx, ports.RelationQuery{ProjectID: "demo", Relations: []string{"conflicts_with"}})
	if err != nil || len(verbs) != 0 {
		t.Fatalf("verb filter = %+v, %v", verbs, err)
	}
}

func TestIndexUpsertReplacesRelations(t *testing.T) {
	index, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	ctx := context.Background()
	if err := index.Upsert(ctx, relationDocument("demo/decision-a", "demo",
		ports.RelationLink{Target: "decision-b", Relation: "relates_to", Status: "pending"},
		ports.RelationLink{Target: "decision-c", Relation: "relates_to", Status: "pending"},
	)); err != nil {
		t.Fatal(err)
	}
	if err := index.Upsert(ctx, relationDocument("demo/decision-a", "demo",
		ports.RelationLink{Target: "decision-b", Relation: "conflicts_with", Status: "accepted", Reason: "judged"},
	)); err != nil {
		t.Fatal(err)
	}
	rows, err := index.Relations(ctx, ports.RelationQuery{ProjectID: "demo"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if rows[0].Relation != "conflicts_with" || rows[0].Status != "accepted" {
		t.Fatalf("row = %+v", rows[0])
	}
}

func TestIndexDeleteRemovesRelations(t *testing.T) {
	index, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	ctx := context.Background()
	doc := relationDocument("demo/decision-a", "demo",
		ports.RelationLink{Target: "decision-b", Relation: "relates_to", Status: "accepted"})
	if err := index.Upsert(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := index.Delete(ctx, doc.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rows, err := index.Relations(ctx, ports.RelationQuery{ProjectID: "demo"})
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows after delete = %+v, %v", rows, err)
	}
}

func TestIndexRejectsInvalidRelations(t *testing.T) {
	index, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	cases := []ports.RelationLink{
		{Target: "", Relation: "relates_to", Status: "accepted"},
		{Target: "decision-b", Relation: "likes", Status: "accepted"},
		{Target: "decision-b", Relation: "relates_to", Status: "maybe"},
		{Target: "decision-b", Relation: "relates_to", Status: "accepted", Confidence: 2},
		{Target: "decision-b", Relation: "relates_to", Status: "accepted", VerdictBy: "model"},
	}
	for i, relation := range cases {
		doc := relationDocument("demo/decision-a", "demo", relation)
		if err := index.Upsert(context.Background(), doc); err == nil {
			t.Fatalf("case %d accepted: %+v", i, relation)
		}
	}
}

func TestRelationListRequiresAScope(t *testing.T) {
	index, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = index.Close() }()
	if _, err := index.Relations(context.Background(), ports.RelationQuery{SourceID: "decision-a"}); err == nil {
		t.Fatal("source without a project accepted")
	}
	if _, err := index.Relations(context.Background(), ports.RelationQuery{TargetID: "decision-a"}); err == nil {
		t.Fatal("target without a project accepted")
	}
}

func TestSchemaVersionBumpRebuildsTheRelationTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index", "brain.db")
	index, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := index.Upsert(ctx, relationDocument("demo/decision-a", "demo",
		ports.RelationLink{Target: "decision-b", Relation: "relates_to", Status: "accepted"})); err != nil {
		t.Fatal(err)
	}
	// Pretend the file was written by the previous schema version.
	if _, err := index.db.ExecContext(ctx, "PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if !reopened.NeedsReindex() {
		t.Fatal("the schema bump did not ask for a reindex")
	}
	rows, err := reopened.Relations(ctx, ports.RelationQuery{ProjectID: "demo"})
	if err != nil {
		t.Fatalf("Relations: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("old rows survived the rebuild: %+v", rows)
	}
	if err := reopened.Upsert(ctx, relationDocument("demo/decision-a", "demo",
		ports.RelationLink{Target: "decision-b", Relation: "relates_to", Status: "accepted"})); err != nil {
		t.Fatalf("Upsert after rebuild: %v", err)
	}
	rows, err = reopened.Relations(ctx, ports.RelationQuery{ProjectID: "demo"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows after rebuild = %+v, %v", rows, err)
	}
}
