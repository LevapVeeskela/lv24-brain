package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A hand-damaged daily notes file is quarantined on the next note, the note
// is still written, and doctor reports the quarantined file.
func TestDamagedNotesFileIsQuarantinedAndReported(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "note", "--create", "--config", cfgPath, "--project", "demo", "--title", "First", "--body", "x"); code != 0 {
		t.Fatalf("note: %s", stderr)
	}
	daily, _ := filepath.Glob(filepath.Join(dataDir, "projects", "demo", "notes", "*.md"))
	if len(daily) != 1 {
		t.Fatalf("daily files = %v", daily)
	}
	raw, err := os.ReadFile(daily[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daily[0], []byte(strings.Replace(string(raw), "kind: note", "kind: [note", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "demo", "--title", "Second", "--body", "y"); code != 0 {
		t.Fatalf("note after damage: %s", stderr)
	}
	_, data := runJSONForTest(t, "doctor", "--json", "--config", cfgPath)
	for _, raw := range asArray(t, "checks", data["checks"]) {
		check := asObject(t, "check", raw)
		if check["name"] != "records" {
			continue
		}
		if check["status"] != "warn" || !strings.Contains(check["detail"].(string), "quarantined") {
			t.Fatalf("records check = %v", check)
		}
		return
	}
	t.Fatal("doctor has no records check")
}
