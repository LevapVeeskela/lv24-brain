package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/journal"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
)

// newConflictFixture builds an app whose config carries its data directory, so
// the conflict-artifact scans of review and doctor are active.
func newConflictFixture(t *testing.T) (*app.App, string) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.User = "alice"
	cfg.DataDir = dataDir
	a := app.New(cfg, app.Dependencies{Store: store, Journal: journal.New(dataDir)})
	if _, err := a.Note(context.Background(), app.NoteInput{ProjectID: "p", Body: "seed", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	return a, dataDir
}

func writeArtifact(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func conflictKinds(findings []app.Finding) map[app.FindingKind]app.Finding {
	kinds := map[app.FindingKind]app.Finding{}
	for _, finding := range findings {
		kinds[finding.Kind] = finding
	}
	return kinds
}

func TestFindingsSurfaceConflictArtifacts(t *testing.T) {
	ctx := context.Background()
	a, dataDir := newConflictFixture(t)

	// Nothing written yet: no conflict findings.
	findings, err := a.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if got := conflictKinds(findings); got[app.FindingMergeConflict].Kind != "" || got[app.FindingRegistryConflict].Kind != "" {
		t.Fatalf("clean data reported conflict findings: %+v", findings)
	}

	writeArtifact(t, filepath.Join(dataDir, "projects", "p", "state-conflicts", "2026-11-17-goal.md"),
		"# Merge conflict: state goal\n\n- path: projects/p/state.md\n- ours: a\n- theirs: b\n")
	writeArtifact(t, filepath.Join(dataDir, "projects", "p", "notes", "merge-conflicts", "2026-11-17.md"),
		"# Merge conflict: notes 2026-11-17.md\n")
	writeArtifact(t, filepath.Join(dataDir, "merges", "2026-11-17T041222.000000000Z.json"),
		"{\n  \"version\": 1,\n  \"project_id\": \"p\",\n  \"status\": \"applied\"\n}\n")
	// An applied manifest records a finished consolidation and must not be
	// counted as pending; a pending one still waits for a decision.
	writeArtifact(t, filepath.Join(dataDir, "merges", "2026-11-17T041230.000000000Z.json"),
		"{\n  \"version\": 1,\n  \"project_id\": \"p\",\n  \"status\": \"pending\"\n}\n")
	// Another project's manifest and an unrelated registry conflict must not
	// leak into this project's findings.
	writeArtifact(t, filepath.Join(dataDir, "merges", "2026-11-17T041251.000000000Z.json"),
		"{\n  \"version\": 1,\n  \"project_id\": \"q\",\n  \"status\": \"applied\"\n}\n")
	writeArtifact(t, filepath.Join(dataDir, "registry-conflicts", "registry-1.md"),
		"# Merge conflict: registry path-/shared\n\n- ours: beta\n- theirs: p\n")
	writeArtifact(t, filepath.Join(dataDir, "registry-conflicts", "registry-2.md"),
		"# Merge conflict: registry alias-web\n\n- ours: other\n- theirs: stranger\n")

	findings, err = a.Findings(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	got := conflictKinds(findings)
	merge := got[app.FindingMergeConflict]
	if merge.Kind == "" {
		t.Fatalf("no merge-conflict finding in %+v", findings)
	}
	if merge.Severity != app.SeverityWarn {
		t.Fatalf("merge-conflict severity = %q, want warn", merge.Severity)
	}
	// state artifact + notes artifact + the pending manifest (the applied
	// manifests and the unrelated registry file are excluded).
	if !strings.Contains(merge.Message, "3 merge conflict artifacts") {
		t.Fatalf("merge-conflict message = %q, want a count of 3", merge.Message)
	}
	if strings.Contains(merge.Message, "041222") || strings.Contains(merge.Message, "041251") {
		t.Fatalf("merge-conflict message = %q, want applied manifests excluded", merge.Message)
	}
	if !strings.Contains(merge.Evidence, "state-conflicts") {
		t.Fatalf("merge-conflict evidence = %q, want a state-conflicts path", merge.Evidence)
	}
	registry := got[app.FindingRegistryConflict]
	if registry.Kind == "" {
		t.Fatalf("no registry-conflict finding in %+v", findings)
	}
	if registry.Severity != app.SeverityWarn {
		t.Fatalf("registry-conflict severity = %q, want warn", registry.Severity)
	}
	if !strings.Contains(registry.Message, "1 registry conflict") {
		t.Fatalf("registry-conflict message = %q, want only the artifact naming p", registry.Message)
	}
	if len(registry.Next) == 0 || !strings.Contains(registry.Next, "registry-conflicts") {
		t.Fatalf("registry-conflict next = %q, want a pointer at registry-conflicts/", registry.Next)
	}
}

func TestDoctorCountsPendingConflictArtifacts(t *testing.T) {
	ctx := context.Background()
	a, dataDir := newConflictFixture(t)

	findCheck := func() app.DoctorCheck {
		t.Helper()
		report, err := a.Doctor(ctx, app.DoctorOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, check := range report.Checks {
			if check.Name == "conflicts" {
				return check
			}
		}
		t.Fatal("no conflicts check")
		return app.DoctorCheck{}
	}
	if check := findCheck(); check.Status != "ok" || !strings.Contains(check.Message, "no pending conflict artifacts") {
		t.Fatalf("clean conflicts check = %+v", check)
	}

	// An applied manifest is the audit of a finished consolidation: it must
	// not count as pending, while a status-less (pending) one must.
	writeArtifact(t, filepath.Join(dataDir, "merges", "2026-11-17T041200.000000000Z.json"),
		"{\n  \"version\": 1,\n  \"project_id\": \"p\",\n  \"status\": \"applied\"\n}\n")
	if check := findCheck(); check.Status != "ok" {
		t.Fatalf("applied manifest counted as pending: %+v", check)
	}

	writeArtifact(t, filepath.Join(dataDir, "projects", "p", "state-conflicts", "2026-11-17-goal.md"), "# conflict\n")
	writeArtifact(t, filepath.Join(dataDir, "projects", "p", "notes", "merge-conflicts", "2026-11-17.md"), "# conflict\n")
	writeArtifact(t, filepath.Join(dataDir, "registry-conflicts", "registry-1.md"), "# conflict\n")
	writeArtifact(t, filepath.Join(dataDir, "merges", "2026-11-17T041222.000000000Z.json"),
		"{\n  \"version\": 1,\n  \"project_id\": \"p\"\n}\n")

	check := findCheck()
	if check.Status != "warn" || check.Code != "pending-conflicts" {
		t.Fatalf("conflicts check = %+v, want warn/pending-conflicts", check)
	}
	for _, want := range []string{"4 conflict artifacts pending", "2 state/notes", "1 registry", "1 merge manifest"} {
		if !strings.Contains(check.Message, want) {
			t.Fatalf("conflicts message %q misses %q", check.Message, want)
		}
	}
}

// The lv24-ytk variants: one repository registered under two project ids stays
// split knowledge, so doctor warns and names both ids.
func TestDoctorWarnsRepositoryUnderTwoIDs(t *testing.T) {
	ctx := context.Background()
	a, _ := newProjectTestApp(t)

	check := func() app.DoctorCheck {
		t.Helper()
		report, err := a.Doctor(ctx, app.DoctorOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range report.Checks {
			if item.Name == "duplicate-repo" {
				return item
			}
		}
		t.Fatal("no duplicate-repo check")
		return app.DoctorCheck{}
	}

	if _, err := a.SaveProject(ctx, app.ProjectInput{
		ID:      "lv24-ytk",
		RepoURL: projectValue("https://example.test/ytk.git"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := check(); got.Status != "ok" {
		t.Fatalf("single-owner repository = %+v, want ok", got)
	}

	if _, err := a.SaveProject(ctx, app.ProjectInput{
		ID:      "lv24-ytk-verify-typecheck",
		RepoURL: projectValue("git@example.test:ytk"),
	}); err != nil {
		t.Fatal(err)
	}
	got := check()
	if got.Status != "warn" || got.Code != "duplicate-repo" {
		t.Fatalf("duplicate-repo check = %+v, want warn/duplicate-repo", got)
	}
	for _, want := range []string{"example.test/ytk", "lv24-ytk", "lv24-ytk-verify-typecheck"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("duplicate-repo message %q misses %q", got.Message, want)
		}
	}
}
