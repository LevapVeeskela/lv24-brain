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

// retiredTestApp registers the replacement project "harvest" and retires a
// temp directory, so the tests never depend on a real workspace checkout.
func retiredTestApp(t *testing.T) (*app.App, *fsstore.Store, string, string) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	root := t.TempDir()
	retired := filepath.Join(root, "lv24-old-checkout")
	harvest := filepath.Join(root, "harvest", "main")
	if err := os.MkdirAll(harvest, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	cfg.RetiredPaths = []config.RetiredPath{{Path: retired, ReplacedBy: "harvest"}}
	a := app.New(cfg, app.Dependencies{Store: store})
	if _, err := a.SaveProject(context.Background(), app.ProjectInput{
		ID:    "harvest",
		Paths: &[]string{harvest},
	}); err != nil {
		t.Fatalf("SaveProject(harvest): %v", err)
	}
	return a, store, retired, harvest
}

func TestResolveSelectorRedirectsRetiredCheckout(t *testing.T) {
	ctx := context.Background()
	a, store, retired, harvest := retiredTestApp(t)
	before, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, selector := range []string{
		retired,
		filepath.Join(retired, "main"),
		filepath.Join(retired, "main", "ops"),
		// The former bare project name, with and without the lv24- prefix.
		"old-checkout",
		"lv24-old-checkout",
		"LV24-OLD-CHECKOUT",
	} {
		resolved, err := a.ResolveSelector(ctx, selector)
		if err != nil {
			t.Fatalf("ResolveSelector(%q): %v", selector, err)
		}
		if resolved.Project.ID != "harvest" || resolved.Path != harvest {
			t.Fatalf("ResolveSelector(%q) = %+v, want harvest at %s", selector, resolved, harvest)
		}
		if !strings.Contains(resolved.Warning, "retired") || !strings.Contains(resolved.Warning, "harvest") {
			t.Fatalf("ResolveSelector(%q) warning = %q", selector, resolved.Warning)
		}
	}

	after, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("resolution changed the registry: %d -> %d projects", len(before), len(after))
	}
	if _, err := store.GetProject(ctx, "old-checkout"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("resolution registered an old-checkout project: %v", err)
	}
}

func TestRetiredPathsAreNotBuiltIn(t *testing.T) {
	// Without a configured retired_paths entry the same directory is just an
	// unknown path: the engine ships no workspace-specific paths.
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	a := app.New(cfg, app.Dependencies{Store: store})

	retired := filepath.Join(t.TempDir(), "lv24-old-checkout")
	if _, err := a.ResolveSelector(ctx, retired); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ResolveSelector(unconfigured retired path) = %v, want ErrValidation", err)
	}
}

func TestResolveSelectorRetiredWithoutReplacementIsAnError(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(t.TempDir(), "lv24-old-checkout")
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	cfg.RetiredPaths = []config.RetiredPath{{Path: retired, ReplacedBy: "harvest"}}
	a := app.New(cfg, app.Dependencies{Store: store})

	_, err = a.ResolveSelector(ctx, filepath.Join(retired, "main"))
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("ResolveSelector(retired, unregistered replacement) = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "harvest") {
		t.Fatalf("error = %v, want the retired path and replacement named", err)
	}
	projects, listErr := store.ListProjects(ctx)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(projects) != 0 {
		t.Fatalf("failed resolution created projects: %+v", projects)
	}
}

func TestSaveProjectRefusesRetiredPath(t *testing.T) {
	ctx := context.Background()
	a, _, retired, _ := retiredTestApp(t)

	_, err := a.SaveProject(ctx, app.ProjectInput{ID: "legacy", Paths: &[]string{filepath.Join(retired, "main")}})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("SaveProject(retired path) = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "retired") || !strings.Contains(err.Error(), "harvest") {
		t.Fatalf("error = %v, want the retired path and replacement named", err)
	}
	if _, err := a.GetProject(ctx, "legacy"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("refused registration still created a project: %v", err)
	}

	// Adding a retired path to an existing project is refused too.
	other := filepath.Join(t.TempDir(), "other")
	if _, err := a.SaveProject(ctx, app.ProjectInput{ID: "other", Paths: &[]string{other}}); err != nil {
		t.Fatalf("SaveProject(other): %v", err)
	}
	if _, err := a.SaveProject(ctx, app.ProjectInput{ID: "other", Paths: &[]string{other, retired}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("SaveProject(other + retired) = %v, want ErrValidation", err)
	}
}

func TestSaveProjectKeepsAStoredRetiredPath(t *testing.T) {
	// A registry written before the guard existed keeps working: unrelated
	// edits are not blocked by the retired path it already stores.
	ctx := context.Background()
	a, store, retired, _ := retiredTestApp(t)
	if err := store.PutProject(ctx, domain.Project{
		ID:    "legacy",
		Name:  "legacy",
		Kind:  domain.ProjectKindRepository,
		Paths: []string{retired},
	}); err != nil {
		t.Fatal(err)
	}
	name := "Legacy (archived)"
	project, err := a.SaveProject(ctx, app.ProjectInput{ID: "legacy", Name: &name})
	if err != nil {
		t.Fatalf("SaveProject(rename legacy) = %v", err)
	}
	if project.Name != name || len(project.Paths) != 1 || project.Paths[0] != retired {
		t.Fatalf("project = %+v, want the rename and the stored path", project)
	}
}

func TestCreateNeverResurrectsTheRetiredProject(t *testing.T) {
	ctx := context.Background()
	a, store, _, _ := retiredTestApp(t)

	// Writing with the former name and --create lands in the replacement.
	note, err := a.Note(ctx, app.NoteInput{ProjectID: "old-checkout", CreateProject: true, Body: "redirected by the retirement guard"})
	if err != nil {
		t.Fatalf("Note(old-checkout, --create) = %v", err)
	}
	if note.Note.ProjectID != "harvest" {
		t.Fatalf("note project = %q, want harvest", note.Note.ProjectID)
	}
	if _, err := store.GetProject(ctx, "old-checkout"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("write created an old-checkout project: %v", err)
	}

	// Registering the former name directly is refused too.
	if _, err := a.SaveProject(ctx, app.ProjectInput{ID: "OLD-CHECKOUT"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("SaveProject(OLD-CHECKOUT) = %v, want ErrValidation", err)
	}
	if _, err := store.GetProject(ctx, "OLD-CHECKOUT"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("SaveProject created the retired name: %v", err)
	}
}

func TestDoctorReportsRetiredCheckout(t *testing.T) {
	ctx := context.Background()
	a, store, retired, _ := retiredTestApp(t)
	if err := os.MkdirAll(retired, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.PutProject(ctx, domain.Project{
		ID:    "legacy",
		Name:  "legacy",
		Kind:  domain.ProjectKindRepository,
		Paths: []string{retired},
	}); err != nil {
		t.Fatal(err)
	}

	report, err := a.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	var check *app.DoctorCheck
	for i := range report.Checks {
		if report.Checks[i].Name == "retired-paths" {
			check = &report.Checks[i]
		}
	}
	if check == nil {
		t.Fatalf("doctor has no retired-paths check: %+v", report.Checks)
	}
	if check.Status != "warn" {
		t.Fatalf("retired-paths status = %q, want warn (%s)", check.Status, check.Message)
	}
	for _, want := range []string{retired, "still exists", "registered to legacy", "replaced by harvest"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("retired-paths message = %q, want %q", check.Message, want)
		}
	}
}
