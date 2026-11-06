package app_test

import (
	"context"
	"os"
	"path/filepath"
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
	// A parent of several projects is ambiguous; unrelated paths are unknown.
	for _, selector := range []string{root, filepath.Join(t.TempDir(), "elsewhere")} {
		if _, err := a.ResolveSelector(ctx, selector); err == nil {
			t.Errorf("ResolveSelector(%q) succeeded, want an error", selector)
		}
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
