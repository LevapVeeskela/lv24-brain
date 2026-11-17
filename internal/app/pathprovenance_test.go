package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func newAdoptApp(t *testing.T, machine string) (*app.App, *fsstore.Store) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	cfg.MachineID = machine
	return app.New(cfg, app.Dependencies{Store: store}), store
}

func TestAdoptLocalPathsStampsOnlyLocalCheckouts(t *testing.T) {
	useCase, store := newAdoptApp(t, "machine-a")
	ctx := context.Background()
	local := t.TempDir()
	remote := filepath.Join(t.TempDir(), "not-here")

	// A registry written before provenance existed: paths, no owners.
	if err := store.PutProject(ctx, domain.Project{
		ID: "legacy", Name: "Legacy", Kind: domain.ProjectKindRepository,
		Paths: []string{local, remote},
	}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}

	plan, err := useCase.AdoptLocalPaths(ctx, false)
	if err != nil {
		t.Fatalf("AdoptLocalPaths plan: %v", err)
	}
	if plan.Applied || plan.Stamped != 1 || plan.Owned != 0 || plan.Missing != 1 {
		t.Fatalf("plan = %+v, want 1 stamped / 0 owned / 1 missing", plan)
	}
	stored, err := store.GetProject(ctx, "legacy")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if len(stored.PathProvenance) != 0 {
		t.Fatalf("dry run wrote provenance %+v", stored.PathProvenance)
	}

	// A stamp for a path that is not on this machine would be a guess.
	applied, err := useCase.AdoptLocalPaths(ctx, true)
	if err != nil {
		t.Fatalf("AdoptLocalPaths apply: %v", err)
	}
	if applied.Stamped != 1 || applied.Machine != "machine-a" {
		t.Fatalf("applied = %+v", applied)
	}
	stored, err = store.GetProject(ctx, "legacy")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.PathProvenance[local] != "machine-a" {
		t.Fatalf("provenance = %+v, want local path owned by machine-a", stored.PathProvenance)
	}
	if _, ok := stored.PathProvenance[remote]; ok {
		t.Fatalf("provenance = %+v, remote path must stay unowned", stored.PathProvenance)
	}

	// Idempotent: a second run owns the path and stamps nothing.
	again, err := useCase.AdoptLocalPaths(ctx, true)
	if err != nil {
		t.Fatalf("AdoptLocalPaths rerun: %v", err)
	}
	if again.Stamped != 0 || again.Owned != 1 || again.Missing != 1 {
		t.Fatalf("rerun = %+v, want 0 stamped / 1 owned / 1 missing", again)
	}
}

func TestAdoptLocalPathsKeepsAnotherMachinesOwner(t *testing.T) {
	useCase, store := newAdoptApp(t, "machine-b")
	ctx := context.Background()
	shared := t.TempDir()
	if err := store.PutProject(ctx, domain.Project{
		ID: "shared", Name: "Shared", Kind: domain.ProjectKindRepository,
		Paths:          []string{shared},
		PathProvenance: map[string]string{shared: "machine-a"},
	}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}

	report, err := useCase.AdoptLocalPaths(ctx, true)
	if err != nil {
		t.Fatalf("AdoptLocalPaths: %v", err)
	}
	if report.Stamped != 0 || report.Owned != 1 {
		t.Fatalf("report = %+v, want the existing owner kept", report)
	}
	stored, err := store.GetProject(ctx, "shared")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if stored.PathProvenance[shared] != "machine-a" {
		t.Fatalf("provenance = %+v, want machine-a kept", stored.PathProvenance)
	}
}
