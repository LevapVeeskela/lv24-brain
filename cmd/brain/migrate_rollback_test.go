package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/migrate"
)

const rbCLIState = `---
project_id: lv24
updated_at: 2026-11-05 03:55:30+03:00
branch: develop
head: 8acf1a0fb
status: active
permalink: company/projects/lv24/state
---

# lv24 — state

## Goal

Городской портал.

## Next action

Решение владельца.
`

func rbCLIWrite(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", rel, err)
	}
}

// rbCLISnapshot maps every path below root (except the backups directory) to its content.
func rbCLISnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	if _, err := os.Stat(root); os.IsNotExist(err) {
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
			if rel == "backups" {
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

func rbCLIAssertSame(t *testing.T, want, got map[string]string) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("tree has %d entries, want %d; got %v", len(got), len(want), got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("%s differs after rollback", key)
		}
	}
}

func rbCLILegacy(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	rbCLIWrite(t, source, "projects/lv24/state.md", rbCLIState)
	return source
}

func rbCLIRun(t *testing.T, fn func(args []string, stdout, stderr *bytes.Buffer) int, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := fn(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func rbCLIMigrate(args []string, stdout, stderr *bytes.Buffer) int {
	return runMigrate(args, stdout, stderr)
}

func TestMigrateRollbackCLIRestoresTarget(t *testing.T) {
	cases := []struct {
		name      string
		withFiles bool
	}{
		{name: "existing registry and config", withFiles: true},
		{name: "fresh target", withFiles: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			source := rbCLILegacy(t)
			target := filepath.Join(t.TempDir(), "data")
			configArgs := []string{"--data-dir", target}
			if tc.withFiles {
				rbCLIWrite(t, target, "registry.json", "{\"version\":1,\"projects\":[]}\n")
				rbCLIWrite(t, target, "brain.yaml", "mode: local\n")
				configArgs = append(configArgs, "--config", filepath.Join(target, "brain.yaml"))
			}
			before := rbCLISnapshot(t, target)

			migrateArgs := append([]string{"--from", source}, configArgs...)
			if code, stdout, stderr := rbCLIRun(t, rbCLIMigrate, migrateArgs...); code != 0 {
				t.Fatalf("migrate exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if _, err := os.Stat(filepath.Join(target, "projects", "lv24", "state.md")); err != nil {
				t.Fatalf("migration did not create state.md: %v", err)
			}

			code, stdout, stderr := rbCLIRun(t, rbCLIMigrate, append([]string{"--rollback", "latest"}, configArgs...)...)
			if code != 0 {
				t.Fatalf("rollback exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if !strings.Contains(stdout, "skipped   0") {
				t.Fatalf("rollback report missing skipped count:\n%s", stdout)
			}
			rbCLIAssertSame(t, before, rbCLISnapshot(t, target))
		})
	}
}

func TestMigrateRollbackCLIRejectsFromCombination(t *testing.T) {
	code, _, stderr := rbCLIRun(t, rbCLIMigrate, "--from", t.TempDir(), "--rollback", "latest")
	if code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "cannot be combined with --from") {
		t.Fatalf("unexpected stderr: %s", stderr)
	}
}

func TestMigrateRollbackCLIKeepsEditedFileAndExitsOne(t *testing.T) {
	source := rbCLILegacy(t)
	target := filepath.Join(t.TempDir(), "data")
	if code, stdout, stderr := rbCLIRun(t, rbCLIMigrate, "--from", source, "--data-dir", target); code != 0 {
		t.Fatalf("migrate exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	rbCLIWrite(t, target, "projects/lv24/state.md", "edited by hand\n")

	code, stdout, stderr := rbCLIRun(t, rbCLIMigrate, "--rollback", "latest", "--data-dir", target)
	if code != 1 {
		t.Fatalf("rollback exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "modified since migration: projects/lv24/state.md") {
		t.Fatalf("modified file not reported:\n%s", stdout)
	}
	raw, err := os.ReadFile(filepath.Join(target, "projects", "lv24", "state.md"))
	if err != nil || string(raw) != "edited by hand\n" {
		t.Fatalf("edited file was not kept: %q, %v", raw, err)
	}
}

func TestMigrateRollbackCLIDryRunChangesNothing(t *testing.T) {
	source := rbCLILegacy(t)
	target := filepath.Join(t.TempDir(), "data")
	if code, stdout, stderr := rbCLIRun(t, rbCLIMigrate, "--from", source, "--data-dir", target); code != 0 {
		t.Fatalf("migrate exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	mid := rbCLISnapshot(t, target)

	code, stdout, stderr := rbCLIRun(t, rbCLIMigrate, "--rollback", "latest", "--data-dir", target, "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "would remove: projects/lv24/state.md") {
		t.Fatalf("dry run did not plan removal:\n%s", stdout)
	}
	rbCLIAssertSame(t, mid, rbCLISnapshot(t, target))
}

func TestMigrateRollbackCLIRejectsManifestFromOtherTarget(t *testing.T) {
	source := rbCLILegacy(t)
	target := filepath.Join(t.TempDir(), "data")
	if code, stdout, stderr := rbCLIRun(t, rbCLIMigrate, "--from", source, "--data-dir", target); code != 0 {
		t.Fatalf("migrate exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	manifest, err := migrate.FindLatestManifest(target)
	if err != nil {
		t.Fatalf("find manifest: %v", err)
	}
	other := filepath.Join(t.TempDir(), "other")
	code, _, stderr := rbCLIRun(t, rbCLIMigrate, "--rollback", manifest, "--data-dir", other)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "manifest belongs to") {
		t.Fatalf("unexpected stderr: %s", stderr)
	}
}
