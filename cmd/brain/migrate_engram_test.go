package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const cliEngramSchema = `
CREATE TABLE observations (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sync_id TEXT,
  session_id TEXT NOT NULL,
  type TEXT NOT NULL,
  title TEXT NOT NULL,
  content TEXT NOT NULL,
  project TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now')),
  updated_at TEXT NOT NULL DEFAULT (datetime('now')),
  deleted_at TEXT
);
CREATE TABLE sessions (id TEXT PRIMARY KEY, project TEXT NOT NULL, directory TEXT NOT NULL, started_at TEXT NOT NULL);
CREATE TABLE user_prompts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sync_id TEXT,
  session_id TEXT NOT NULL,
  content TEXT NOT NULL,
  project TEXT,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
`

func writeCLIEngram(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "engram.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open engram fixture: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(cliEngramSchema); err != nil {
		t.Fatalf("create engram fixture: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, project, directory, started_at)
		VALUES ('ses-1', 'lv', 'C:\0project\lv24', '2026-11-01 00:00:00')`); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	rows := []struct {
		syncID, typ, title, content, project, created string
	}{
		{"obs-1", "discovery", "CLI note", "note body", "lv", "2026-11-01 10:00:00"},
		{"obs-2", "decision", "CLI decision", "**What**: body\n**Why**: reason", "lv24", "2026-11-02 10:00:00"},
		{"obs-3", "architecture", "Quarantined note", "quarantine body", "", "2026-11-03 10:00:00"},
	}
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO observations (sync_id, session_id, type, title, content, project, created_at, updated_at)
			VALUES (?, 'ses-1', ?, ?, ?, ?, ?, ?)`,
			row.syncID, row.typ, row.title, row.content, row.project, row.created, row.created); err != nil {
			t.Fatalf("insert observation %s: %v", row.syncID, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO user_prompts (sync_id, session_id, content, project, created_at)
		VALUES ('prompt-1', 'ses-1', 'cli prompt', 'lv24', '2026-11-01 11:00:00')`); err != nil {
		t.Fatalf("insert prompt: %v", err)
	}
	return path
}

func runCLIEngram(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runMigrate(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestMigrateEngramDryRunJSONWritesNothing(t *testing.T) {
	source := writeCLIEngram(t)
	target := filepath.Join(t.TempDir(), "data")
	code, stdout, stderr := runCLIEngram(t, "--from", "engram", "--db", source, "--data-dir", target, "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	var report struct {
		SourceSystem string `json:"source_system"`
		Created      int    `json:"created"`
		Quarantined  int    `json:"quarantined"`
		TreeBefore   string `json:"tree_hash_before"`
		TreeAfter    string `json:"tree_hash_after"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("parse JSON report: %v\n%s", err, stdout)
	}
	if report.SourceSystem != "engram" || report.Created == 0 || report.Quarantined != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.TreeBefore != report.TreeAfter {
		t.Fatalf("dry-run tree hash changed: %s -> %s", report.TreeBefore, report.TreeAfter)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry run created the target: %v", err)
	}
}

func TestMigrateEngramUsageValidation(t *testing.T) {
	cases := [][]string{
		{"--from", "engram", "--data-dir", t.TempDir()},
		{"--from", t.TempDir(), "--db", "engram.db"},
		{"--from", t.TempDir(), "--include-prompts"},
		{"--from", t.TempDir(), "--project-map", "map.json"},
		{"--from", "engram", "--db", "engram.db", "--rollback", "latest"},
	}
	for _, args := range cases {
		code, _, stderr := runCLIEngram(t, args...)
		if code != 2 {
			t.Fatalf("%v: exit = %d, want 2; stderr: %s", args, code, stderr)
		}
	}
}

func TestMigrateEngramPromptsRequireOptInConfig(t *testing.T) {
	source := writeCLIEngram(t)
	target := filepath.Join(t.TempDir(), "data")
	code, _, stderr := runCLIEngram(t, "--from", "engram", "--db", source, "--data-dir", target, "--include-prompts")
	if code != 1 || !strings.Contains(stderr, "prompts.enabled is false") {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
}

func TestMigrateEngramApplyRollbackAndPromptZone(t *testing.T) {
	source := writeCLIEngram(t)
	target := filepath.Join(t.TempDir(), "data")
	configPath := filepath.Join(target, "brain.yaml")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("create target: %v", err)
	}
	config := "mode: local\ndata_dir: " + filepath.ToSlash(target) + "\nprompts:\n  enabled: true\n"
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	before := rbCLISnapshot(t, target)

	code, stdout, stderr := runCLIEngram(t, "--from", "engram", "--db", source,
		"--data-dir", target, "--config", configPath, "--include-prompts")
	if code != 0 {
		t.Fatalf("apply exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "manifest  ") {
		t.Fatalf("apply did not print the manifest:\n%s", stdout)
	}
	promptFiles, err := filepath.Glob(filepath.Join(target, "projects", "lv24", "prompts", "*.md"))
	if err != nil || len(promptFiles) != 1 {
		t.Fatalf("prompt files = %v, %v", promptFiles, err)
	}
	raw, err := os.ReadFile(promptFiles[0])
	if err != nil || !strings.Contains(string(raw), "visibility: local") || !strings.Contains(string(raw), "cli prompt") {
		t.Fatalf("prompt file = %q, %v", raw, err)
	}

	code, stdout, stderr = runCLIEngram(t, "--rollback", "latest", "--data-dir", target, "--config", configPath)
	if code != 0 {
		t.Fatalf("rollback exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "skipped   0") {
		t.Fatalf("rollback report missing skipped count:\n%s", stdout)
	}
	rbCLIAssertSame(t, before, rbCLISnapshot(t, target))
}

func TestMigrateEngramProjectMapOverride(t *testing.T) {
	source := writeCLIEngram(t)
	target := filepath.Join(t.TempDir(), "data")
	mapPath := filepath.Join(t.TempDir(), "project-map.json")
	if err := os.WriteFile(mapPath, []byte(`{"map":{"lv":"mapped-project"}}`), 0o644); err != nil {
		t.Fatalf("write project map: %v", err)
	}
	code, stdout, stderr := runCLIEngram(t, "--from", "engram", "--db", source,
		"--data-dir", target, "--project-map", mapPath, "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "mapped-project") {
		t.Fatalf("project map override not reported:\n%s", stdout)
	}
}
