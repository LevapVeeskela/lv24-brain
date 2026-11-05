package fsstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

var testClock = time.Date(2026, 11, 5, 10, 30, 0, 0, time.UTC)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store.now = func() time.Time { return testClock }
	return store
}

func seedProject(t *testing.T, store *Store) domain.Project {
	t.Helper()
	project := domain.Project{
		ID:        "lv24-brain",
		Name:      "LV24 Brain",
		Kind:      domain.ProjectKindRepository,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if err := store.PutProject(context.Background(), project); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	return project
}

func globFiles(t *testing.T, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("Glob(%q): %v", pattern, err)
	}
	if matches == nil {
		return []string{}
	}
	return matches
}

func TestProjectRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	project := domain.Project{
		ID:            "lv24-brain",
		Name:          "LV24 Brain",
		Paths:         []string{"/srv/lv24-brain"},
		Aliases:       []string{"brain", "memory"},
		Kind:          domain.ProjectKindRepository,
		RepoURL:       "https://example.invalid/lv24-brain",
		DefaultBranch: "develop",
		CreatedAt:     created,
	}
	if err := store.PutProject(ctx, project); err != nil {
		t.Fatalf("PutProject: %v", err)
	}

	got, err := store.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.ID != project.ID || got.Name != project.Name || got.Kind != project.Kind {
		t.Fatalf("project identity mismatch: %+v", got)
	}
	if !reflect.DeepEqual(got.Paths, project.Paths) || !reflect.DeepEqual(got.Aliases, project.Aliases) {
		t.Fatalf("project slices mismatch: %+v", got)
	}
	if got.RepoURL != project.RepoURL || got.DefaultBranch != project.DefaultBranch {
		t.Fatalf("project repository metadata mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %v, want %v", got.CreatedAt, created)
	}
	if !got.UpdatedAt.Equal(testClock) {
		t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, testClock)
	}

	path := filepath.Join(store.Root(), "registry.json")
	raw, err := os.ReadFile(path)
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
	record := registry.Projects[0]
	if record.ID != project.ID || record.Name != project.Name || record.Kind != project.Kind {
		t.Fatalf("registry record = %+v", record)
	}

	clock := testClock.Add(time.Hour)
	store.now = func() time.Time { return clock }
	project.Name = "LV24 Brain Updated"
	if err := store.PutProject(ctx, project); err != nil {
		t.Fatalf("PutProject update: %v", err)
	}
	updated, err := store.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("GetProject after update: %v", err)
	}
	if updated.Name != "LV24 Brain Updated" {
		t.Fatalf("name after update = %q", updated.Name)
	}
	if !updated.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt changed on update: %v", updated.CreatedAt)
	}
	if !updated.UpdatedAt.Equal(clock) {
		t.Fatalf("UpdatedAt after update = %v, want %v", updated.UpdatedAt, clock)
	}
}

func TestListProjectsSortedAndFiltered(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	projects := []domain.Project{
		{ID: "zeta", Name: "zeta project", Kind: domain.ProjectKindOther},
		{ID: "alpha", Name: "Alpha project", Kind: domain.ProjectKindOther},
		{ID: "middle", Name: "Middle project", Kind: domain.ProjectKindOther},
	}
	for _, project := range projects {
		if err := store.PutProject(ctx, project); err != nil {
			t.Fatalf("PutProject(%s): %v", project.ID, err)
		}
	}

	got, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListProjects returned %d projects: %+v", len(got), got)
	}
	wantIDs := []string{"alpha", "middle", "zeta"}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Fatalf("project %d = %q, want %q", i, got[i].ID, id)
		}
	}

	if _, err := store.GetProject(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetProject(missing) error = %v, want ErrNotFound", err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	freshUntil := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	state := domain.State{
		ProjectID:     project.ID,
		Goal:          "Ship the filesystem store",
		Active:        []string{"WP2 adapter", "multi-line item\nwith a second line"},
		Blocked:       []string{"awaiting review"},
		NextAction:    "run the targeted test suite",
		OpenQuestions: []string{"should notes support edits?"},
		FreshUntil:    freshUntil,
	}
	revision, err := store.PutState(ctx, state)
	if err != nil {
		t.Fatalf("PutState: %v", err)
	}
	if revision.Kind != domain.SyncKindGit || revision.Value == "" {
		t.Fatalf("revision = %+v", revision)
	}

	got, err := store.GetState(ctx, project.ID)
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if got.ProjectID != state.ProjectID || got.Goal != state.Goal || got.NextAction != state.NextAction {
		t.Fatalf("state text mismatch: %+v", got)
	}
	if !reflect.DeepEqual(got.Active, state.Active) {
		t.Fatalf("active = %#v, want %#v", got.Active, state.Active)
	}
	if !reflect.DeepEqual(got.Blocked, state.Blocked) {
		t.Fatalf("blocked = %#v, want %#v", got.Blocked, state.Blocked)
	}
	if !reflect.DeepEqual(got.OpenQuestions, state.OpenQuestions) {
		t.Fatalf("open questions = %#v, want %#v", got.OpenQuestions, state.OpenQuestions)
	}
	if !got.FreshUntil.Equal(freshUntil) {
		t.Fatalf("FreshUntil = %v, want %v", got.FreshUntil, freshUntil)
	}
	if !got.UpdatedAt.Equal(testClock) {
		t.Fatalf("UpdatedAt = %v, want %v", got.UpdatedAt, testClock)
	}

	statePath := filepath.Join(store.Root(), "projects", project.ID, "state.md")
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var frontmatter stateFrontmatter
	if _, err := markdown.Parse(raw, &frontmatter); err != nil {
		t.Fatalf("parse state file: %v", err)
	}
	if frontmatter.ProjectID != project.ID || frontmatter.Kind != "state" || frontmatter.SchemaVersion != SchemaVersion {
		t.Fatalf("state frontmatter = %+v", frontmatter)
	}

	if _, err := store.GetState(ctx, "ghost"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetState(ghost) error = %v, want ErrNotFound", err)
	}
	if _, err := store.PutState(ctx, domain.State{ProjectID: "ghost"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("PutState(ghost) error = %v, want ErrNotFound", err)
	}
}

func TestStateRejectsIncompatibleFrontmatter(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	path := filepath.Join(store.Root(), "projects", project.ID, "state.md")
	writeState := func(projectID, kind string, schema int) {
		t.Helper()
		frontmatter := stateFrontmatter{Frontmatter: markdown.Frontmatter{
			ProjectID:     projectID,
			Kind:          kind,
			UpdatedAt:     testClock,
			SchemaVersion: schema,
		}}
		raw, err := markdown.Render(frontmatter, "# State\n\n## Goal\n\ntext\n")
		if err != nil {
			t.Fatalf("render state: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir state dir: %v", err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatalf("write state: %v", err)
		}
	}

	writeState(project.ID, "state", SchemaVersion+1)
	if _, err := store.GetState(ctx, project.ID); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("future schema error = %v, want ErrValidation", err)
	}

	writeState(project.ID, "decision", SchemaVersion)
	if _, err := store.GetState(ctx, project.ID); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("kind mismatch error = %v, want ErrValidation", err)
	}

	writeState("other-project", "state", SchemaVersion)
	if _, err := store.GetState(ctx, project.ID); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("project mismatch error = %v, want ErrValidation", err)
	}
}
