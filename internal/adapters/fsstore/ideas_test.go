package fsstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ideaULID is a valid, fixed Crockford base32 ULID suffix so a test can pin an
// idea id without depending on the random source.
const ideaULID = "01jcs6abcdefghjkmnpqrstvwx"

func ideaID(date, slug string) string {
	at, err := time.Parse(domain.IdeaDateLayout, date)
	if err != nil {
		panic(err)
	}
	return domain.NewRecordID(ports.RecordIdea, at, slug, ideaULID)
}

func intPtr(value int) *int { return &value }

func moscowTime(t *testing.T, year int, month time.Month, day, hour int) time.Time {
	t.Helper()
	zone := time.FixedZone("MSK", 3*60*60)
	return time.Date(year, month, day, hour, 0, 0, 0, zone)
}

func readIdea(t *testing.T, store *Store, id string) domain.Idea {
	t.Helper()
	idea, err := store.GetIdea(context.Background(), id)
	if err != nil {
		t.Fatalf("GetIdea(%q): %v", id, err)
	}
	return idea
}

func TestIdeaRoundTripTimed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	due := moscowTime(t, 2026, time.November, 27, 18)
	idea := domain.Idea{
		ID:                  ideaID("2026-11-26", "calendar-dark-theme"),
		ProjectID:           project.ID,
		Title:               "Тёмная тема для календаря",
		Body:                "Сделать тёмную тему.\n\nПроверить контраст и клавиатуру.",
		Status:              domain.IdeaOpen,
		Priority:            domain.IdeaPriorityNormal,
		DueAt:               &due,
		RemindBeforeMinutes: intPtr(120),
		Tags:                []string{"ui", "calendar"},
		Source:              &domain.IdeaSource{Type: domain.IdeaSourceSession, ID: "ses_01jcs6example", Harness: "opencode"},
		Provenance:          domain.Provenance{Origin: domain.OriginAgent, Reviewed: true, Trust: domain.TrustVerified},
		CreatedAt:           created,
	}
	if _, err := store.AppendIdea(ctx, idea); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}

	path := filepath.Join(store.Root(), "projects", project.ID, "ideas", idea.ID+".md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read idea file: %v", err)
	}
	for _, want := range []string{
		"kind: idea",
		"2026-11-27T18:00:00+03:00",
		"remind_before_minutes: 120",
		"id: " + idea.ID,
		"source:",
		"created_at: 2026-11-26T10:00:00Z",
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("idea file is missing %q:\n%s", want, raw)
		}
	}
	for _, unwanted := range []string{"due_date:", "content_updated_at:", "permalink:"} {
		if strings.Contains(string(raw), unwanted) {
			t.Fatalf("idea file must not contain %q:\n%s", unwanted, raw)
		}
	}

	got := readIdea(t, store, idea.ID)
	if got.ID != idea.ID || got.ProjectID != project.ID || got.Title != idea.Title || got.Body != idea.Body {
		t.Fatalf("idea identity mismatch: %+v", got)
	}
	if got.Status != domain.IdeaOpen || got.Priority != domain.IdeaPriorityNormal {
		t.Fatalf("status/priority = %q/%q", got.Status, got.Priority)
	}
	if got.DueAt == nil || got.DueAt.Format(time.RFC3339) != "2026-11-27T18:00:00+03:00" {
		t.Fatalf("due_at offset lost: %v", got.DueAt)
	}
	if !got.DueAt.Equal(due) {
		t.Fatalf("due instant = %v, want %v", got.DueAt, due)
	}
	if got.DueDate != "" {
		t.Fatalf("due_date = %q, want empty", got.DueDate)
	}
	if got.RemindBeforeMinutes == nil || *got.RemindBeforeMinutes != 120 {
		t.Fatalf("remind_before_minutes = %v", got.RemindBeforeMinutes)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "ui" || got.Tags[1] != "calendar" {
		t.Fatalf("tags = %#v", got.Tags)
	}
	if got.Source == nil || got.Source.Type != domain.IdeaSourceSession || got.Source.ID != "ses_01jcs6example" || got.Source.Harness != "opencode" {
		t.Fatalf("source = %+v", got.Source)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, created)
	}
	if got.CompletedAt != nil {
		t.Fatalf("completed_at = %v, want nil", got.CompletedAt)
	}
}

func TestIdeaRoundTripAllDay(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	idea := domain.Idea{
		ID:        ideaID("2026-11-26", "call-dentist"),
		ProjectID: project.ID,
		Title:     "Позвонить стоматологу",
		Status:    domain.IdeaOpen,
		Priority:  domain.IdeaPriorityHigh,
		DueDate:   "2026-11-27",
		Tags:      []string{"personal"},
		CreatedAt: created,
	}
	if _, err := store.AppendIdea(ctx, idea); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}
	got := readIdea(t, store, idea.ID)
	if got.DueDate != "2026-11-27" {
		t.Fatalf("due_date = %q, want 2026-11-27", got.DueDate)
	}
	if got.DueAt != nil {
		t.Fatalf("due_at = %v, want nil", got.DueAt)
	}
	if got.Priority != domain.IdeaPriorityHigh {
		t.Fatalf("priority = %q", got.Priority)
	}
	if got.RemindBeforeMinutes != nil {
		t.Fatalf("remind_before_minutes = %v, want absent", got.RemindBeforeMinutes)
	}
}

func TestIdeaCompletedRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	completed := time.Date(2026, 11, 27, 15, 30, 0, 0, time.UTC)
	idea := domain.Idea{
		ID:             ideaID("2026-11-26", "fix-footer"),
		ProjectID:      project.ID,
		Title:          "Починить футер",
		Status:         domain.IdeaDone,
		Priority:       domain.IdeaPriorityNormal,
		CompletedAt:    &completed,
		CompletionNote: "Тема проверена на контраст.",
		CreatedAt:      created,
	}
	if _, err := store.AppendIdea(ctx, idea); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}
	got := readIdea(t, store, idea.ID)
	if got.Status != domain.IdeaDone || got.CompletedAt == nil || !got.CompletedAt.Equal(completed) {
		t.Fatalf("completion = %q %v", got.Status, got.CompletedAt)
	}
	if got.CompletionNote != "Тема проверена на контраст." {
		t.Fatalf("completion_note = %q", got.CompletionNote)
	}
}

func TestIdeaAppendIsCreateOnly(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	idea := domain.Idea{
		ID:        ideaID("2026-11-26", "once"),
		ProjectID: project.ID,
		Title:     "Only once",
		Status:    domain.IdeaOpen,
		Priority:  domain.IdeaPriorityNormal,
		CreatedAt: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC),
	}
	if _, err := store.AppendIdea(ctx, idea); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}
	if _, err := store.AppendIdea(ctx, idea); !errors.Is(err, domain.ErrTargetExists) {
		t.Fatalf("second append error = %v, want ErrTargetExists", err)
	}
}

func TestIdeaUnknownProjectRejected(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	idea := domain.Idea{
		ID:        ideaID("2026-11-26", "ghost"),
		ProjectID: "ghost",
		Title:     "Ghost",
		Status:    domain.IdeaOpen,
		Priority:  domain.IdeaPriorityNormal,
		CreatedAt: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC),
	}
	if _, err := store.AppendIdea(ctx, idea); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("append unknown project error = %v, want ErrNotFound", err)
	}
	if _, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: "ghost"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("list unknown project error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetIdea(ctx, ideaID("2026-11-26", "missing")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get missing idea error = %v, want ErrNotFound", err)
	}
}

func TestIdeaListOrder(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	dueLater := moscowTime(t, 2026, time.November, 28, 18)
	dueSoon := moscowTime(t, 2026, time.November, 27, 9)
	ideas := []domain.Idea{
		{ID: ideaID("2026-11-26", "dropped"), Title: "dropped", Status: domain.IdeaDropped, Priority: domain.IdeaPriorityNormal},
		{ID: ideaID("2026-11-26", "done"), Title: "done", Status: domain.IdeaDone, Priority: domain.IdeaPriorityNormal},
		{ID: ideaID("2026-11-26", "someday"), Title: "someday", Status: domain.IdeaSomeday, Priority: domain.IdeaPriorityNormal},
		{ID: ideaID("2026-11-26", "doing"), Title: "doing", Status: domain.IdeaDoing, Priority: domain.IdeaPriorityNormal},
		{ID: ideaID("2026-11-26", "open-normal"), Title: "open normal", Status: domain.IdeaOpen, Priority: domain.IdeaPriorityNormal},
		{ID: ideaID("2026-11-26", "open-high"), Title: "open high", Status: domain.IdeaOpen, Priority: domain.IdeaPriorityHigh},
		{ID: ideaID("2026-11-26", "open-late"), Title: "open late", Status: domain.IdeaOpen, Priority: domain.IdeaPriorityLow, DueAt: &dueLater},
		{ID: ideaID("2026-11-26", "open-soon"), Title: "open soon", Status: domain.IdeaOpen, Priority: domain.IdeaPriorityLow, DueAt: &dueSoon},
	}
	for i := range ideas {
		ideas[i].ProjectID = project.ID
		ideas[i].CreatedAt = created
		ideas[i].UpdatedAt = created
		if _, err := store.AppendIdea(ctx, ideas[i]); err != nil {
			t.Fatalf("AppendIdea %q: %v", ideas[i].Title, err)
		}
	}
	listed, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: project.ID})
	if err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	want := []string{"open soon", "open late", "open high", "open normal", "doing", "someday", "done", "dropped"}
	if len(listed) != len(want) {
		t.Fatalf("listed %d ideas, want %d: %+v", len(listed), len(want), listed)
	}
	for i, title := range want {
		if listed[i].Title != title {
			t.Fatalf("order[%d] = %q, want %q (%+v)", i, listed[i].Title, title, listed)
		}
	}
}

func TestIdeaListFilters(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	due := moscowTime(t, 2026, time.November, 27, 18)
	ideas := []domain.Idea{
		{ID: ideaID("2026-11-26", "alpha-ui"), Title: "Alpha UI", Body: "dark calendar", Tags: []string{"ui"}},
		{ID: ideaID("2026-11-26", "beta-api"), Title: "Beta API", Body: "server routes", Tags: []string{"api"}, DueAt: &due},
		{ID: ideaID("2026-11-26", "gamma-done"), Title: "Gamma", Body: "finished", Status: domain.IdeaDone},
	}
	for i := range ideas {
		ideas[i].ProjectID = project.ID
		if ideas[i].Status == "" {
			ideas[i].Status = domain.IdeaOpen
		}
		ideas[i].Priority = domain.IdeaPriorityNormal
		ideas[i].CreatedAt = created
		if _, err := store.AppendIdea(ctx, ideas[i]); err != nil {
			t.Fatalf("AppendIdea %q: %v", ideas[i].Title, err)
		}
	}

	open, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: project.ID, Statuses: []domain.IdeaStatus{domain.IdeaOpen}})
	if err != nil || len(open) != 2 {
		t.Fatalf("open ideas = %d, %v", len(open), err)
	}
	query, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: project.ID, Query: "CALENDAR"})
	if err != nil || len(query) != 1 || query[0].Title != "Alpha UI" {
		t.Fatalf("query ideas = %+v, %v", query, err)
	}
	tag, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: project.ID, Query: "api"})
	if err != nil || len(tag) != 1 || tag[0].Title != "Beta API" {
		t.Fatalf("tag query ideas = %+v, %v", tag, err)
	}
	before := time.Date(2026, 11, 27, 23, 59, 0, 0, time.UTC)
	window, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: project.ID, DueBefore: before})
	if err != nil || len(window) != 1 || window[0].Title != "Beta API" {
		// An undated idea is excluded when a due bound is set; only Beta is
		// dated and before the bound.
		t.Fatalf("window ideas = %+v, %v", window, err)
	}
	limited, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: project.ID, Limit: 2})
	if err != nil || len(limited) != 2 {
		t.Fatalf("limited ideas = %d, %v", len(limited), err)
	}
}

func TestIdeaDueWindowExcludesUndated(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	dated := domain.Idea{
		ID: ideaID("2026-11-26", "dated"), ProjectID: project.ID, Title: "Dated",
		Status: domain.IdeaOpen, Priority: domain.IdeaPriorityNormal, DueDate: "2026-11-27", CreatedAt: created,
	}
	undated := domain.Idea{
		ID: ideaID("2026-11-26", "undated"), ProjectID: project.ID, Title: "Undated",
		Status: domain.IdeaOpen, Priority: domain.IdeaPriorityNormal, CreatedAt: created,
	}
	for _, idea := range []domain.Idea{dated, undated} {
		if _, err := store.AppendIdea(ctx, idea); err != nil {
			t.Fatalf("AppendIdea: %v", err)
		}
	}
	after := time.Date(2026, 11, 27, 0, 0, 0, 0, time.Local)
	before := time.Date(2026, 11, 28, 0, 0, 0, 0, time.Local)
	listed, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: project.ID, DueAfter: after, DueBefore: before})
	if err != nil || len(listed) != 1 || listed[0].Title != "Dated" {
		t.Fatalf("today window = %+v, %v", listed, err)
	}
}

func TestIdeaUpdateMovesProjectAndPreservesCreatedAt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	first := seedProject(t, store)
	second := domain.Project{ID: "desktop", Name: "Desktop", Kind: domain.ProjectKindApp}
	if err := store.PutProject(ctx, second); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	idea := domain.Idea{
		ID: ideaID("2026-11-26", "move-me"), ProjectID: first.ID, Title: "Move me",
		Status: domain.IdeaOpen, Priority: domain.IdeaPriorityNormal, CreatedAt: created,
	}
	if _, err := store.AppendIdea(ctx, idea); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}
	later := testClock.Add(2 * time.Hour)
	store.now = func() time.Time { return later }

	idea.ProjectID = second.ID
	idea.Status = domain.IdeaDone
	completed := later.Add(-time.Minute)
	idea.CompletedAt = &completed
	if err := store.UpdateIdea(ctx, idea); err != nil {
		t.Fatalf("UpdateIdea: %v", err)
	}
	firstPath := filepath.Join(store.Root(), "projects", first.ID, "ideas", idea.ID+".md")
	if _, err := os.Stat(firstPath); !os.IsNotExist(err) {
		t.Fatalf("old idea file still exists: %v", err)
	}
	secondPath := filepath.Join(store.Root(), "projects", second.ID, "ideas", idea.ID+".md")
	if _, err := os.Stat(secondPath); err != nil {
		t.Fatalf("moved idea file: %v", err)
	}
	got := readIdea(t, store, idea.ID)
	if got.ProjectID != second.ID || got.ID != idea.ID {
		t.Fatalf("moved idea = %+v", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("created_at = %v, want %v", got.CreatedAt, created)
	}
	if !got.UpdatedAt.Equal(later.UTC().Truncate(time.Second)) {
		t.Fatalf("updated_at = %v, want %v", got.UpdatedAt, later)
	}
	if got.Status != domain.IdeaDone || got.CompletedAt == nil {
		t.Fatalf("completion = %q %v", got.Status, got.CompletedAt)
	}
}

func TestIdeaTrashAndRestore(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	idea := domain.Idea{
		ID: ideaID("2026-11-26", "trash-me"), ProjectID: project.ID, Title: "Trash me",
		Body: "keep this body", Status: domain.IdeaOpen, Priority: domain.IdeaPriorityNormal, CreatedAt: created,
	}
	if _, err := store.AppendIdea(ctx, idea); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}
	if err := store.TrashIdea(ctx, project.ID, idea.ID); err != nil {
		t.Fatalf("TrashIdea: %v", err)
	}
	if _, err := store.GetIdea(ctx, idea.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("trashed idea still live: %v", err)
	}
	trashPath := filepath.Join(store.Root(), "projects", project.ID, "trash", "ideas", idea.ID+".md")
	if _, err := os.Stat(trashPath); err != nil {
		t.Fatalf("trash file: %v", err)
	}
	if err := store.RestoreIdea(ctx, project.ID, idea.ID); err != nil {
		t.Fatalf("RestoreIdea: %v", err)
	}
	got := readIdea(t, store, idea.ID)
	if got.Body != "keep this body" || got.ID != idea.ID {
		t.Fatalf("restored idea = %+v", got)
	}
	if _, err := os.Stat(trashPath); !os.IsNotExist(err) {
		t.Fatalf("trash file still exists: %v", err)
	}
}

func TestIdeaRestoreCollisionIsConflict(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	idea := domain.Idea{
		ID: ideaID("2026-11-26", "collide"), ProjectID: project.ID, Title: "Collide",
		Status: domain.IdeaOpen, Priority: domain.IdeaPriorityNormal, CreatedAt: created,
	}
	if _, err := store.AppendIdea(ctx, idea); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}
	if err := store.TrashIdea(ctx, project.ID, idea.ID); err != nil {
		t.Fatalf("TrashIdea: %v", err)
	}
	replacement := idea
	replacement.Title = "Collide again"
	if _, err := store.AppendIdea(ctx, replacement); err != nil {
		t.Fatalf("replacement AppendIdea: %v", err)
	}
	if err := store.RestoreIdea(ctx, project.ID, idea.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("restore collision error = %v, want ErrConflict", err)
	}
	got := readIdea(t, store, idea.ID)
	if got.Title != "Collide again" {
		t.Fatalf("live idea was overwritten: %+v", got)
	}
}

func TestIdeaConflictFileIsProblemRow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	good := domain.Idea{
		ID: ideaID("2026-11-26", "good"), ProjectID: project.ID, Title: "Good",
		Status: domain.IdeaOpen, Priority: domain.IdeaPriorityNormal, CreatedAt: created,
	}
	if _, err := store.AppendIdea(ctx, good); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}
	conflictID := ideaID("2026-11-26", "conflicted")
	conflictPath := filepath.Join(store.Root(), "projects", project.ID, "ideas", conflictID+".md")
	conflict := "---\nproject_id: " + project.ID + "\nkind: idea\nid: " + conflictID + "\ntitle: Conflicted\n---\n<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> origin/main\n"
	if err := os.WriteFile(conflictPath, []byte(conflict), 0o644); err != nil {
		t.Fatalf("write conflict file: %v", err)
	}

	listed, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: project.ID})
	if err != nil || len(listed) != 1 || listed[0].Title != "Good" {
		t.Fatalf("listed ideas = %+v, %v", listed, err)
	}
	problems, err := store.Problems(ctx, project.ID)
	if err != nil {
		t.Fatalf("Problems: %v", err)
	}
	found := false
	for _, problem := range problems {
		if problem.Kind == ports.RecordIdea && problem.Path == conflictPath {
			found = true
			if !strings.Contains(problem.Reason, "conflict") {
				t.Fatalf("problem reason = %q", problem.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("idea problem not reported: %+v", problems)
	}
	if _, err := store.GetIdea(ctx, conflictID); err == nil {
		t.Fatal("GetIdea returned a conflicted file as a valid idea")
	}
	if _, err := os.Stat(conflictPath); err != nil {
		t.Fatalf("conflict file was deleted: %v", err)
	}
}

func TestIdeaMalformedFileIsProblemRow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	badID := ideaID("2026-11-26", "no-title")
	badPath := filepath.Join(store.Root(), "projects", project.ID, "ideas", badID+".md")
	if err := os.MkdirAll(filepath.Dir(badPath), 0o755); err != nil {
		t.Fatalf("create ideas dir: %v", err)
	}
	bad := "---\nproject_id: " + project.ID + "\nkind: idea\nid: " + badID + "\nstatus: open\n---\nno title\n"
	if err := os.WriteFile(badPath, []byte(bad), 0o644); err != nil {
		t.Fatalf("write malformed file: %v", err)
	}
	if _, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: project.ID}); err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	problems, err := store.Problems(ctx, project.ID)
	if err != nil {
		t.Fatalf("Problems: %v", err)
	}
	found := false
	for _, problem := range problems {
		if problem.Kind == ports.RecordIdea && problem.Path == badPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("malformed idea not reported: %+v", problems)
	}
	if _, err := os.Stat(badPath); err != nil {
		t.Fatalf("malformed file was deleted: %v", err)
	}
}

func TestIdeaListAcrossProjects(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	first := seedProject(t, store)
	second := domain.Project{ID: "personal", Name: "Personal", Kind: domain.ProjectKindOther}
	if err := store.PutProject(ctx, second); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	for _, idea := range []domain.Idea{
		{ID: ideaID("2026-11-26", "one"), ProjectID: first.ID, Title: "One"},
		{ID: ideaID("2026-11-26", "two"), ProjectID: second.ID, Title: "Two"},
	} {
		idea.Status = domain.IdeaOpen
		idea.Priority = domain.IdeaPriorityNormal
		idea.CreatedAt = created
		if _, err := store.AppendIdea(ctx, idea); err != nil {
			t.Fatalf("AppendIdea %q: %v", idea.Title, err)
		}
	}
	all, err := store.ListIdeas(ctx, ports.IdeaFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("all ideas = %+v, %v", all, err)
	}
	one, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: second.ID})
	if err != nil || len(one) != 1 || one[0].ProjectID != second.ID {
		t.Fatalf("personal ideas = %+v, %v", one, err)
	}
}

func TestIdeaGeneratedID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	created := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	idea := domain.Idea{
		ProjectID: project.ID,
		Title:     "Dark theme",
		Status:    domain.IdeaOpen,
		Priority:  domain.IdeaPriorityNormal,
		CreatedAt: created,
	}
	if _, err := store.AppendIdea(ctx, idea); err != nil {
		t.Fatalf("AppendIdea: %v", err)
	}
	listed, err := store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: project.ID})
	if err != nil || len(listed) != 1 {
		t.Fatalf("listed ideas = %+v, %v", listed, err)
	}
	if !domain.IsRecordID(ports.RecordIdea, listed[0].ID) {
		t.Fatalf("generated id %q is not an idea id", listed[0].ID)
	}
	if !strings.HasPrefix(listed[0].ID, "idea-2026-11-26-dark-theme-") {
		t.Fatalf("generated id = %q", listed[0].ID)
	}
}
