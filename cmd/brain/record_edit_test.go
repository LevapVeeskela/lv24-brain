package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordEditCLI(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	code, stdout, stderr := runForTest(t, "note", "--create", "--config", cfgPath, "--project", "demo",
		"--title", "Original", "--body", "old text")
	if code != 0 {
		t.Fatalf("note: %s", stderr)
	}
	id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(stdout), "note "))
	if id == "" || strings.Contains(id, " ") {
		t.Fatalf("note output = %q", stdout)
	}

	code, stdout, stderr = runWithInputForTest(t, "new body from stdin\n",
		"record", "edit", id, "--title", "Edited", "--body-file", "-",
		"--config", cfgPath, "--project", "demo")
	if code != 0 {
		t.Fatalf("edit: %s", stderr)
	}
	if !strings.Contains(stdout, "edited note "+id) || !strings.Contains(stdout, "updated:") {
		t.Fatalf("edit output = %q", stdout)
	}

	_, data := runJSONForTest(t, "record", "show", id, "--json", "--config", cfgPath, "--project", "demo")
	record := asObject(t, "record", data["record"])
	if record["title"] != "Edited" || record["body"] != "new body from stdin" {
		t.Fatalf("record after edit = %v", record)
	}
	if record["id"] != id {
		t.Fatalf("id changed: %v", record["id"])
	}

	if code, _, stderr := runForTest(t, "record", "edit", id, "--title", "Preview", "--dry-run",
		"--config", cfgPath, "--project", "demo"); code != 0 {
		t.Fatalf("dry run: %s", stderr)
	}
	_, data = runJSONForTest(t, "record", "show", id, "--json", "--config", cfgPath, "--project", "demo")
	record = asObject(t, "record", data["record"])
	if record["title"] != "Edited" {
		t.Fatalf("dry run wrote: %v", record["title"])
	}

	updated, _ := record["updated_at"].(string)
	if updated == "" {
		t.Fatalf("record has no updated_at: %v", record)
	}
	if code, _, stderr := runForTest(t, "record", "edit", id, "--title", "CAS edit",
		"--expected-updated-at", updated, "--config", cfgPath, "--project", "demo"); code != 0 {
		t.Fatalf("CAS edit: %s", stderr)
	}
	code, _, stderr = runForTest(t, "record", "edit", id, "--title", "Stale edit",
		"--expected-updated-at", updated, "--config", cfgPath, "--project", "demo")
	if code != 1 || !strings.Contains(stderr, "current revision") {
		t.Fatalf("stale edit = %d %q", code, stderr)
	}

	if code, _, stderr := runForTest(t, "record", "edit", id, "--config", cfgPath, "--project", "demo"); code != 2 {
		t.Fatalf("edit without fields = %d %q", code, stderr)
	}

	if code, _, stderr := runForTest(t, "record", "edit", id, "--tags", "one,two",
		"--config", cfgPath, "--project", "demo"); code != 0 {
		t.Fatalf("tags edit: %s", stderr)
	}
	notes, err := filepath.Glob(filepath.Join(dataDir, "projects", "demo", "notes", "*.md"))
	if err != nil || len(notes) != 1 {
		t.Fatalf("notes files = %v %v", notes, err)
	}
	raw, err := os.ReadFile(notes[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"tags":["one","two"]`) {
		t.Fatalf("tags not stored: %s", raw)
	}
}
