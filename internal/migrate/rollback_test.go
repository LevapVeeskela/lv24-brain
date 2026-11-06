package migrate

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var rollbackTestNow = time.Date(2026, 11, 6, 10, 30, 0, 0, time.UTC)

const (
	rollbackTestRegistry = "{\"version\":1,\"projects\":[]}\n"
	rollbackTestConfig   = "mode: local\n"
)

// snapshotTreeExceptBackups maps every path under root (slash-separated, relative)
// to its kind and content. The backups directory is excluded on purpose.
func snapshotTreeExceptBackups(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	if _, statErr := os.Stat(root); os.IsNotExist(statErr) {
		return snap
	}
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == backupsDir {
				return filepath.SkipDir
			}
			snap[rel] = "dir"
			return nil
		}
		raw, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		snap[rel] = "file:" + string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snap
}

func assertSameTree(t *testing.T, want, got map[string]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("tree has %d entries, want %d\ngot:  %v\nwant: %v", len(got), len(want), keysOf(got), keysOf(want))
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s differs: got %q want %q", key, got[key], value)
		}
	}
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

// rollbackTestMigrate runs a migration of the legacy fixture into target using the
// same BeginBackup / Run / Finish sequence as the CLI. between runs after the
// importer and before the manifest is written, to mimic the CLI registry and
// config updates. It returns the manifest path.
func rollbackTestMigrate(t *testing.T, target string, between func()) string {
	t.Helper()
	source := newLegacyFixture(t)
	configPath := filepath.Join(target, "brain.yaml")
	opts := Options{SourceDir: source, TargetDir: target}
	backup, err := BeginBackup(opts, configPath, rollbackTestNow)
	if err != nil {
		t.Fatalf("BeginBackup: %v", err)
	}
	result, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if between != nil {
		between()
	}
	manifestPath, err := backup.Finish(result)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return manifestPath
}

func newRollbackTestTarget(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "data")
	writeFixture(t, target, registryName, rollbackTestRegistry)
	writeFixture(t, target, "brain.yaml", rollbackTestConfig)
	return target
}

func TestRollbackRestoresTargetByteIdentical(t *testing.T) {
	target := newRollbackTestTarget(t)
	before := snapshotTreeExceptBackups(t, target)

	manifestPath := rollbackTestMigrate(t, target, func() {
		writeFixture(t, target, registryName, `{"version":1,"projects":[{"id":"lv24"}]}`+"\n")
		writeFixture(t, target, "brain.yaml", "mode: local\nschema_version: 1\n")
	})
	if mid := snapshotTreeExceptBackups(t, target); len(mid) <= len(before) {
		t.Fatalf("migration did not change the target: %d entries before, %d after", len(before), len(mid))
	}

	result, err := Rollback(context.Background(), RollbackOptions{ManifestPath: manifestPath})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if result.Skipped != 0 {
		t.Fatalf("Skipped = %d, want 0: %+v", result.Skipped, result.Entries)
	}
	assertSameTree(t, before, snapshotTreeExceptBackups(t, target))
	if !hasEntry(result, "registry.json", RollbackRestored) || !hasKindStatus(result, "config", RollbackRestored) {
		t.Fatalf("registry and config should be restored: %+v", result.Entries)
	}
}

func TestRollbackRemovesFilesWhenTargetWasEmpty(t *testing.T) {
	target := filepath.Join(t.TempDir(), "data")
	before := snapshotTreeExceptBackups(t, target)

	manifestPath := rollbackTestMigrate(t, target, func() {
		writeFixture(t, target, registryName, rollbackTestRegistry)
		writeFixture(t, target, "brain.yaml", rollbackTestConfig)
	})
	result, err := Rollback(context.Background(), RollbackOptions{ManifestPath: manifestPath})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if result.Skipped != 0 {
		t.Fatalf("Skipped = %d, want 0", result.Skipped)
	}
	assertSameTree(t, before, snapshotTreeExceptBackups(t, target))
}

func TestRollbackKeepsFileEditedAfterMigration(t *testing.T) {
	target := newRollbackTestTarget(t)
	manifestPath := rollbackTestMigrate(t, target, nil)

	edited := filepath.Join("projects", "lv24", "state.md")
	writeFixture(t, target, edited, "edited by hand\n")

	result, err := Rollback(context.Background(), RollbackOptions{ManifestPath: manifestPath})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1: %+v", result.Skipped, result.Entries)
	}
	if !hasEntry(result, "projects/lv24/state.md", RollbackModified) {
		t.Fatalf("edited file not reported as modified: %+v", result.Entries)
	}
	if got := readFixture(t, filepath.Join(target, edited)); got != "edited by hand\n" {
		t.Fatalf("edited file was changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(target, "projects", "lv24", "notes")); !os.IsNotExist(err) {
		t.Fatalf("untouched migrated notes directory should be removed: %v", err)
	}
	if got := readFixture(t, filepath.Join(target, registryName)); got != rollbackTestRegistry {
		t.Fatalf("registry not restored: %q", got)
	}
}

func TestRollbackDryRunChangesNothing(t *testing.T) {
	target := newRollbackTestTarget(t)
	manifestPath := rollbackTestMigrate(t, target, func() {
		writeFixture(t, target, registryName, `{"version":1,"projects":[{"id":"lv24"}]}`+"\n")
	})
	mid := snapshotTreeExceptBackups(t, target)

	result, err := Rollback(context.Background(), RollbackOptions{ManifestPath: manifestPath, DryRun: true})
	if err != nil {
		t.Fatalf("Rollback dry run: %v", err)
	}
	if !result.DryRun {
		t.Fatalf("DryRun flag not reported")
	}
	assertSameTree(t, mid, snapshotTreeExceptBackups(t, target))
	if !hasEntry(result, "registry.json", RollbackWouldRestore) {
		t.Fatalf("dry run should report a registry restore: %+v", result.Entries)
	}
	if !hasStatus(result, RollbackWouldRemove) {
		t.Fatalf("dry run should report removals: %+v", result.Entries)
	}
}

func TestRollbackRejectsUnsafeManifestPaths(t *testing.T) {
	bad := []string{
		"../evil.md",
		"/evil.md",
		"projects/../../evil.md",
		"projects/lv24/../../../evil.md",
		"projects/./lv24/state.md",
		"projects/lv24",
		"registry.json",
		`projects\lv24\state.md`,
		"C:/evil.md",
		"",
	}
	for _, rel := range bad {
		rel := rel
		t.Run(rel, func(t *testing.T) {
			parent := t.TempDir()
			target := filepath.Join(parent, "data")
			sentinel := filepath.Join(parent, "evil.md")
			writeFixture(t, parent, "evil.md", "keep me\n")
			manifestPath := rollbackTestMigrate(t, target, nil)
			manifest, err := LoadManifest(manifestPath)
			if err != nil {
				t.Fatalf("LoadManifest: %v", err)
			}
			if len(manifest.Files) == 0 {
				t.Fatalf("fixture created no files")
			}
			manifest.Files[0].Path = rel
			if err := WriteManifest(manifestPath, manifest); err != nil {
				t.Fatalf("WriteManifest: %v", err)
			}
			before := snapshotTreeExceptBackups(t, target)

			if _, err := Rollback(context.Background(), RollbackOptions{ManifestPath: manifestPath}); err == nil {
				t.Fatalf("Rollback accepted unsafe path %q", rel)
			}
			assertSameTree(t, before, snapshotTreeExceptBackups(t, target))
			if got := readFixture(t, sentinel); got != "keep me\n" {
				t.Fatalf("file outside target was touched: %q", got)
			}
		})
	}
}

func TestFindLatestManifestPicksNewestBackup(t *testing.T) {
	target := newRollbackTestTarget(t)
	first := rollbackTestMigrate(t, target, nil)
	later := filepath.Join(target, backupsDir, backupDirPrefix+"20261231T000000Z", manifestFile)
	writeFixture(t, target, filepath.Join(backupsDir, backupDirPrefix+"20261231T000000Z", manifestFile), "{}")
	got, err := FindLatestManifest(target)
	if err != nil {
		t.Fatalf("FindLatestManifest: %v", err)
	}
	if got != later || got == first {
		t.Fatalf("FindLatestManifest = %s, want %s", got, later)
	}
}

func TestBackupDirNamesDoNotCollideWithinASecond(t *testing.T) {
	target := newRollbackTestTarget(t)
	source := newLegacyFixture(t)
	opts := Options{SourceDir: source, TargetDir: target}
	configPath := filepath.Join(target, "brain.yaml")
	var manifests []string
	for i := 0; i < 3; i++ {
		backup, err := BeginBackup(opts, configPath, rollbackTestNow)
		if err != nil {
			t.Fatalf("BeginBackup %d: %v", i, err)
		}
		result, err := Run(context.Background(), opts)
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		manifestPath, err := backup.Finish(result)
		if err != nil {
			t.Fatalf("Finish %d: %v", i, err)
		}
		manifests = append(manifests, manifestPath)
	}
	if len(manifests) != 3 || manifests[0] == manifests[1] || manifests[1] == manifests[2] {
		t.Fatalf("backup manifests collided: %v", manifests)
	}
	latest, err := FindLatestManifest(target)
	if err != nil {
		t.Fatalf("FindLatestManifest: %v", err)
	}
	if latest != manifests[2] {
		t.Fatalf("latest = %s, want %s", latest, manifests[2])
	}
}

func hasEntry(result RollbackResult, path string, status RollbackStatus) bool {
	for _, entry := range result.Entries {
		if entry.Path == path && entry.Status == status {
			return true
		}
	}
	return false
}

func hasKindStatus(result RollbackResult, kind string, status RollbackStatus) bool {
	for _, entry := range result.Entries {
		if entry.Kind == kind && entry.Status == status {
			return true
		}
	}
	return false
}

func hasStatus(result RollbackResult, status RollbackStatus) bool {
	for _, entry := range result.Entries {
		if entry.Status == status {
			return true
		}
	}
	return false
}
