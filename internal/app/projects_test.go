package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func newProjectTestApp(t *testing.T) (*app.App, *fsstore.Store) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	return app.New(cfg, app.Dependencies{Store: store}), store
}

func projectValue[T any](value T) *T {
	return &value
}

func TestSaveProjectCreatesMinimal(t *testing.T) {
	useCase, _ := newProjectTestApp(t)
	ctx := context.Background()

	project, err := useCase.SaveProject(ctx, app.ProjectInput{ID: "  demo  "})
	if err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	if project.ID != "demo" || project.Name != "demo" || project.Kind != domain.ProjectKindOther {
		t.Fatalf("project = %+v", project)
	}
	if project.CreatedAt.IsZero() || project.UpdatedAt.IsZero() {
		t.Fatalf("timestamps = %+v", project)
	}

	stored, err := useCase.GetProject(ctx, " demo ")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.ID != project.ID || stored.Name != project.Name || stored.Kind != project.Kind {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestSaveProjectCreatesWithMetadata(t *testing.T) {
	useCase, _ := newProjectTestApp(t)
	ctx := context.Background()

	project, err := useCase.SaveProject(ctx, app.ProjectInput{
		ID:            "demo",
		Name:          projectValue("Demo"),
		Paths:         projectValue([]string{"/src/demo"}),
		Aliases:       projectValue([]string{"d"}),
		Kind:          projectValue(domain.ProjectKindRepository),
		RepoURL:       projectValue("https://example.test/demo.git"),
		DefaultBranch: projectValue("main"),
	})
	if err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	if project.Name != "Demo" || project.Kind != domain.ProjectKindRepository {
		t.Fatalf("project = %+v", project)
	}
	if len(project.Paths) != 1 || project.Paths[0] != "/src/demo" {
		t.Fatalf("paths = %+v", project.Paths)
	}
	if len(project.Aliases) != 1 || project.Aliases[0] != "d" {
		t.Fatalf("aliases = %+v", project.Aliases)
	}
	if project.RepoURL != "https://example.test/demo.git" || project.DefaultBranch != "main" {
		t.Fatalf("repo metadata = %+v", project)
	}
}

func TestSaveProjectMergesOmittedFields(t *testing.T) {
	useCase, store := newProjectTestApp(t)
	ctx := context.Background()
	createdAt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	seed := domain.Project{
		ID:            "demo",
		Name:          "Demo",
		Paths:         []string{"/src/demo"},
		Aliases:       []string{"d"},
		Kind:          domain.ProjectKindRepository,
		RepoURL:       "https://example.test/demo.git",
		DefaultBranch: "main",
		CreatedAt:     createdAt,
	}
	if err := store.PutProject(ctx, seed); err != nil {
		t.Fatalf("PutProject: %v", err)
	}

	project, err := useCase.SaveProject(ctx, app.ProjectInput{
		ID:            "demo",
		DefaultBranch: projectValue("dev"),
	})
	if err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	if project.Name != "Demo" || project.Kind != domain.ProjectKindRepository {
		t.Fatalf("merge lost identity fields: %+v", project)
	}
	if len(project.Paths) != 1 || project.Paths[0] != "/src/demo" {
		t.Fatalf("merge lost paths: %+v", project.Paths)
	}
	if len(project.Aliases) != 1 || project.Aliases[0] != "d" {
		t.Fatalf("merge lost aliases: %+v", project.Aliases)
	}
	if project.RepoURL != "https://example.test/demo.git" || project.DefaultBranch != "dev" {
		t.Fatalf("merge lost repo metadata: %+v", project)
	}
	if !project.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt = %s, want %s", project.CreatedAt, createdAt)
	}
	if !project.UpdatedAt.After(createdAt) {
		t.Fatalf("UpdatedAt = %s, want after %s", project.UpdatedAt, createdAt)
	}
}

func TestSaveProjectClearsFields(t *testing.T) {
	useCase, _ := newProjectTestApp(t)
	ctx := context.Background()
	if _, err := useCase.SaveProject(ctx, app.ProjectInput{
		ID:            "demo",
		Name:          projectValue("Demo"),
		Paths:         projectValue([]string{"/src/demo"}),
		Aliases:       projectValue([]string{"d"}),
		Kind:          projectValue(domain.ProjectKindRepository),
		RepoURL:       projectValue("https://example.test/demo.git"),
		DefaultBranch: projectValue("main"),
	}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	project, err := useCase.SaveProject(ctx, app.ProjectInput{
		ID:            "demo",
		Paths:         projectValue([]string{}),
		Aliases:       projectValue([]string{}),
		RepoURL:       projectValue(""),
		DefaultBranch: projectValue(""),
	})
	if err != nil {
		t.Fatalf("SaveProject(clear): %v", err)
	}
	if len(project.Paths) != 0 || len(project.Aliases) != 0 {
		t.Fatalf("lists were not cleared: paths=%+v aliases=%+v", project.Paths, project.Aliases)
	}
	if project.RepoURL != "" || project.DefaultBranch != "" {
		t.Fatalf("text fields were not cleared: %+v", project)
	}
	if project.Name != "Demo" || project.Kind != domain.ProjectKindRepository {
		t.Fatalf("clear lost other fields: %+v", project)
	}
}

func TestSaveProjectRejectsInvalidKind(t *testing.T) {
	useCase, _ := newProjectTestApp(t)
	ctx := context.Background()

	_, err := useCase.SaveProject(ctx, app.ProjectInput{
		ID:   "demo",
		Kind: projectValue(domain.ProjectKind("bogus")),
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if _, getErr := useCase.GetProject(ctx, "demo"); !errors.Is(getErr, domain.ErrNotFound) {
		t.Fatalf("invalid project was stored: %v", getErr)
	}
}

func TestSaveProjectRequiresID(t *testing.T) {
	useCase, _ := newProjectTestApp(t)
	ctx := context.Background()

	for _, id := range []string{"", "   "} {
		if _, err := useCase.SaveProject(ctx, app.ProjectInput{ID: id}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("SaveProject(%q) err = %v, want ErrValidation", id, err)
		}
		if _, err := useCase.GetProject(ctx, id); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("GetProject(%q) err = %v, want ErrValidation", id, err)
		}
	}
}

func TestSaveProjectRejectsEmptyNameOnUpdate(t *testing.T) {
	useCase, _ := newProjectTestApp(t)
	ctx := context.Background()
	if _, err := useCase.SaveProject(ctx, app.ProjectInput{ID: "demo", Name: projectValue("Demo")}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	if _, err := useCase.SaveProject(ctx, app.ProjectInput{ID: "demo", Name: projectValue("  ")}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	stored, err := useCase.GetProject(ctx, "demo")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.Name != "Demo" {
		t.Fatalf("name = %q, want Demo", stored.Name)
	}
}

func TestSaveProjectDefaultsEmptyNameOnCreate(t *testing.T) {
	useCase, _ := newProjectTestApp(t)
	ctx := context.Background()

	project, err := useCase.SaveProject(ctx, app.ProjectInput{ID: "demo", Name: projectValue("  ")})
	if err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	if project.Name != "demo" {
		t.Fatalf("name = %q, want demo", project.Name)
	}
}

func TestListProjectsSortsByID(t *testing.T) {
	useCase, _ := newProjectTestApp(t)
	ctx := context.Background()
	for _, project := range []struct {
		id   string
		name string
	}{
		{id: "zeta", name: "Alpha Name"},
		{id: "alpha", name: "Zulu Name"},
		{id: "beta", name: "Mike Name"},
	} {
		if _, err := useCase.SaveProject(ctx, app.ProjectInput{ID: project.id, Name: projectValue(project.name)}); err != nil {
			t.Fatalf("SaveProject(%s): %v", project.id, err)
		}
	}

	projects, err := useCase.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	want := []string{"alpha", "beta", "zeta"}
	if len(projects) != len(want) {
		t.Fatalf("projects = %+v", projects)
	}
	for i, id := range want {
		if projects[i].ID != id {
			t.Fatalf("projects[%d].ID = %q, want %q (all: %+v)", i, projects[i].ID, id, projects)
		}
	}
}

func TestProjectOperationsRequireStore(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	useCase := app.New(cfg, app.Dependencies{})
	ctx := context.Background()

	if _, err := useCase.ListProjects(ctx); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("ListProjects err = %v, want ErrUnsupported", err)
	}
	if _, err := useCase.GetProject(ctx, "demo"); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("GetProject err = %v, want ErrUnsupported", err)
	}
	if _, err := useCase.SaveProject(ctx, app.ProjectInput{ID: "demo"}); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("SaveProject err = %v, want ErrUnsupported", err)
	}
}
