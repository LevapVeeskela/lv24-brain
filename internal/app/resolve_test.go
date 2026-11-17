package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestResolveSelectorMatchesLegacyRules(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := fsstore.New(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	harvest := filepath.Join(root, "lv24-harvest", "main")
	city := filepath.Join(root, "lv24")
	// Registered checkouts and the subdirectories resolved below exist on this
	// machine; a missing path under a project is rejected (see the test below).
	for _, dir := range []string{
		city,
		filepath.Join(city, "web", "src"),
		filepath.Join(city, "services", "nested", "cmd"),
		harvest,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range []domain.Project{
		{ID: "lv24", Name: "lv24", Kind: domain.ProjectKindApp, Paths: []string{city}, Aliases: []string{"city"}},
		{ID: "lv24-harvest", Name: "harvest", Kind: domain.ProjectKindApp, Paths: []string{harvest}},
		{ID: "nested", Name: "nested", Kind: domain.ProjectKindApp, Paths: []string{filepath.Join(city, "services", "nested")}},
	} {
		if err := store.PutProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	a := app.New(cfg, app.Dependencies{Store: store})

	cases := []struct {
		selector, want string
	}{
		{"lv24", "lv24"},
		{"CITY", "lv24"},
		{city, "lv24"},
		{filepath.Join(city, "web", "src"), "lv24"},
		// Longest registered prefix wins over its parent project.
		{filepath.Join(city, "services", "nested", "cmd"), "nested"},
		// A parent of exactly one registered project resolves to it.
		{filepath.Join(root, "lv24-harvest"), "lv24-harvest"},
	}
	for _, tc := range cases {
		got, err := a.ResolveSelector(ctx, tc.selector)
		if err != nil || got.Project.ID != tc.want {
			t.Errorf("ResolveSelector(%q) = %q, %v; want %q", tc.selector, got.Project.ID, err, tc.want)
		}
	}
	// A parent of several projects is a workspace: an error that carries the
	// projects, rendered as an overview. Unrelated paths are unknown.
	_, err = a.ResolveSelector(ctx, root)
	workspace, ok := app.AsWorkspace(err)
	if !ok || len(workspace.Projects) < 2 || !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ResolveSelector(workspace root) = %v, want a WorkspaceError", err)
	}
	overview, err := a.WorkspaceOverview(ctx, workspace)
	if err != nil || !strings.Contains(overview, "- lv24 ") || !strings.Contains(overview, "- lv24-harvest ") {
		t.Fatalf("overview = %q, %v", overview, err)
	}
	if _, err := a.ResolveSelector(ctx, filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Error("ResolveSelector(unrelated path) succeeded, want an error")
	} else if _, ok := app.AsWorkspace(err); ok {
		t.Error("unrelated path reported as a workspace")
	}

	// With several projects and no --project, writes use the working
	// directory's project.
	if err := os.MkdirAll(harvest, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(harvest)
	note, err := a.Note(ctx, app.NoteInput{Body: "from the harvest checkout"})
	if err != nil || note.Note.ProjectID != "lv24-harvest" {
		t.Fatalf("Note without project = %+v, %v", note.Note, err)
	}
}

func TestResolveSelectorRejectsMissingPathUnderProject(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := fsstore.New(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	// A checkout that only exists on another machine: the exact registered
	// path still resolves, so cross-machine selectors keep working.
	otherMachine := filepath.Join(root, "other-machine")
	if err := store.PutProject(ctx, domain.Project{
		ID: "alpha", Name: "alpha", Kind: domain.ProjectKindApp, Paths: []string{checkout},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutProject(ctx, domain.Project{
		ID: "beta", Name: "beta", Kind: domain.ProjectKindApp, Paths: []string{otherMachine},
	}); err != nil {
		t.Fatal(err)
	}
	a := app.New(config.Default(), app.Dependencies{Store: store})

	missing := filepath.Join(checkout, "nope")
	if got, err := a.ResolveSelector(ctx, missing); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ResolveSelector(%q) = %+v, %v; want a validation error", missing, got, err)
	}
	if got, err := a.ResolveSelector(ctx, otherMachine); err != nil || got.Project.ID != "beta" {
		t.Fatalf("exact registered path = %+v, %v; want beta", got, err)
	}
	sub := filepath.Join(checkout, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := a.ResolveSelector(ctx, sub); err != nil || got.Project.ID != "alpha" {
		t.Fatalf("existing subdirectory = %+v, %v; want alpha", got, err)
	}
}

func TestForeignOSPathsAreIgnored(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := fsstore.New(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(root, "work", "alpha")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	// A path that is absolute on the other operating system only.
	foreign := `C:\0project\beta`
	if filepath.IsAbs(foreign) {
		foreign = "/Users/someone/Projects/beta"
	}
	for _, project := range []domain.Project{
		{ID: "alpha", Name: "alpha", Kind: domain.ProjectKindApp, Paths: []string{foreign + "-alpha", local}},
		{ID: "beta", Name: "beta", Kind: domain.ProjectKindApp, Paths: []string{foreign}},
		{ID: "gamma", Name: "gamma", Kind: domain.ProjectKindApp, Paths: []string{foreign + "-gamma"}},
	} {
		if err := store.PutProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	a := app.New(config.Default(), app.Dependencies{Store: store})
	// From inside the local checkout the foreign paths must not make the
	// directory look like a parent of beta and gamma.
	got, err := a.ResolveSelector(ctx, local)
	if err != nil || got.Project.ID != "alpha" || got.Path != local {
		t.Fatalf("resolve local = %+v, %v", got, err)
	}
	// By id, the local checkout wins over the foreign first entry.
	got, err = a.ResolveSelector(ctx, "alpha")
	if err != nil || got.Path != local {
		t.Fatalf("resolve id = %+v, %v", got, err)
	}
	if paths := app.LocalPaths([]string{foreign, local, ""}); len(paths) != 1 || paths[0] != local {
		t.Fatalf("LocalPaths = %v", paths)
	}
}
