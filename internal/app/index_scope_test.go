package app_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	_ "modernc.org/sqlite"
)

const sharedNoteID = "note-2026-01-01-setup"

// registerScopeProjects creates projects p1 and p2 in the data directory.
func registerScopeProjects(t *testing.T, dataDir string) {
	t.Helper()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"p1", "p2"} {
		project := domain.Project{ID: id, Name: id, Kind: domain.ProjectKindRepository, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
		if err := store.PutProject(context.Background(), project); err != nil {
			t.Fatal(err)
		}
	}
}

// newScopeFixture opens a local app with a real Markdown store and SQLite
// index under one data directory.
func newScopeFixture(t *testing.T, dataDir string) (*app.App, *sqliteindex.Index) {
	t.Helper()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	return app.New(cfg, app.Dependencies{Store: store, Index: index}), index
}

func appendSharedNote(t *testing.T, dataDir, projectID, body string) {
	t.Helper()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	note := domain.Note{ID: sharedNoteID, ProjectID: projectID, Title: "Setup", Body: body, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if _, err := store.AppendNote(context.Background(), note); err != nil {
		t.Fatalf("AppendNote %s: %v", projectID, err)
	}
}

func TestSameRecordIDInTwoProjectsStaysSeparateInIndex(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	a, _ := newScopeFixture(t, dataDir)
	registerScopeProjects(t, dataDir)
	appendSharedNote(t, dataDir, "p1", "shared kiwi setup")
	appendSharedNote(t, dataDir, "p2", "shared kiwi setup")
	if _, err := a.Reindex(ctx, app.IndexOptions{}); err != nil {
		t.Fatal(err)
	}

	hits, err := a.Search(ctx, "kiwi", "", 10)
	if err != nil || len(hits) != 2 {
		t.Fatalf("search = %+v, %v; want both projects", hits, err)
	}
	for _, hit := range hits {
		if hit.ID != sharedNoteID {
			t.Fatalf("hit id = %q, want plain record id %q", hit.ID, sharedNoteID)
		}
	}
	if hits[0].ProjectID == hits[1].ProjectID {
		t.Fatalf("both hits attributed to %q", hits[0].ProjectID)
	}

	if _, err := a.TrashRecord(ctx, "p1", sharedNoteID); err != nil {
		t.Fatalf("TrashRecord p1: %v", err)
	}
	hits, err = a.Search(ctx, "kiwi", "", 10)
	if err != nil || len(hits) != 1 || hits[0].ProjectID != "p2" || hits[0].ID != sharedNoteID {
		t.Fatalf("after trashing p1: search = %+v, %v; want only p2", hits, err)
	}
}

func TestIndexWithLegacyIDsIsRebuiltAfterSchemaUpgrade(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	indexPath := filepath.Join(dataDir, "index", "brain.db")
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeLegacyIndex(t, indexPath)
	registerScopeProjects(t, dataDir)
	appendSharedNote(t, dataDir, "p1", "fresh kiwi setup")

	// Opening the legacy file recreates it empty and reports the reset; the
	// bootstrap then marks the index dirty so the next search refills it.
	reopened, err := sqliteindex.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.NeedsReindex() {
		t.Fatal("legacy index not reported for reindex")
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := app.MarkIndexDirty(dataDir); err != nil {
		t.Fatal(err)
	}

	a, index := newScopeFixture(t, dataDir)
	if index.NeedsReindex() {
		t.Fatal("current-version index reported for reindex on second open")
	}
	hits, err := a.Search(ctx, "kiwi", "", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != sharedNoteID || hits[0].ProjectID != "p1" {
		t.Fatalf("search after upgrade = %+v, %v; want the rebuilt p1 note", hits, err)
	}
	if stale, err := a.Search(ctx, "ghost", "", 10); err != nil || len(stale) != 0 {
		t.Fatalf("stale legacy document survived: %+v, %v", stale, err)
	}
}

// writeLegacyIndex creates an index in the pre-scoping layout: unversioned,
// with bare record ids as primary keys.
func writeLegacyIndex(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	statements := []string{
		`CREATE TABLE documents (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, kind TEXT NOT NULL,
			title TEXT NOT NULL, body TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO documents VALUES ('` + sharedNoteID + `', 'p2', 'note', 'Setup', 'stale ghost', 1)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}
}

var _ ports.Index = (*sqliteindex.Index)(nil)
