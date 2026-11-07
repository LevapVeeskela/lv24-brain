package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func codexFixture(t *testing.T) (cfgPath, dataDir, checkout string) {
	t.Helper()
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir = filepath.Join(root, "brain")
	cfgPath = filepath.Join(dataDir, "brain.yaml")
	checkout = filepath.Join(root, "demo")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "note", "--create", "--config", cfgPath, "--project", "demo", "--title", "Seed", "--body", "x"); code != 0 {
		t.Fatalf("note: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "project", "set", "demo", "--add-path", checkout, "--config", cfgPath); code != 0 {
		t.Fatalf("add-path: %s", stderr)
	}
	return cfgPath, dataDir, checkout
}

func codexInput(t *testing.T, fields map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func handoffFiles(t *testing.T, dataDir string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dataDir, "projects", "demo", "handoffs", "*.md"))
	return files
}

func TestCodexSessionHooks(t *testing.T) {
	cfgPath, dataDir, checkout := codexFixture(t)

	code, stdout, stderr := runWithInputForTest(t, codexInput(t, map[string]string{
		"session_id": "s1", "cwd": checkout, "hook_event_name": "SessionStart", "source": "startup",
	}), "session", "start", "--hook", "codex", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("start: %d %s", code, stderr)
	}
	var out claudeStartOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || !strings.Contains(out.HookSpecificOutput.AdditionalContext, "Company Brain: demo") {
		t.Fatalf("start output = %q (%v)", stdout, err)
	}

	for _, message := range []string{"first turn", "Done: shipped the parser."} {
		if code, _, stderr := runWithInputForTest(t, codexInput(t, map[string]string{
			"session_id": "s1", "cwd": checkout, "hook_event_name": "Stop", "last_assistant_message": message,
		}), "session", "end", "--hook", "codex", "--config", cfgPath); code != 0 {
			t.Fatalf("stop: %s", stderr)
		}
	}
	if files := handoffFiles(t, dataDir); len(files) != 0 {
		t.Fatalf("Stop wrote a handoff: %v", files)
	}

	if code, _, stderr := runWithInputForTest(t, codexInput(t, map[string]string{
		"session_id": "s1", "cwd": checkout, "hook_event_name": "SessionEnd", "reason": "other",
	}), "session", "end", "--hook", "codex", "--config", cfgPath); code != 0 {
		t.Fatalf("session end: %s", stderr)
	}
	files := handoffFiles(t, dataDir)
	if len(files) != 1 {
		t.Fatalf("handoffs = %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	if !strings.Contains(string(raw), "[auto] Done: shipped the parser.") {
		t.Fatalf("handoff = %s", raw)
	}
	if left, _ := filepath.Glob(filepath.Join(dataDir, "index", "sessions", "*")); len(left) != 0 {
		t.Fatalf("checkpoint not removed: %v", left)
	}
}

func TestCodexStaleCheckpointIsFlushedByTheNextStart(t *testing.T) {
	cfgPath, dataDir, checkout := codexFixture(t)
	if code, _, stderr := runWithInputForTest(t, codexInput(t, map[string]string{
		"session_id": "old", "cwd": checkout, "hook_event_name": "Stop", "last_assistant_message": "Left without SessionEnd.",
	}), "session", "end", "--hook", "codex", "--config", cfgPath); code != 0 {
		t.Fatalf("stop: %s", stderr)
	}
	checkpoints, _ := filepath.Glob(filepath.Join(dataDir, "index", "sessions", "codex-*.json"))
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints = %v", checkpoints)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(checkpoints[0], past, past); err != nil {
		t.Fatal(err)
	}

	if code, _, stderr := runWithInputForTest(t, codexInput(t, map[string]string{
		"session_id": "new", "cwd": checkout, "hook_event_name": "SessionStart",
	}), "session", "start", "--hook", "codex", "--config", cfgPath); code != 0 {
		t.Fatalf("start: %s", stderr)
	}
	files := handoffFiles(t, dataDir)
	if len(files) != 1 {
		t.Fatalf("stale checkpoint not flushed: %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	if !strings.Contains(string(raw), "Left without SessionEnd.") {
		t.Fatalf("handoff = %s", raw)
	}
}
