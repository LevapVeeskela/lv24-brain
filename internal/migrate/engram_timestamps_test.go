package migrate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/migrate"
)

// engramNotesOn is the set of planned note files for one project and day.
func engramNotesOn(t *testing.T, result migrate.Result, project, day string) []migrate.FileResult {
	t.Helper()
	prefix := "projects/" + project + "/notes/engram-" + day
	files := []migrate.FileResult{}
	for _, file := range result.Files {
		if strings.HasPrefix(file.TargetPath, prefix) && file.Kind == "note" {
			files = append(files, file)
		}
	}
	return files
}

func TestEngramKeepsPerNoteUpdatedAt(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	result := applyFixture(t, source, target, nil)
	store := registerFixtureProjects(t, target, result.Projects)
	ctx := context.Background()

	files := engramNotesOn(t, result, "lv24", "2026-11-07")
	if len(files) != 3 {
		t.Fatalf("2026-11-07 note files = %d (%+v), want one per updated_at", len(files), files)
	}

	notes, err := store.ListNotes(ctx, "lv24")
	if err != nil {
		t.Fatalf("list notes: %v", err)
	}
	want := map[string]time.Time{
		"Relation source":     time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC),
		"Relation target":     time.Date(2026, 11, 7, 11, 0, 0, 0, time.UTC),
		"Relation target two": time.Date(2026, 11, 7, 11, 30, 0, 0, time.UTC),
	}
	for title, updatedAt := range want {
		note := findNote(t, notes, title)
		if !note.UpdatedAt.Equal(updatedAt) {
			t.Fatalf("%s updated_at = %v, want %v (the file maximum must not leak)", title, note.UpdatedAt, updatedAt)
		}
	}

	raw, err := os.ReadFile(filepath.Join(target, "projects", "lv24", "notes", "engram-2026-11-07-100000.md"))
	if err != nil {
		t.Fatalf("read per-timestamp note file: %v", err)
	}
	if !strings.Contains(string(raw), `"updated_at":"2026-11-07T10:00:00Z"`) {
		t.Fatalf("note marker does not carry its own updated_at:\n%s", raw)
	}
}

// legacyDailyFile merges the per-timestamp files of one day into the daily file
// the old renderer wrote: one file whose frontmatter holds the maximum
// updated_at. The per-note timestamps are then only recoverable from the source.
func legacyDailyFile(t *testing.T, target, project, day string, stamp time.Time) {
	t.Helper()
	dir := filepath.Join(target, "projects", project, "notes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read notes dir: %v", err)
	}
	frontmatter := ""
	body := strings.Builder{}
	first := true
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "engram-"+day) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		parts := strings.SplitN(string(raw), "---\n", 3)
		if len(parts) != 3 {
			t.Fatalf("%s has no frontmatter:\n%s", entry.Name(), raw)
		}
		if first {
			frontmatter = parts[1]
			first = false
		}
		body.WriteString(parts[2])
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			t.Fatalf("remove %s: %v", entry.Name(), err)
		}
	}
	if first {
		t.Fatalf("no note files for %s", day)
	}
	lines := []string{}
	for _, line := range strings.Split(strings.TrimRight(frontmatter, "\n"), "\n") {
		if strings.HasPrefix(line, "updated_at:") {
			continue
		}
		lines = append(lines, line)
	}
	lines = append(lines[:1], append([]string{"updated_at: " + stamp.UTC().Format("2006-01-02T15:04:05Z")}, lines[1:]...)...)
	rendered := "---\n" + strings.Join(lines, "\n") + "\n---\n" + body.String()
	if err := os.WriteFile(filepath.Join(dir, "engram-"+day+".md"), []byte(rendered), 0o644); err != nil {
		t.Fatalf("write legacy daily file: %v", err)
	}
}

func TestEngramRepairKeepsFilesWithOwnerEdits(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	applyFixture(t, source, target, nil)
	legacyDailyFile(t, target, "lv24", "2026-11-07", time.Date(2026, 11, 7, 11, 30, 0, 0, time.UTC))
	legacy := filepath.Join(target, "projects", "lv24", "notes", "engram-2026-11-07.md")
	raw, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("read legacy file: %v", err)
	}
	if err := os.WriteFile(legacy, append(raw, []byte("\n## Owner note\n\nmine\n")...), 0o644); err != nil {
		t.Fatalf("add owner note: %v", err)
	}

	repaired, err := runFixture(t, source, target, func(options *migrate.EngramOptions) {
		options.DryRun = false
		options.RepairTimestamps = true
	})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if repaired.Removed != 0 {
		t.Fatalf("repair set aside a file with an owner edit: %+v", repaired.Files)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("owner file is gone: %v", err)
	}
	if _, err := os.Stat(legacy + ".replaced"); !os.IsNotExist(err) {
		t.Fatalf("owner file was renamed aside: %v", err)
	}
	if !strings.Contains(strings.Join(repaired.Warnings, "\n"), "engram-2026-11-07.md") {
		t.Fatalf("repair did not warn about the kept file: %+v", repaired.Warnings)
	}

	// No note may appear twice: the blocked day keeps only the legacy file.
	store := registerFixtureProjects(t, target, repaired.Projects)
	notes, err := store.ListNotes(context.Background(), "lv24")
	if err != nil {
		t.Fatalf("list notes: %v", err)
	}
	seen := map[string]int{}
	for _, note := range notes {
		seen[note.ID]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Fatalf("note %s was imported %d times", id, count)
		}
	}
}

func TestEngramPlainRerunRefusesLegacyDailyLayout(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	applyFixture(t, source, target, nil)
	legacyDailyFile(t, target, "lv24", "2026-11-07", time.Date(2026, 11, 7, 11, 30, 0, 0, time.UTC))
	legacy := filepath.Join(target, "projects", "lv24", "notes", "engram-2026-11-07.md")
	before, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("read legacy file: %v", err)
	}

	result, err := runFixture(t, source, target, func(options *migrate.EngramOptions) { options.DryRun = false })
	var aborted *migrate.AbortedError
	if !errors.As(err, &aborted) || aborted.Conflicts != 1 {
		t.Fatalf("plain rerun error = %v (result %+v), want one legacy-layout conflict", err, result)
	}
	if !strings.Contains(err.Error(), "--repair-timestamps") {
		t.Fatalf("abort message does not point at the repair: %v", err)
	}
	if after, readErr := os.ReadFile(legacy); readErr != nil || string(after) != string(before) {
		t.Fatalf("aborted rerun touched the legacy file: %v", readErr)
	}

	skipped, err := runFixture(t, source, target, func(options *migrate.EngramOptions) {
		options.DryRun = false
		options.SkipConflicts = true
	})
	if err == nil {
		t.Fatalf("skip-conflicts must still report the conflict: %+v", skipped)
	}
	store := registerFixtureProjects(t, target, skipped.Projects)
	notes, listErr := store.ListNotes(context.Background(), "lv24")
	if listErr != nil {
		t.Fatalf("list notes: %v", listErr)
	}
	seen := map[string]int{}
	for _, note := range notes {
		seen[note.ID]++
		if seen[note.ID] > 1 {
			t.Fatalf("note %s was imported twice", note.ID)
		}
	}
}

func TestEngramRepairTimestampsRestoresPerNoteTimes(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	first := applyFixture(t, source, target, nil)
	legacyDailyFile(t, target, "lv24", "2026-11-07", time.Date(2026, 11, 7, 11, 30, 0, 0, time.UTC))

	store := registerFixtureProjects(t, target, first.Projects)
	afterLegacy, err := store.ListNotes(context.Background(), "lv24")
	if err != nil {
		t.Fatalf("list notes: %v", err)
	}
	if got := findNote(t, afterLegacy, "Relation source").UpdatedAt; !got.Equal(time.Date(2026, 11, 7, 11, 30, 0, 0, time.UTC)) {
		t.Fatalf("fixture is wrong: legacy file should leak the maximum, got %v", got)
	}

	repaired, err := runFixture(t, source, target, func(options *migrate.EngramOptions) {
		options.DryRun = false
		options.RepairTimestamps = true
	})
	if err != nil {
		t.Fatalf("repair: %v\nfiles: %+v", err, repaired.Files)
	}
	if repaired.Removed != 1 {
		t.Fatalf("repair removed=%d, want 1 legacy file: %+v", repaired.Removed, repaired.Files)
	}
	if _, err := os.Stat(filepath.Join(target, "projects", "lv24", "notes", "engram-2026-11-07.md.replaced")); err != nil {
		t.Fatalf("legacy file was not kept aside: %v", err)
	}

	notes, err := store.ListNotes(context.Background(), "lv24")
	if err != nil {
		t.Fatalf("list notes after repair: %v", err)
	}
	want := map[string]time.Time{
		"Relation source":     time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC),
		"Relation target":     time.Date(2026, 11, 7, 11, 0, 0, 0, time.UTC),
		"Relation target two": time.Date(2026, 11, 7, 11, 30, 0, 0, time.UTC),
	}
	for title, updatedAt := range want {
		if got := findNote(t, notes, title).UpdatedAt; !got.Equal(updatedAt) {
			t.Fatalf("%s updated_at after repair = %v, want %v", title, got, updatedAt)
		}
	}

	// A second repair is a no-op: nothing is removed and the tree is stable.
	second, err := runFixture(t, source, target, func(options *migrate.EngramOptions) {
		options.DryRun = false
		options.RepairTimestamps = true
	})
	if err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if second.Removed != 0 || second.Created != 0 || second.Conflicts != 0 {
		t.Fatalf("second repair created=%d removed=%d conflicts=%d", second.Created, second.Removed, second.Conflicts)
	}
	if second.TreeHashBefore != second.TreeHashAfter {
		t.Fatalf("idempotent repair changed the tree: %s -> %s", second.TreeHashBefore, second.TreeHashAfter)
	}
}

func TestEngramConflictAbortsBeforeWriting(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	planned, err := runFixture(t, source, target, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(planned.Files) < 3 {
		t.Fatalf("plan has %d files", len(planned.Files))
	}
	conflictRel := planned.Files[0].TargetPath
	conflictPath := filepath.Join(target, filepath.FromSlash(conflictRel))
	if err := os.MkdirAll(filepath.Dir(conflictPath), 0o755); err != nil {
		t.Fatalf("prepare conflict: %v", err)
	}
	if err := os.WriteFile(conflictPath, []byte("owner edit\n"), 0o644); err != nil {
		t.Fatalf("write conflict: %v", err)
	}
	before := snapshotTree(t, target)

	result, err := runFixture(t, source, target, func(options *migrate.EngramOptions) { options.DryRun = false })
	var aborted *migrate.AbortedError
	if !errors.As(err, &aborted) || aborted.Conflicts != 1 {
		t.Fatalf("apply error = %v (result %+v), want AbortedError with 1 conflict", err, result)
	}
	if !strings.Contains(err.Error(), "aborted before writing: 1 conflict") {
		t.Fatalf("abort message does not name the count: %v", err)
	}
	if result.Created != 0 || result.Conflicts != 1 {
		t.Fatalf("aborted result created=%d conflicts=%d", result.Created, result.Conflicts)
	}
	if after := snapshotTree(t, target); len(after) != len(before) {
		t.Fatalf("aborted run wrote %d files", len(after)-len(before))
	}

	skipped, err := runFixture(t, source, target, func(options *migrate.EngramOptions) {
		options.DryRun = false
		options.SkipConflicts = true
	})
	var incomplete *migrate.IncompleteError
	if !errors.As(err, &incomplete) || skipped.Conflicts != 1 {
		t.Fatalf("skip-conflicts error = %v (result %+v)", err, skipped)
	}
	if skipped.Created != len(planned.Files)-1 {
		t.Fatalf("skip-conflicts created=%d, want %d", skipped.Created, len(planned.Files)-1)
	}
	if raw, readErr := os.ReadFile(conflictPath); readErr != nil || string(raw) != "owner edit\n" {
		t.Fatalf("skip-conflicts overwrote the conflict: %q %v", raw, readErr)
	}
}

func TestEngramDryRunHashesOnlyProjects(t *testing.T) {
	source := writeEngramFixture(t)
	target := filepath.Join(t.TempDir(), "data")
	indexFile := filepath.Join(target, "index", "search.db")
	if err := os.MkdirAll(filepath.Dir(indexFile), 0o755); err != nil {
		t.Fatalf("prepare index: %v", err)
	}
	if err := os.WriteFile(indexFile, []byte("index bytes"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	first, err := runFixture(t, source, target, nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if err := os.WriteFile(indexFile, []byte("index bytes rewritten"), 0o644); err != nil {
		t.Fatalf("rewrite index: %v", err)
	}
	second, err := runFixture(t, source, target, nil)
	if err != nil {
		t.Fatalf("second dry run: %v", err)
	}
	if first.TreeHashBefore != second.TreeHashBefore || second.TreeHashBefore != second.TreeHashAfter {
		t.Fatalf("dry-run hash depends on index/: %s %s %s",
			first.TreeHashBefore, second.TreeHashBefore, second.TreeHashAfter)
	}
	if raw, err := os.ReadFile(indexFile); err != nil || string(raw) != "index bytes rewritten" {
		t.Fatalf("dry run rewrote index: %q %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(target, "backups")); !os.IsNotExist(err) {
		t.Fatalf("dry run created backups/: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "projects")); !os.IsNotExist(err) {
		t.Fatalf("dry run created projects/: %v", err)
	}
}
