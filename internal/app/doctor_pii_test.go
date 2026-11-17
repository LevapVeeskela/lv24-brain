package app_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

// TestDoctorFailsOnTrackedConnectorPII: connectors/*.json, connectors/runs and
// the legacy connectors/github and connectors/gitlab dumps must not publish
// e-mail addresses or mailbox user names. The check fails, names the files,
// ignores fixtures and changes nothing until the owner untracks them.
func TestDoctorFailsOnTrackedConnectorPII(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	dir := machines.dirs[0]
	offenders := map[string]string{
		"connectors/runs/run-1.json":      `{"id":"run-1","error":"alice@example.test"}`,
		"connectors/mail-state.json":      `{"username":"alice.smith"}`,
		"connectors/github/owner/repo.md": "Author: alice@example.test\n",
	}
	fixtures := map[string]string{
		"connectors/_test/connection.json":     `{"username":"fixture@example.test"}`,
		"connectors/github/_test/fixture.md":   "Author: fixture@example.test\n",
		"connectors/gitlab/testdata/sample.md": "author_email: fixture@example.test\n",
		"connectors/notes.md":                  "Author: alice@example.test\n",
	}
	for rel, content := range offenders {
		fixtures[rel] = content
	}
	for rel, content := range fixtures {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		machines.git(dir, "add", "-f", rel)
	}
	machines.git(dir, "commit", "-q", "-m", "track connector dumps")

	report, err := doctorFor(t, machines, 0).Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := findDoctorCheck(t, report, "tracked-pii")
	if check.Status != "warn" || check.Code != "tracked-pii" {
		t.Fatalf("tracked-pii check = %+v, want warn/tracked-pii", check)
	}
	for rel := range offenders {
		if !strings.Contains(check.Message, rel) {
			t.Fatalf("tracked-pii message %q misses %q", check.Message, rel)
		}
	}
	for _, rel := range []string{"_test", "testdata", "connectors/notes.md"} {
		if strings.Contains(check.Message, rel) {
			t.Fatalf("tracked-pii message %q names ignored fixture %q", check.Message, rel)
		}
	}
	// Report only: nothing is untracked or removed.
	for rel := range offenders {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("doctor removed %s: %v", rel, err)
		}
		if tracked := machines.git(dir, "ls-files", "--", rel); strings.TrimSpace(tracked) != rel {
			t.Fatalf("doctor untracked %s: ls-files = %q", rel, tracked)
		}
	}

	machines.git(dir, "rm", "--cached", "-r", "-q", "--", "connectors/runs", "connectors/mail-state.json", "connectors/github/owner")
	machines.git(dir, "commit", "-q", "-m", "untrack connector dumps")
	report, err = doctorFor(t, machines, 0).Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor after untrack: %v", err)
	}
	if check := findDoctorCheck(t, report, "tracked-pii"); check.Status != "ok" {
		t.Fatalf("tracked-pii after untrack = %+v", check)
	}
}

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
