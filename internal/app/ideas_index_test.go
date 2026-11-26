package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ideaIndexClock is the fixture instant; due windows and revision times stay
// independent of the machine clock.
var ideaIndexClock = time.Date(2026, time.November, 27, 10, 0, 0, 0, time.UTC)

// ideaIndexFixture is a local brain with a real Markdown store, a SQLite index
// and one registered project.
type ideaIndexFixture struct {
	app     *app.App
	store   *fsstore.Store
	dataDir string
}

func newIdeaIndexFixture(t *testing.T) *ideaIndexFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	if err := store.PutProject(context.Background(), domain.Project{
		ID: "p1", Name: "P1", Kind: domain.ProjectKindOther,
		CreatedAt: ideaIndexClock.Add(-90 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatalf("sqliteindex.Open: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	return &ideaIndexFixture{
		app: app.New(cfg, app.Dependencies{
			Store: store, Index: index,
			Now: func() time.Time { return ideaIndexClock },
		}),
		store:   store,
		dataDir: dataDir,
	}
}

func (f *ideaIndexFixture) addIdea(t *testing.T, input app.IdeaInput) domain.Idea {
	t.Helper()
	input.ProjectID = "p1"
	result, err := f.app.AddIdea(context.Background(), input)
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	return result.Idea
}

func (f *ideaIndexFixture) reindex(t *testing.T) app.IndexResult {
	t.Helper()
	result, err := f.app.Reindex(context.Background(), app.IndexOptions{})
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	return result
}

func (f *ideaIndexFixture) search(t *testing.T, text string, kinds ...string) []ports.Hit {
	t.Helper()
	hits, err := f.app.SearchWith(context.Background(), app.SearchOptions{
		Text: text, ProjectID: "p1", Kinds: kinds, Limit: 20,
	})
	if err != nil {
		t.Fatalf("SearchWith(%q): %v", text, err)
	}
	return hits
}

func ideaHit(t *testing.T, hits []ports.Hit, id string) ports.Hit {
	t.Helper()
	for _, hit := range hits {
		if hit.ID == id {
			return hit
		}
	}
	t.Fatalf("idea %s not in %+v", id, hits)
	return ports.Hit{}
}

// An idea is projected into the index after every write and found by title,
// body and tag, with its status, priority and due fields on the hit.
func TestIdeaIndexRoundTripAndSearch(t *testing.T) {
	fixture := newIdeaIndexFixture(t)
	priority := domain.IdeaPriorityHigh
	idea := fixture.addIdea(t, app.IdeaInput{
		Title: "Dark calendar theme", Body: "Check contrast and keyboard",
		Priority: priority, DueDate: "2026-11-28", Tags: []string{"ui", "calendar"},
	})

	for _, query := range []string{"theme", "contrast", "keyboard", "ui", "calendar"} {
		hits := fixture.search(t, query, ports.RecordIdea)
		if len(hits) != 1 || hits[0].ID != idea.ID {
			t.Fatalf("search %q = %+v, want the idea", query, hits)
		}
	}
	hit := ideaHit(t, fixture.search(t, "theme", ports.RecordIdea), idea.ID)
	if hit.Kind != ports.RecordIdea || hit.Status != string(domain.IdeaOpen) || hit.Priority != string(domain.IdeaPriorityHigh) {
		t.Fatalf("hit row = %+v", hit)
	}
	if hit.DueDate != "2026-11-28" || !hit.DueAt.IsZero() {
		t.Fatalf("hit due = %v / %q, want the all-day date", hit.DueAt, hit.DueDate)
	}
}

// An explicit idea kind filter never returns knowledge records, and a plain
// search still finds ideas: they are curated records.
func TestIdeaSearchHonorsKindFilter(t *testing.T) {
	fixture := newIdeaIndexFixture(t)
	idea := fixture.addIdea(t, app.IdeaInput{Title: "Calendar theme"})
	if _, err := fixture.store.AppendNote(context.Background(), domain.Note{
		ID: "note-2026-11-27-calendar", ProjectID: "p1", Title: "Calendar notes",
		Body: "calendar notes", CreatedAt: ideaIndexClock, UpdatedAt: ideaIndexClock,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.reindex(t)

	ideasOnly := fixture.search(t, "calendar", ports.RecordIdea)
	if len(ideasOnly) != 1 || ideasOnly[0].ID != idea.ID {
		t.Fatalf("idea search = %+v, want only the idea", ideasOnly)
	}
	anyKind := fixture.search(t, "calendar")
	if len(anyKind) != 2 {
		t.Fatalf("unfiltered search = %+v, want the idea and the note", anyKind)
	}
}

// The hooks keep the projection in sync with Add, Update, Trash and Restore, so
// a search never returns a stale title or a trashed row.
func TestIdeaIndexFollowsWritesAndTrash(t *testing.T) {
	ctx := context.Background()
	fixture := newIdeaIndexFixture(t)
	idea := fixture.addIdea(t, app.IdeaInput{Title: "Zebracache cleanup", Body: "old plan"})
	if hits := fixture.search(t, "zebracache", ports.RecordIdea); len(hits) != 1 {
		t.Fatalf("after add = %+v", hits)
	}

	title := "Quartz cleanup"
	status := domain.IdeaDone
	priority := domain.IdeaPriorityHigh
	if _, err := fixture.app.UpdateIdea(ctx, app.IdeaUpdateInput{
		ID: idea.ID, Title: &title, Status: &status, Priority: &priority,
	}); err != nil {
		t.Fatalf("UpdateIdea: %v", err)
	}
	if hits := fixture.search(t, "zebracache", ports.RecordIdea); len(hits) != 0 {
		t.Fatalf("stale title still indexed: %+v", hits)
	}
	hit := ideaHit(t, fixture.search(t, "quartz", ports.RecordIdea), idea.ID)
	if hit.Status != string(domain.IdeaDone) || hit.Priority != string(domain.IdeaPriorityHigh) {
		t.Fatalf("updated row = %+v", hit)
	}

	if _, err := fixture.app.TrashIdea(ctx, "", idea.ID); err != nil {
		t.Fatalf("TrashIdea: %v", err)
	}
	if hits := fixture.search(t, "quartz", ports.RecordIdea); len(hits) != 0 {
		t.Fatalf("trashed idea still indexed: %+v", hits)
	}
	if _, err := fixture.app.RestoreIdea(ctx, "", idea.ID); err != nil {
		t.Fatalf("RestoreIdea: %v", err)
	}
	hit = ideaHit(t, fixture.search(t, "quartz", ports.RecordIdea), idea.ID)
	if hit.Status != string(domain.IdeaDone) {
		t.Fatalf("restored row = %+v", hit)
	}
}

// A reindex rebuilds the idea rows from Markdown and prunes a row whose file is
// gone, which is the contract for every derived entry.
func TestReindexRebuildsAndPrunesIdeas(t *testing.T) {
	ctx := context.Background()
	fixture := newIdeaIndexFixture(t)
	idea := domain.Idea{
		ID:        "idea-2026-11-27-zebracache-01jcs6abcdefghjkmnpqrstvwx",
		ProjectID: "p1", Title: "Zebracache cleanup", Body: "old plan",
		Status: domain.IdeaOpen, Priority: domain.IdeaPriorityNormal,
		CreatedAt: ideaIndexClock, UpdatedAt: ideaIndexClock,
	}
	if _, err := fixture.store.AppendIdea(ctx, idea); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}
	result := fixture.reindex(t)
	if result.Ideas != 1 || result.Documents != 1 {
		t.Fatalf("reindex result = %+v, want one idea document", result)
	}
	if hits := fixture.search(t, "zebracache", ports.RecordIdea); len(hits) != 1 {
		t.Fatalf("reindexed idea = %+v", hits)
	}

	if err := os.Remove(ideaPath(fixture.store, "p1", idea.ID)); err != nil {
		t.Fatal(err)
	}
	result = fixture.reindex(t)
	if result.Ideas != 0 || result.Pruned != 1 {
		t.Fatalf("after removing the file = %+v, want one pruned row", result)
	}
	if hits := fixture.search(t, "zebracache", ports.RecordIdea); len(hits) != 0 {
		t.Fatalf("pruned idea still indexed: %+v", hits)
	}
}

// Two independent indexes over the same Markdown produce the same idea rows:
// the file, not the database, is the source of truth.
func TestIdeaRowsReproducedFromMarkdown(t *testing.T) {
	ctx := context.Background()
	fixture := newIdeaIndexFixture(t)
	idea := domain.Idea{
		ID:        "idea-2026-11-27-calendar-01jcs6abcdefghjkmnpqrstvwx",
		ProjectID: "p1", Title: "Calendar export", Body: "ics file",
		Status: domain.IdeaDoing, Priority: domain.IdeaPriorityLow, DueDate: "2026-12-01",
		CreatedAt: ideaIndexClock, UpdatedAt: ideaIndexClock,
	}
	if _, err := fixture.store.AppendIdea(ctx, idea); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}
	fixture.reindex(t)
	first := ideaHit(t, fixture.search(t, "calendar", ports.RecordIdea), idea.ID)

	second, err := sqliteindex.Open(filepath.Join(t.TempDir(), "second.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = fixture.dataDir
	rebuilt := app.New(cfg, app.Dependencies{
		Store: fixture.store, Index: second,
		Now: func() time.Time { return ideaIndexClock },
	})
	if _, err := rebuilt.Reindex(ctx, app.IndexOptions{}); err != nil {
		t.Fatal(err)
	}
	hits, err := rebuilt.SearchWith(ctx, app.SearchOptions{Text: "calendar", Kinds: []string{ports.RecordIdea}})
	if err != nil {
		t.Fatal(err)
	}
	secondHit := ideaHit(t, hits, idea.ID)
	if first.Title != secondHit.Title || first.Status != secondHit.Status ||
		first.Priority != secondHit.Priority || first.DueDate != secondHit.DueDate ||
		first.ProjectID != secondHit.ProjectID {
		t.Fatalf("reproduced row = %+v, want %+v", secondHit, first)
	}
}

// An unparsable idea file stays a problem row: it is reported, never indexed,
// never deleted and never rewritten.
func TestProblemIdeaFileIsNotIndexed(t *testing.T) {
	ctx := context.Background()
	fixture := newIdeaIndexFixture(t)
	valid := fixture.addIdea(t, app.IdeaInput{Title: "Valid idea", Body: "zebracache"})
	problemPath := ideaPath(fixture.store, "p1", "idea-2026-11-27-broken")
	conflicted := []byte("---\nid: idea-2026-11-27-broken\n<<<<<<< HEAD\n=======\n>>>>>>> other\n")
	if err := os.WriteFile(problemPath, conflicted, 0o644); err != nil {
		t.Fatal(err)
	}

	result := fixture.reindex(t)
	if result.Ideas != 1 || result.Documents != 1 {
		t.Fatalf("reindex result = %+v, want only the valid idea", result)
	}
	if hits := fixture.search(t, "zebracache", ports.RecordIdea); len(hits) != 1 || hits[0].ID != valid.ID {
		t.Fatalf("valid idea = %+v", hits)
	}
	list, err := fixture.app.ListIdeas(ctx, app.IdeaListOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, problem := range list.Problems {
		if problem.Kind == ports.RecordIdea && strings.HasSuffix(problem.Path, "idea-2026-11-27-broken.md") {
			found = true
		}
	}
	if !found {
		t.Fatalf("problems = %+v, want the conflicted file", list.Problems)
	}
	raw, err := os.ReadFile(problemPath)
	if err != nil || string(raw) != string(conflicted) {
		t.Fatalf("problem file changed: %q, %v", raw, err)
	}
}

// Ideas never enter task context or the bootstrap summary, even when their
// text matches the query and a knowledge record does too.
func TestIdeasExcludedFromContextAndBootstrap(t *testing.T) {
	ctx := context.Background()
	fixture := newIdeaIndexFixture(t)
	idea := fixture.addIdea(t, app.IdeaInput{Title: "Zebracache migration", Body: "zebracache plan"})
	if _, err := fixture.store.AppendNote(ctx, domain.Note{
		ID: "note-2026-11-27-zebracache", ProjectID: "p1", Title: "Zebracache notes",
		Body: "zebracache notes", CreatedAt: ideaIndexClock, UpdatedAt: ideaIndexClock,
	}); err != nil {
		t.Fatal(err)
	}
	fixture.reindex(t)

	contextResult, err := fixture.app.Context(ctx, app.ContextOptions{Query: "zebracache", ProjectID: "p1"})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	for _, record := range contextResult.Records {
		if record.Kind == ports.RecordIdea || record.ID == idea.ID {
			t.Fatalf("idea leaked into context: %+v", record)
		}
	}
	if strings.Contains(contextResult.Summary, idea.Title) {
		t.Fatalf("idea title in the context summary:\n%s", contextResult.Summary)
	}

	bootstrap, err := fixture.app.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "p1"})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if strings.Contains(bootstrap.Summary, idea.Title) {
		t.Fatalf("idea title in the bootstrap summary:\n%s", bootstrap.Summary)
	}
}

// doctor reports the idea kind, the idea problem files and the index schema,
// and a missing index is a warning, not a hard failure.
func TestDoctorReportsIdeasAndIndexSchema(t *testing.T) {
	ctx := context.Background()
	fixture := newIdeaIndexFixture(t)
	fixture.addIdea(t, app.IdeaInput{Title: "Doctor idea"})
	fixture.reindex(t)

	report, err := fixture.app.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	indexCheck, ideasCheck := doctorCheck(t, report, "index"), doctorCheck(t, report, "ideas")
	if indexCheck.Status != "ok" || !strings.Contains(indexCheck.Message, "schema 11") {
		t.Fatalf("index check = %+v, want schema 11", indexCheck)
	}
	if ideasCheck.Status != "ok" || !strings.Contains(ideasCheck.Message, "kind idea") || !strings.Contains(ideasCheck.Message, "1 idea") {
		t.Fatalf("ideas check = %+v, want the kind and the count", ideasCheck)
	}
	if strings.Contains(ideasCheck.Message, "problem") {
		t.Fatalf("ideas check reports a problem for a healthy file: %+v", ideasCheck)
	}

	if err := os.WriteFile(ideaPath(fixture.store, "p1", "idea-2026-11-27-broken"),
		[]byte("---\nid: idea-2026-11-27-broken\n<<<<<<< HEAD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err = fixture.app.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ideasCheck = doctorCheck(t, report, "ideas")
	if ideasCheck.Status != "warn" || !strings.Contains(ideasCheck.Message, "problem") {
		t.Fatalf("ideas check with a problem file = %+v, want a warning naming the count", ideasCheck)
	}
}

func TestDoctorWarnsWhenIndexIsMissing(t *testing.T) {
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	application := app.New(cfg, app.Dependencies{Store: store})
	report, err := application.Doctor(context.Background(), app.DoctorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check := doctorCheck(t, report, "index")
	if check.Status != "warn" || !strings.Contains(check.Message, "not available") {
		t.Fatalf("index check = %+v, want a warning", check)
	}
}
