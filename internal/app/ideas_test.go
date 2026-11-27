package app_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ideaFake assembles a synthetic credential from fragments so no complete
// credential-shaped literal appears in the test source.
func ideaFake(parts ...string) string { return strings.Join(parts, "") }

func newIdeaApp(t *testing.T) (*app.App, *fsstore.Store) {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	return app.New(cfg, app.Dependencies{Store: store}), store
}

func seedIdeaProject(t *testing.T, store *fsstore.Store, project domain.Project) domain.Project {
	t.Helper()
	if err := store.PutProject(context.Background(), project); err != nil {
		t.Fatalf("PutProject(%s): %v", project.ID, err)
	}
	return project
}

func ideaPath(store *fsstore.Store, projectID, id string) string {
	return filepath.Join(store.Root(), "projects", projectID, "ideas", id+".md")
}

func stringPointer(value string) *string { return &value }

func ideaStatus(value domain.IdeaStatus) *domain.IdeaStatus { return &value }

func ideaPriority(value domain.IdeaPriority) *domain.IdeaPriority { return &value }

func TestAddIdeaResolvesProjectExplicitly(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	dir := t.TempDir()
	project := seedIdeaProject(t, store, domain.Project{
		ID: "lv24-brain-desktop", Name: "Desktop", Aliases: []string{"desktop"}, Paths: []string{dir},
		Kind: domain.ProjectKindApp,
	})

	for _, selector := range []string{project.ID, "desktop", dir} {
		result, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: selector, Title: "From " + selector})
		if err != nil {
			t.Fatalf("AddIdea(%q): %v", selector, err)
		}
		if result.Idea.ProjectID != project.ID || result.ProjectSource != app.IdeaProjectExplicit {
			t.Fatalf("selector %q = project %q, source %q", selector, result.Idea.ProjectID, result.ProjectSource)
		}
		if result.Idea.Status != domain.IdeaOpen || result.Idea.Priority != domain.IdeaPriorityNormal {
			t.Fatalf("defaults = %q/%q", result.Idea.Status, result.Idea.Priority)
		}
		if _, err := os.Stat(ideaPath(store, project.ID, result.Idea.ID)); err != nil {
			t.Fatalf("idea file for %q: %v", selector, err)
		}
	}
}

func TestAddIdeaUsesWorkingDirectoryThenPersonal(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	cwdProject := t.TempDir()
	seedIdeaProject(t, store, domain.Project{
		ID: "checkout", Name: "Checkout", Paths: []string{cwdProject}, Kind: domain.ProjectKindRepository,
	})
	t.Chdir(cwdProject)
	result, err := a.AddIdea(ctx, app.IdeaInput{Title: "From cwd"})
	if err != nil {
		t.Fatalf("AddIdea(cwd): %v", err)
	}
	if result.Idea.ProjectID != "checkout" || result.ProjectSource != app.IdeaProjectCWD {
		t.Fatalf("cwd resolution = %q, %q", result.Idea.ProjectID, result.ProjectSource)
	}
}

func TestAddIdeaFallsBackToPersonal(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	seedIdeaProject(t, store, domain.Project{ID: "personal", Name: "Personal", Kind: domain.ProjectKindOther})
	t.Chdir(t.TempDir())
	result, err := a.AddIdea(ctx, app.IdeaInput{Title: "No cwd project"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	if result.Idea.ProjectID != "personal" || result.ProjectSource != app.IdeaProjectDefault {
		t.Fatalf("personal fallback = %q, %q", result.Idea.ProjectID, result.ProjectSource)
	}
}

func TestAddIdeaRejectsUnknownProjectWithoutInventingIt(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	seedIdeaProject(t, store, domain.Project{ID: "lv24-brain-desktop", Name: "Desktop", Kind: domain.ProjectKindApp})
	before, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	_, err = a.AddIdea(ctx, app.IdeaInput{ProjectID: "lv24-brain-desk", Title: "Typo"})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown project error = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "lv24-brain-desktop") {
		t.Fatalf("error does not name a candidate: %v", err)
	}
	after, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("unknown project was registered: %d -> %d", len(before), len(after))
	}
}

func TestAddIdeaFailsWithoutPersonalProject(t *testing.T) {
	ctx := context.Background()
	a, _ := newIdeaApp(t)
	t.Chdir(t.TempDir())
	if _, err := a.AddIdea(ctx, app.IdeaInput{Title: "Nowhere"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no project error = %v, want ErrValidation", err)
	}
}

// TestAddIdeaRoundTrip checks every field through the app and store, including
// the timed offset, tags, source and completion.
func TestAddIdeaRoundTrip(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	zone := time.FixedZone("MSK", 3*60*60)
	due := time.Date(2026, 11, 27, 18, 0, 0, 0, zone)
	remind := 120
	result, err := a.AddIdea(ctx, app.IdeaInput{
		ProjectID:           project.ID,
		Title:               "  Тёмная тема  ",
		Body:                "Сделать тёмную тему.\n\nПроверить контраст.",
		Status:              domain.IdeaDoing,
		Priority:            domain.IdeaPriorityHigh,
		DueAt:               &due,
		RemindBeforeMinutes: &remind,
		Tags:                []string{"ui", "calendar"},
		Source:              &domain.IdeaSource{Type: domain.IdeaSourceSession, ID: "ses_01jcs6example", Harness: "opencode"},
	})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	got, err := a.GetIdea(ctx, result.Idea.ID)
	if err != nil {
		t.Fatalf("GetIdea: %v", err)
	}
	if got.Title != "Тёмная тема" || got.Body != "Сделать тёмную тему.\n\nПроверить контраст." {
		t.Fatalf("title/body = %q / %q", got.Title, got.Body)
	}
	if got.Status != domain.IdeaDoing || got.Priority != domain.IdeaPriorityHigh {
		t.Fatalf("status/priority = %q/%q", got.Status, got.Priority)
	}
	if got.DueAt == nil || got.DueAt.Format(time.RFC3339) != "2026-11-27T18:00:00+03:00" {
		t.Fatalf("due_at = %v", got.DueAt)
	}
	if got.RemindBeforeMinutes == nil || *got.RemindBeforeMinutes != 120 {
		t.Fatalf("remind = %v", got.RemindBeforeMinutes)
	}
	if len(got.Tags) != 2 || got.Source == nil || got.Source.Harness != "opencode" {
		t.Fatalf("tags/source = %#v / %+v", got.Tags, got.Source)
	}
}

func TestAddIdeaRedactsSecrets(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	token := ideaFake("ghp_", "0123456789abcdefghijklmnopqrstuvwxyzAB")
	result, err := a.AddIdea(ctx, app.IdeaInput{
		ProjectID:      project.ID,
		Title:          "Rotate " + token,
		Body:           "used " + token + " today",
		CompletionNote: "done: " + token,
	})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	raw, err := os.ReadFile(ideaPath(store, project.ID, result.Idea.ID))
	if err != nil {
		t.Fatalf("read idea file: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatalf("token was stored:\n%s", raw)
	}
	if !strings.Contains(string(raw), "[redacted]") {
		t.Fatalf("redaction marker missing:\n%s", raw)
	}
}

func TestAddIdeaRejectsInvalidUTF8AndLongSource(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	invalid := string([]byte{0xff, 0xfe})
	if _, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: invalid}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid UTF-8 title error = %v, want ErrValidation", err)
	}
	if _, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "ok", Body: invalid}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid UTF-8 body error = %v, want ErrValidation", err)
	}
	longSource := &domain.IdeaSource{Type: domain.IdeaSourceSession, ID: strings.Repeat("a", 257)}
	if _, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "ok", Source: longSource}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("long source id error = %v, want ErrValidation", err)
	}
	if _, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "legacy", ID: "idea-2026-11-26-legacy"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("legacy pinned id error = %v, want ErrValidation", err)
	}
	listed, err := a.ListIdeas(ctx, app.IdeaListOptions{ProjectID: project.ID})
	if err != nil || len(listed.Ideas) != 0 {
		t.Fatalf("refused writes created ideas: %+v, %v", listed.Ideas, err)
	}
}

func TestAddIdeaDoneSetsCompletedAt(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	added, err := a.AddIdea(ctx, app.IdeaInput{
		ProjectID: project.ID,
		Title:     "Already finished",
		Status:    domain.IdeaDone,
	})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	if added.Idea.CompletedAt == nil {
		t.Fatal("done idea was created without completed_at")
	}
	got, err := a.GetIdea(ctx, added.Idea.ID)
	if err != nil || got.Status != domain.IdeaDone || got.CompletedAt == nil {
		t.Fatalf("stored done idea = %+v, %v", got, err)
	}
}

func TestIdeaStatusTransitionsAndCompletion(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	added, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "Flow"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	id := added.Idea.ID

	doing, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: id, Status: ideaStatus(domain.IdeaDoing)})
	if err != nil || doing.Idea.Status != domain.IdeaDoing {
		t.Fatalf("open -> doing = %+v, %v", doing, err)
	}
	if _, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: id, Status: ideaStatus(domain.IdeaSomeday)}); err != nil {
		t.Fatalf("doing -> someday: %v", err)
	}
	if _, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: id, Status: ideaStatus(domain.IdeaDoing)}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("someday -> doing error = %v, want ErrValidation", err)
	}
	done, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: id, Status: ideaStatus(domain.IdeaDone), CompletionNote: stringPointer("finished")})
	if err != nil || done.Idea.CompletedAt == nil || done.Idea.CompletionNote != "finished" {
		t.Fatalf("done = %+v, %v", done, err)
	}
	if _, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: id, Status: ideaStatus(domain.IdeaDoing)}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("done -> doing error = %v, want ErrValidation", err)
	}
	reopened, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: id, Status: ideaStatus(domain.IdeaOpen)})
	if err != nil || reopened.Idea.CompletedAt != nil || reopened.Idea.CompletionNote != "" {
		t.Fatalf("reopen = %+v, %v", reopened, err)
	}
	if _, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: id, Status: ideaStatus(domain.IdeaSomeday)}); err != nil {
		t.Fatalf("open -> someday: %v", err)
	}
	dropped, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: id, Status: ideaStatus(domain.IdeaDropped), CompletionNote: stringPointer("not now")})
	if err != nil || dropped.Idea.Status != domain.IdeaDropped || dropped.Idea.CompletionNote != "not now" || dropped.Idea.CompletedAt != nil {
		t.Fatalf("dropped = %+v, %v", dropped, err)
	}
	unparked, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: id, Status: ideaStatus(domain.IdeaOpen)})
	if err != nil || unparked.Idea.CompletionNote != "" || unparked.Idea.Status != domain.IdeaOpen {
		t.Fatalf("dropped -> open = %+v, %v", unparked, err)
	}
}

func TestUpdateIdeaStaleRevisionConflicts(t *testing.T) {
	ctx := context.Background()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	clock := time.Date(2026, 11, 27, 1, 37, 2, 0, time.UTC)
	a := app.New(cfg, app.Dependencies{Store: store, Now: func() time.Time { return clock }})
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})

	added, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "First"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	revision := added.Idea.UpdatedAt
	updated, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{
		ID: added.Idea.ID, Title: stringPointer("Second"), ExpectedUpdatedAt: revision,
	})
	if err != nil {
		t.Fatalf("UpdateIdea: %v", err)
	}
	if !updated.Idea.UpdatedAt.After(revision) {
		t.Fatalf("revision did not advance: %v -> %v", revision, updated.Idea.UpdatedAt)
	}

	// A second writer that still holds the create-time revision must be
	// refused and must not overwrite the newer title.
	if _, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{
		ID: added.Idea.ID, Title: stringPointer("Stale"), ExpectedUpdatedAt: revision,
	}); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale CAS error = %v, want ErrConflict", err)
	}
	got, err := a.GetIdea(ctx, added.Idea.ID)
	if err != nil {
		t.Fatalf("GetIdea: %v", err)
	}
	if got.Title != "Second" {
		t.Fatalf("stale write overwrote the title: %q", got.Title)
	}
	if !got.UpdatedAt.Equal(updated.Idea.UpdatedAt) {
		t.Fatalf("stale write moved the revision: %v -> %v", updated.Idea.UpdatedAt, got.UpdatedAt)
	}
}

func TestUpdateIdeaMovesProject(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	first := seedIdeaProject(t, store, domain.Project{ID: "first", Name: "First", Kind: domain.ProjectKindRepository})
	second := seedIdeaProject(t, store, domain.Project{ID: "second", Name: "Second", Kind: domain.ProjectKindApp})
	added, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: first.ID, Title: "Move me"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	moved, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: added.Idea.ID, ProjectID: second.ID})
	if err != nil {
		t.Fatalf("UpdateIdea: %v", err)
	}
	if !moved.Moved || moved.ProjectSource != app.IdeaProjectExplicit || moved.Idea.ProjectID != second.ID {
		t.Fatalf("move result = %+v", moved)
	}
	if _, err := os.Stat(ideaPath(store, first.ID, added.Idea.ID)); !os.IsNotExist(err) {
		t.Fatalf("old idea file still exists: %v", err)
	}
	if _, err := os.Stat(ideaPath(store, second.ID, added.Idea.ID)); err != nil {
		t.Fatalf("new idea file: %v", err)
	}
	got, err := a.GetIdea(ctx, added.Idea.ID)
	if err != nil || got.ProjectID != second.ID || got.ID != added.Idea.ID {
		t.Fatalf("moved idea = %+v, %v", got, err)
	}
	if !got.CreatedAt.Equal(added.Idea.CreatedAt) {
		t.Fatalf("created_at changed: %v -> %v", added.Idea.CreatedAt, got.CreatedAt)
	}
}

func TestAddIdeaRejectsInvalidCompletionNote(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	_, err := a.AddIdea(ctx, app.IdeaInput{
		ProjectID: project.ID, Title: "Bad note", CompletionNote: "bad\x00note",
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid completion note error = %v, want validation", err)
	}
}

func TestUpdateIdeaRejectsInvalidCompletionNote(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	added, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "Note"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	_, err = a.UpdateIdea(ctx, app.IdeaUpdateInput{
		ID: added.Idea.ID, CompletionNote: stringPointer("bad\x00note"),
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid completion note error = %v, want validation", err)
	}
}

func TestUpdateIdeaReopenClearsCompletionNote(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	added, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "Finish me"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	done := domain.IdeaDone
	updated, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{
		ID: added.Idea.ID, Status: &done, CompletionNote: stringPointer("shipped"),
	})
	if err != nil {
		t.Fatalf("done edit: %v", err)
	}
	if updated.Idea.CompletionNote != "shipped" || updated.Idea.CompletedAt == nil {
		t.Fatalf("done idea = %+v", updated.Idea)
	}
	open := domain.IdeaOpen
	reopened, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: added.Idea.ID, Status: &open})
	if err != nil {
		t.Fatalf("reopen edit: %v", err)
	}
	if reopened.Idea.CompletionNote != "" || reopened.Idea.CompletedAt != nil {
		t.Fatalf("reopened idea kept completion fields: %+v", reopened.Idea)
	}
}

func TestUpdateIdeaCompareAndSwapUsesTheStoredRevision(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	added, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "Original"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	revision := added.Idea.UpdatedAt
	if revision.Nanosecond() != 0 {
		t.Fatalf("the returned revision %v is not the second-precision stored value", revision)
	}

	_, err = a.UpdateIdea(ctx, app.IdeaUpdateInput{
		ID: added.Idea.ID, Title: stringPointer("Stale"), ExpectedUpdatedAt: revision.Add(-time.Hour),
	})
	var conflict *domain.RevisionConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("stale edit error = %v, want a revision conflict", err)
	}
	if conflict.CurrentRevision() != revision.UTC().Format(time.RFC3339) {
		t.Fatalf("current revision = %q, want %q", conflict.CurrentRevision(), revision.UTC().Format(time.RFC3339))
	}
	if got, err := a.GetIdea(ctx, added.Idea.ID); err != nil || got.Title != "Original" {
		t.Fatalf("a stale edit wrote: %q, %v", got.Title, err)
	}

	updated, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{
		ID: added.Idea.ID, Title: stringPointer("Fresh"), ExpectedUpdatedAt: revision,
	})
	if err != nil {
		t.Fatalf("fresh edit: %v", err)
	}
	stored, err := a.GetIdea(ctx, added.Idea.ID)
	if err != nil {
		t.Fatalf("GetIdea after edit: %v", err)
	}
	if !updated.Idea.UpdatedAt.Equal(stored.UpdatedAt) {
		t.Fatalf("returned revision %v != stored revision %v", updated.Idea.UpdatedAt, stored.UpdatedAt)
	}
	if stored.Title != "Fresh" {
		t.Fatalf("stored title = %q", stored.Title)
	}
}

func TestIdeaTrashRestoreAndCollision(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	added, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "Trash me", Body: "body"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	if _, err := a.TrashIdea(ctx, "", added.Idea.ID); err != nil {
		t.Fatalf("TrashIdea: %v", err)
	}
	if _, err := a.GetIdea(ctx, added.Idea.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("trashed idea still live: %v", err)
	}
	restored, err := a.RestoreIdea(ctx, "", added.Idea.ID)
	if err != nil || restored.Body != "body" || restored.ID != added.Idea.ID {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	// Trash again, create a live idea with the same pinned id, then refuse the
	// restore: nothing is overwritten.
	if _, err := a.TrashIdea(ctx, project.ID, added.Idea.ID); err != nil {
		t.Fatalf("TrashIdea second: %v", err)
	}
	if _, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "Collide", ID: added.Idea.ID}); err != nil {
		t.Fatalf("replacement AddIdea: %v", err)
	}
	if _, err := a.RestoreIdea(ctx, "", added.Idea.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("restore collision error = %v, want ErrConflict", err)
	}
	got, err := a.GetIdea(ctx, added.Idea.ID)
	if err != nil || got.Title != "Collide" {
		t.Fatalf("live idea was replaced by restore: %+v, %v", got, err)
	}
}

func TestTrashIdeaUsesStoredProjectForOrphan(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "orphan", Name: "Orphan", Kind: domain.ProjectKindRepository})
	empty, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "Empty selector"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	stored, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "Stored selector"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	// Forget the project: only projects/orphan/ideas remains on disk.
	registryPath := filepath.Join(store.Root(), "registry.json")
	if err := os.WriteFile(registryPath, []byte(`{"version":1,"projects":[]}`), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}

	// A different explicit project still has to resolve through the registry
	// and must not move the file.
	if _, err := a.TrashIdea(ctx, "other", stored.Idea.ID); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("TrashIdea(other) error = %v, want ErrValidation", err)
	}
	if _, err := os.Stat(ideaPath(store, project.ID, stored.Idea.ID)); err != nil {
		t.Fatalf("refused trash moved the live file: %v", err)
	}

	for _, test := range []struct {
		name     string
		selector string
		idea     domain.Idea
	}{
		{"empty selector", "", empty.Idea},
		{"stored selector", project.ID, stored.Idea},
	} {
		trashed, err := a.TrashIdea(ctx, test.selector, test.idea.ID)
		if err != nil {
			t.Fatalf("%s: TrashIdea: %v", test.name, err)
		}
		if trashed.ProjectID != project.ID {
			t.Fatalf("%s: project = %q, want %q", test.name, trashed.ProjectID, project.ID)
		}
		if _, err := os.Stat(ideaPath(store, project.ID, test.idea.ID)); !os.IsNotExist(err) {
			t.Fatalf("%s: live file still exists: %v", test.name, err)
		}
		trashedFiles, err := filepath.Glob(filepath.Join(store.Root(), "projects", project.ID, "trash", "ideas", test.idea.ID+"*.md"))
		if err != nil || len(trashedFiles) != 1 {
			t.Fatalf("%s: trash files = %v, %v", test.name, trashedFiles, err)
		}
		if _, err := a.GetIdea(ctx, test.idea.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("%s: trashed orphan still live: %v", test.name, err)
		}
	}

	// Reading remains sane: no live orphan appears in the global list.
	all, err := a.ListIdeas(ctx, app.IdeaListOptions{All: true})
	if err != nil || len(all.Ideas) != 0 {
		t.Fatalf("global list after orphan trash = %+v, %v", all.Ideas, err)
	}
	// Restore does not invent a project: it is refused, and the trashed file
	// stays exactly where it is for manual recovery.
	if _, err := a.RestoreIdea(ctx, project.ID, empty.Idea.ID); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("RestoreIdea(orphan) error = %v, want ErrValidation", err)
	}
	remaining, err := filepath.Glob(filepath.Join(store.Root(), "projects", project.ID, "trash", "ideas", empty.Idea.ID+"*.md"))
	if err != nil || len(remaining) != 1 {
		t.Fatalf("restore moved the orphan trash: %v, %v", remaining, err)
	}
}

func TestListIdeasFiltersTagsAndProblems(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	for _, input := range []app.IdeaInput{
		{ProjectID: project.ID, Title: "UI one", Tags: []string{"ui", "calendar"}},
		{ProjectID: project.ID, Title: "UI two", Tags: []string{"ui"}},
		{ProjectID: project.ID, Title: "API", Tags: []string{"api"}},
	} {
		if _, err := a.AddIdea(ctx, input); err != nil {
			t.Fatalf("AddIdea(%q): %v", input.Title, err)
		}
	}
	both, err := a.ListIdeas(ctx, app.IdeaListOptions{ProjectID: project.ID, Tags: []string{"UI", "Calendar"}})
	if err != nil || len(both.Ideas) != 1 || both.Ideas[0].Title != "UI one" {
		t.Fatalf("AND tag filter = %+v, %v", both.Ideas, err)
	}
	none, err := a.ListIdeas(ctx, app.IdeaListOptions{ProjectID: project.ID, Tags: []string{"ui", "api"}})
	if err != nil || len(none.Ideas) != 0 {
		t.Fatalf("impossible AND tag filter = %+v, %v", none.Ideas, err)
	}
	byQuery, err := a.ListIdeas(ctx, app.IdeaListOptions{ProjectID: project.ID, Query: "CALENDAR"})
	if err != nil || len(byQuery.Ideas) != 1 || byQuery.Ideas[0].Title != "UI one" {
		t.Fatalf("query over tags = %+v, %v", byQuery.Ideas, err)
	}

	conflictID := "idea-2026-11-26-conflict-01jcs6abcdefghjkmnpqrstvwx"
	conflictPath := ideaPath(store, project.ID, conflictID)
	conflict := "---\nproject_id: p\nkind: idea\nid: " + conflictID + "\ntitle: Conflict\n---\n<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> origin/main\n"
	if err := os.WriteFile(conflictPath, []byte(conflict), 0o644); err != nil {
		t.Fatalf("write conflict file: %v", err)
	}
	listed, err := a.ListIdeas(ctx, app.IdeaListOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatalf("ListIdeas: %v", err)
	}
	if len(listed.Ideas) != 3 || len(listed.Problems) != 1 || listed.Problems[0].Kind != ports.RecordIdea {
		t.Fatalf("list with problem = %d ideas, %+v", len(listed.Ideas), listed.Problems)
	}
	if !strings.Contains(listed.Problems[0].Reason, "conflict") {
		t.Fatalf("problem reason = %q", listed.Problems[0].Reason)
	}
	var problem *domain.ProblemError
	if _, err := a.GetIdea(ctx, conflictID); !errors.As(err, &problem) {
		t.Fatalf("GetIdea problem error = %v, want *domain.ProblemError", err)
	}
	before, err := os.ReadFile(conflictPath)
	if err != nil {
		t.Fatalf("read conflict file: %v", err)
	}
	if _, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: conflictID, Title: stringPointer("rewritten")}); !errors.As(err, &problem) {
		t.Fatalf("UpdateIdea problem error = %v, want *domain.ProblemError", err)
	}
	if _, err := a.TrashIdea(ctx, project.ID, conflictID); !errors.As(err, &problem) {
		t.Fatalf("TrashIdea problem error = %v, want *domain.ProblemError", err)
	}
	after, err := os.ReadFile(conflictPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("problem file was modified: %v", err)
	}
}

func TestRecordCommandsRefuseIdeasButListThem(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "p", Name: "P", Kind: domain.ProjectKindRepository})
	added, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "Visible"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	listed, err := a.ListRecords(ctx, app.ListRecordsOptions{ProjectID: project.ID, Kinds: []string{ports.RecordIdea}})
	if err != nil || len(listed) != 1 || listed[0].Kind != ports.RecordIdea || listed[0].Title != "Visible" {
		t.Fatalf("record list ideas = %+v, %v", listed, err)
	}
	record, err := a.GetRecord(ctx, project.ID, added.Idea.ID)
	if err != nil || record.Idea == nil || record.Idea.ID != added.Idea.ID {
		t.Fatalf("record show idea = %+v, %v", record, err)
	}
	if _, err := a.TrashRecord(ctx, project.ID, added.Idea.ID); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("record trash idea error = %v, want ErrValidation", err)
	}
	if _, err := a.RestoreRecord(ctx, project.ID, added.Idea.ID); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("record restore idea error = %v, want ErrValidation", err)
	}
	if _, err := a.EditRecord(ctx, app.RecordEdit{ProjectID: project.ID, ID: added.Idea.ID, Title: stringPointer("nope")}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("record edit idea error = %v, want ErrValidation", err)
	}
	if _, err := a.SetRecordPinned(ctx, project.ID, added.Idea.ID, true); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("record pin idea error = %v, want ErrValidation", err)
	}
	if _, err := a.GetIdea(ctx, added.Idea.ID); err != nil {
		t.Fatalf("idea was changed by a refused command: %v", err)
	}
	knowledge, err := a.Knowledge(ctx, project.ID)
	if err != nil || knowledge.Records() != 0 {
		t.Fatalf("knowledge counts ideas: %+v, %v", knowledge, err)
	}
}

// TestIdeaTwoClones checks the intended multi-machine flow: create on A, sync,
// see it on B; then create on B, sync, and see both on A without loss.
func TestIdeaTwoClones(t *testing.T) {
	ctx := context.Background()
	machines := newRawZoneMachines(t)
	a := appForMachine(t, machines, 0)
	if _, err := a.SaveProject(ctx, app.ProjectInput{ID: "ideas-shared"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	machines.sync(0, "seed project")
	machines.git(machines.dirs[1], "pull", "-q", "--rebase", "origin", "main")
	b := appForMachine(t, machines, 1)

	first, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: "ideas-shared", Title: "From A", Body: "a"})
	if err != nil {
		t.Fatalf("A AddIdea: %v", err)
	}
	machines.sync(0, "idea from A")
	machines.git(machines.dirs[1], "pull", "-q", "--rebase", "origin", "main")
	onB, err := b.ListIdeas(ctx, app.IdeaListOptions{ProjectID: "ideas-shared"})
	if err != nil || len(onB.Ideas) != 1 || onB.Ideas[0].ID != first.Idea.ID {
		t.Fatalf("B did not see A's idea: %+v, %v", onB.Ideas, err)
	}

	second, err := b.AddIdea(ctx, app.IdeaInput{ProjectID: "ideas-shared", Title: "From B", Body: "b"})
	if err != nil {
		t.Fatalf("B AddIdea: %v", err)
	}
	machines.sync(1, "idea from B")
	machines.git(machines.dirs[0], "pull", "-q", "--rebase", "origin", "main")
	onA, err := a.ListIdeas(ctx, app.IdeaListOptions{ProjectID: "ideas-shared"})
	if err != nil || len(onA.Ideas) != 2 {
		t.Fatalf("A did not see both ideas: %+v, %v", onA.Ideas, err)
	}
	ids := map[string]bool{}
	for _, idea := range onA.Ideas {
		ids[idea.ID] = true
	}
	if !ids[first.Idea.ID] || !ids[second.Idea.ID] {
		t.Fatalf("a record was lost: %+v", onA.Ideas)
	}
}

func TestGetIdeaReadsAnOrphanProject(t *testing.T) {
	ctx := context.Background()
	a, store := newIdeaApp(t)
	project := seedIdeaProject(t, store, domain.Project{ID: "gone", Name: "Gone", Kind: domain.ProjectKindOther})
	added, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: project.ID, Title: "Still here"})
	if err != nil {
		t.Fatalf("AddIdea: %v", err)
	}
	// Simulate a registry that no longer lists the project. The idea files stay
	// on disk and must remain readable by their global id.
	registryPath := filepath.Join(store.Root(), "registry.json")
	if err := os.WriteFile(registryPath, []byte(`{"version":1,"projects":[]}`), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	got, err := a.GetIdea(ctx, added.Idea.ID)
	if err != nil {
		t.Fatalf("GetIdea orphan: %v", err)
	}
	if got.ProjectID != "gone" || got.Title != "Still here" {
		t.Fatalf("orphan idea = %+v", got)
	}
	updated, err := a.UpdateIdea(ctx, app.IdeaUpdateInput{ID: added.Idea.ID, Title: stringPointer("Still editable")})
	if err != nil || updated.Idea.Title != "Still editable" {
		t.Fatalf("in-place orphan update = %+v, %v", updated, err)
	}
	all, err := a.ListIdeas(ctx, app.IdeaListOptions{All: true})
	if err != nil || len(all.Ideas) != 1 || all.Ideas[0].ID != added.Idea.ID {
		t.Fatalf("global list orphan = %+v, %v", all.Ideas, err)
	}
	if all.ProjectSource != "" {
		t.Fatalf("all scope ProjectSource = %q, want empty (no single project)", all.ProjectSource)
	}
}

// pointerFixture seeds two projects and returns their actionable counts.
func pointerFixture(t *testing.T) (*app.App, *fsstore.Store) {
	t.Helper()
	ctx := context.Background()
	a, store := newIdeaApp(t)
	seedIdeaProject(t, store, domain.Project{ID: "demo", Name: "Demo", Kind: domain.ProjectKindRepository})
	seedIdeaProject(t, store, domain.Project{ID: "personal", Name: "Personal", Kind: domain.ProjectKindOther})
	today := time.Now().In(time.Local).Format(domain.IdeaDateLayout)
	for i := range 4 {
		due := ""
		if i < 2 {
			due = today
		}
		if _, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: "demo", Title: fmt.Sprintf("Demo item %d", i), DueDate: due}); err != nil {
			t.Fatalf("AddIdea demo %d: %v", i, err)
		}
	}
	for i := range 2 {
		if _, err := a.AddIdea(ctx, app.IdeaInput{ProjectID: "personal", Title: fmt.Sprintf("Personal item %d", i)}); err != nil {
			t.Fatalf("AddIdea personal %d: %v", i, err)
		}
	}
	return a, store
}

func TestIdeaPointerCountsAndBudget(t *testing.T) {
	ctx := context.Background()
	a, _ := pointerFixture(t)
	pointer, err := a.IdeaPointer(ctx, "demo", 3)
	if err != nil {
		t.Fatalf("IdeaPointer: %v", err)
	}
	if pointer.ProjectOpen != 4 || pointer.Open != 6 || pointer.DueToday != 2 {
		t.Fatalf("pointer counts = %+v", pointer)
	}
	if !strings.HasPrefix(pointer.Text, "ideas: 4 open in this project · 6 open overall, 2 due today") {
		t.Fatalf("pointer text = %q", pointer.Text)
	}
	if utf8.RuneCountInString(pointer.Text) > app.IdeaPointerMaxCharacters {
		t.Fatalf("pointer is %d runes, cap %d", utf8.RuneCountInString(pointer.Text), app.IdeaPointerMaxCharacters)
	}
	if items := strings.Count(pointer.Text, "\n- "); items > 3 || items == 0 {
		t.Fatalf("pointer items = %d in %q", items, pointer.Text)
	}
	// The block never mentions personal items: it is project-scoped.
	if strings.Contains(pointer.Text, "Personal item") {
		t.Fatalf("pointer leaked another project: %q", pointer.Text)
	}
}

func TestBootstrapCarriesTheIdeasPointerAndSessionBlock(t *testing.T) {
	ctx := context.Background()
	a, _ := pointerFixture(t)
	packed, err := a.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "demo", TokenBudget: 4000})
	if err != nil {
		t.Fatalf("Bootstrap packed: %v", err)
	}
	if !strings.Contains(packed.Summary, "ideas: 4 open in this project · 6 open overall") {
		t.Fatalf("packed summary has no pointer:\n%s", packed.Summary)
	}
	if strings.Index(packed.Summary, "ideas:") < strings.Index(packed.Summary, "## Latest handoff") {
		t.Fatalf("pointer is above the handoff:\n%s", packed.Summary)
	}
	if !strings.Contains(packed.Summary, "Demo item") {
		t.Fatalf("packed summary has no project item:\n%s", packed.Summary)
	}
	// A tiny budget keeps the curated context: the optional pointer never
	// crowds out decisions.
	small, err := a.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "demo", TokenBudget: 400})
	if err != nil {
		t.Fatalf("Bootstrap small: %v", err)
	}
	if strings.Contains(small.Summary, "ideas:") {
		t.Fatalf("pointer crowded a 400-token answer:\n%s", small.Summary)
	}

	block := "## Ideas — 4 open · 2 due today\n- [ ] today — Demo item 0 (demo)"
	plain, err := a.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "demo", Budget: 4000, IdeasBlock: block})
	if err != nil {
		t.Fatalf("Bootstrap rune: %v", err)
	}
	handoff := strings.Index(plain.Summary, "## Latest handoff")
	ideas := strings.Index(plain.Summary, "## Ideas")
	decisions := strings.Index(plain.Summary, "## Recent decisions")
	if handoff < 0 || ideas < 0 || decisions < 0 || !(handoff < ideas && ideas < decisions) {
		t.Fatalf("ideas block position = handoff %d, ideas %d, decisions %d:\n%s",
			handoff, ideas, decisions, plain.Summary)
	}
	if !strings.Contains(plain.Summary, "- [ ] today — Demo item 0 (demo)") {
		t.Fatalf("the supplied block was not rendered verbatim:\n%s", plain.Summary)
	}
}
