package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

// TestDoctorReportsTrackedLegacyPIIZones: mail, root github/gitlab and merges
// predate the publish policy. A tracked file there is a warning with the exact
// untrack command; the files stay on disk and untracking clears the finding.
func TestDoctorReportsTrackedLegacyPIIZones(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	dir := machines.dirs[0]
	files := []string{
		"mail/gmail/2026-11-05.md",
		"github/owner/repo.md",
		"gitlab/group/project.md",
		"merges/2026-11-16T041222.000000000Z.json",
		"connectors/gitlab/index.md",
	}
	for _, rel := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte("# legacy\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		machines.git(dir, "add", "-f", rel)
	}
	machines.git(dir, "commit", "-q", "-m", "track legacy zones")

	report, err := doctorFor(t, machines, 0).Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := findDoctorCheck(t, report, "pii-zone")
	for _, want := range []string{"tracked-pii-zone", "mail (1 files)", "merges (1 files)", "git rm --cached"} {
		if !strings.Contains(check.Code+check.Message, want) {
			t.Fatalf("pii-zone check = %+v, want %q", check, want)
		}
	}
	if check.Status == "ok" {
		t.Fatalf("pii-zone check = %+v, want a warning", check)
	}
	for _, rel := range files {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("doctor removed %s: %v", rel, err)
		}
	}

	machines.git(dir, "rm", "--cached", "-r", "-q", "--", "mail", "github", "gitlab", "merges", "connectors/gitlab")
	report, err = doctorFor(t, machines, 0).Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor after untrack: %v", err)
	}
	if check := findDoctorCheck(t, report, "pii-zone"); check.Status != "ok" {
		t.Fatalf("pii-zone after untrack = %+v", check)
	}
	for _, rel := range files {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("untrack removed %s: %v", rel, err)
		}
	}
}
