package fsstore

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

func TestCurrentRevisionIsContentAddressed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	empty, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision empty: %v", err)
	}
	again, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision empty again: %v", err)
	}
	if empty.Kind != domain.SyncKindGit || !strings.HasPrefix(empty.Value, "sha256:") {
		t.Fatalf("empty revision = %+v", empty)
	}
	if empty.Value != again.Value {
		t.Fatalf("empty revision is not stable: %q vs %q", empty.Value, again.Value)
	}
	if !empty.At.IsZero() {
		t.Fatalf("empty revision At = %v, want zero", empty.At)
	}

	seedProject(t, store)
	afterProject, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision after project: %v", err)
	}
	if afterProject.Value == empty.Value {
		t.Fatal("revision did not change after PutProject")
	}
	stable, err := store.CurrentRevision(ctx)
	if err != nil || stable.Value != afterProject.Value {
		t.Fatalf("revision is not stable after write: %v / %+v", err, stable)
	}

	projectPath := filepath.Join(store.Root(), "registry.json")
	raw, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read project file: %v", err)
	}
	if err := os.WriteFile(projectPath, raw, 0o644); err != nil {
		t.Fatalf("rewrite project file: %v", err)
	}
	rewritten, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision after rewrite: %v", err)
	}
	if rewritten.Value != afterProject.Value {
		t.Fatal("revision changed when file content did not")
	}

	secretsDir := filepath.Join(store.Root(), "secrets")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		t.Fatalf("mkdir secrets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "token.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	excluded, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision excluded: %v", err)
	}
	if excluded.Value != afterProject.Value {
		t.Fatal("secret file changed the content revision")
	}

	if err := os.WriteFile(filepath.Join(store.Root(), "brain.yaml"), []byte("mode: local\n"), 0o644); err != nil {
		t.Fatalf("write brain.yaml: %v", err)
	}
	included, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision included: %v", err)
	}
	if included.Value == afterProject.Value {
		t.Fatal("included root file did not change the revision")
	}

	if err := os.WriteFile(projectPath, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("modify project file: %v", err)
	}
	changed, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision changed: %v", err)
	}
	if changed.Value == included.Value {
		t.Fatal("modified content did not change the revision")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.CurrentRevision(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled revision error = %v, want context.Canceled", err)
	}
}

func TestUnicodeCyrillicRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := domain.Project{
		ID:        "lv24",
		Name:      "Проект ЛВ24",
		Kind:      domain.ProjectKindApp,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := store.PutProject(ctx, project); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil || len(projects) != 1 || projects[0].Name != project.Name {
		t.Fatalf("ListProjects = %+v, %v", projects, err)
	}

	created := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	decision := domain.Decision{
		ProjectID:    project.ID,
		Title:        "Улучшить карту — решение",
		Status:       domain.DecisionAccepted,
		Context:      "Контекст решения с кириллицей.",
		Alternatives: []string{"Вариант А", "Вариант Б"},
		Consequences: []string{"Нужна проверка"},
		CreatedAt:    created,
	}
	if _, err := store.AppendDecision(ctx, decision); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	decisionPath := filepath.Join(store.Root(), "projects", project.ID, "decisions", "2026-11-05-uluchshit-kartu-reshenie.md")
	gotDecision, err := readDecisionFile(decisionPath)
	if err != nil {
		t.Fatalf("readDecisionFile: %v", err)
	}
	if gotDecision.Title != decision.Title || gotDecision.Context != decision.Context {
		t.Fatalf("Cyrillic decision = %+v", gotDecision)
	}
	if len(gotDecision.Alternatives) != 2 || gotDecision.Alternatives[0] != "Вариант А" {
		t.Fatalf("Cyrillic alternatives = %#v", gotDecision.Alternatives)
	}
	rawDecision, err := os.ReadFile(decisionPath)
	if err != nil {
		t.Fatalf("read decision raw: %v", err)
	}
	if !strings.Contains(string(rawDecision), "кириллицей") {
		t.Fatalf("decision file did not preserve Cyrillic:\n%s", rawDecision)
	}

	state := domain.State{
		ProjectID:  project.ID,
		Goal:       "Довести хранилище до готовности",
		Active:     []string{"Проверка кириллицы"},
		NextAction: "Запустить тесты",
	}
	if _, err := store.PutState(ctx, state); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	gotState, err := store.GetState(ctx, project.ID)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if gotState.Goal != state.Goal || gotState.Active[0] != "Проверка кириллицы" {
		t.Fatalf("Cyrillic state = %+v", gotState)
	}

	note := domain.Note{
		ProjectID: project.ID,
		Title:     "Заметка",
		Body:      "Заметка с эмодзи 🧠 и кириллицей.",
		Tags:      []string{"кириллица"},
		CreatedAt: created,
	}
	if _, err := store.AppendNote(ctx, note); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	notesPath := filepath.Join(store.Root(), "projects", project.ID, "notes", "2026-11-05.md")
	_, notes, err := readNotesFile(notesPath)
	if err != nil {
		t.Fatalf("readNotesFile: %v", err)
	}
	if len(notes) != 1 || notes[0].Body != note.Body || notes[0].Tags[0] != "кириллица" {
		t.Fatalf("Cyrillic note = %+v", notes)
	}

	first, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}
	second, err := store.CurrentRevision(ctx)
	if err != nil || first.Value != second.Value {
		t.Fatalf("Cyrillic revision is not stable: %+v / %+v (%v)", first, second, err)
	}
}

func TestProjectIDPathTraversalRejected(t *testing.T) {
	parent := t.TempDir()
	dataDir := filepath.Join(parent, "data")
	store, err := New(dataDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store.now = func() time.Time { return testClock }
	ctx := context.Background()

	invalidIDs := []string{
		"",
		"..",
		"../escape",
		`..\escape`,
		"a/b",
		`a\b`,
		"/absolute",
		`C:\temp`,
		"CON",
		"nul",
		".hidden",
		"trailing.",
		"white space",
	}
	for _, id := range invalidIDs {
		t.Run("put_"+id, func(t *testing.T) {
			err := store.PutProject(ctx, domain.Project{ID: id, Name: "escape"})
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("PutProject(%q) error = %v, want ErrValidation", id, err)
			}
		})
		if id == "" {
			continue
		}
		if _, err := store.GetProject(ctx, id); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("GetProject(%q) error = %v, want ErrValidation", id, err)
		}
		if _, err := store.PutState(ctx, domain.State{ProjectID: id}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("PutState(%q) error = %v, want ErrValidation", id, err)
		}
		if _, err := store.AppendNote(ctx, domain.Note{ProjectID: id, Body: "x"}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("AppendNote(%q) error = %v, want ErrValidation", id, err)
		}
		if _, err := store.AppendDecision(ctx, domain.Decision{ProjectID: id, Title: "x"}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("AppendDecision(%q) error = %v, want ErrValidation", id, err)
		}
	}

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "data" {
		t.Fatalf("traversal created entries outside the data dir: %v", entryNames(entries))
	}
	if _, err := os.Stat(filepath.Join(parent, "escape")); !os.IsNotExist(err) {
		t.Fatalf("escape file exists outside data dir: %v", err)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestWritesAreAtomicAndFrontmatterComplete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	if _, err := store.PutState(ctx, domain.State{ProjectID: project.ID, Goal: "goal"}); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	if _, err := store.AppendNote(ctx, domain.Note{
		ProjectID: project.ID,
		Title:     "note",
		Body:      "body",
		CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendNote: %v", err)
	}
	if _, err := store.AppendDecision(ctx, domain.Decision{
		ProjectID: project.ID,
		Title:     "decision",
		CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	if _, err := store.AppendAttempt(ctx, domain.Attempt{
		ProjectID: project.ID,
		Title:     "attempt",
		CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	if _, err := store.AppendHandoff(ctx, domain.Handoff{
		ProjectID: project.ID,
		Summary:   "handoff",
		CreatedAt: testClock,
	}); err != nil {
		t.Fatalf("AppendHandoff: %v", err)
	}

	walkErr := filepath.WalkDir(store.Root(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), tempFilePrefix) {
			t.Fatalf("temporary file left behind: %s", path)
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var frontmatter markdown.Frontmatter
		if _, err := markdown.Parse(raw, &frontmatter); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if frontmatter.ProjectID == "" || frontmatter.Kind == "" {
			t.Fatalf("%s has incomplete frontmatter: %+v", path, frontmatter)
		}
		if frontmatter.SchemaVersion != SchemaVersion {
			t.Fatalf("%s schema_version = %d", path, frontmatter.SchemaVersion)
		}
		if frontmatter.UpdatedAt.IsZero() {
			t.Fatalf("%s has zero updated_at", path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk data dir: %v", walkErr)
	}

	raw, err := os.ReadFile(filepath.Join(store.Root(), "registry.json"))
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	var registry projectRegistry
	if err := json.Unmarshal(raw, &registry); err != nil {
		t.Fatalf("parse registry: %v", err)
	}
	if registry.Version != registryVersion || len(registry.Projects) != 1 {
		t.Fatalf("registry = %+v", registry)
	}
}
