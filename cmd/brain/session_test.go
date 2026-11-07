package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionHooksForClaude(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	checkout := filepath.Join(root, "demo")
	unrelated := filepath.Join(root, "elsewhere")
	for _, dir := range []string{checkout, unrelated} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
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
	hookJSON := func(fields map[string]string) string {
		raw, _ := json.Marshal(fields)
		return string(raw)
	}

	// Start injects the project context as Claude Code additionalContext.
	code, stdout, stderr := runWithInputForTest(t, hookJSON(map[string]string{"cwd": checkout, "source": "startup"}),
		"session", "start", "--hook", "claude", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("session start exit %d: %s", code, stderr)
	}
	var out claudeStartOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if out.HookSpecificOutput.HookEventName != "SessionStart" ||
		!strings.Contains(out.HookSpecificOutput.AdditionalContext, "Company Brain: demo") ||
		!strings.Contains(out.HookSpecificOutput.AdditionalContext, "Seed") {
		t.Fatalf("additionalContext = %+v", out)
	}

	// An unregistered directory is silent and never fails the agent.
	code, stdout, _ = runWithInputForTest(t, hookJSON(map[string]string{"cwd": unrelated}),
		"session", "start", "--hook", "claude", "--config", cfgPath)
	if code != 0 || stdout != "" {
		t.Fatalf("unregistered start = %d %q", code, stdout)
	}

	// End records the agent's last message as an automatic handoff.
	transcript := filepath.Join(root, "transcript.jsonl")
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"do it"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done:\n  shipped the   parser."}]}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	endInput := hookJSON(map[string]string{"cwd": checkout, "transcript_path": transcript, "reason": "exit"})
	if code, _, stderr := runWithInputForTest(t, endInput, "session", "end", "--hook", "claude", "--config", cfgPath); code != 0 {
		t.Fatalf("session end exit %d: %s", code, stderr)
	}
	handoffs, _ := filepath.Glob(filepath.Join(dataDir, "projects", "demo", "handoffs", "*.md"))
	if len(handoffs) != 1 {
		t.Fatalf("handoffs after end = %v", handoffs)
	}
	raw, _ := os.ReadFile(handoffs[0])
	if !strings.Contains(string(raw), "[auto] Done: shipped the parser.") {
		t.Fatalf("handoff = %s", raw)
	}

	// A second end right away is skipped: a handoff was just written.
	if code, _, _ := runWithInputForTest(t, endInput, "session", "end", "--hook", "claude", "--config", cfgPath); code != 0 {
		t.Fatal("second session end failed")
	}
	if again, _ := filepath.Glob(filepath.Join(dataDir, "projects", "demo", "handoffs", "*.md")); len(again) != 1 {
		t.Fatalf("duplicate handoff written: %v", again)
	}
}

func TestSessionEndWithoutTranscriptNeedsActivity(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "note", "--create", "--config", cfgPath, "--project", "demo", "--title", "Found the bug", "--body", "x"); code != 0 {
		t.Fatalf("note: %s", stderr)
	}
	code, stdout, stderr := runForTest(t, "session", "end", "--hook", "opencode", "--cwd", filepath.Join(root, "nowhere"), "--config", cfgPath)
	if code != 0 || stdout != "" {
		t.Fatalf("unregistered end = %d %q %s", code, stdout, stderr)
	}
	if code, _, _ := runForTest(t, "session", "end", "--hook", "bogus", "--config", cfgPath); code != 2 {
		t.Fatal("unknown hook accepted")
	}
	if code, _, _ := runForTest(t, "session", "middle"); code != 2 {
		t.Fatal("unknown action accepted")
	}

	checkout := filepath.Join(root, "demo")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runForTest(t, "project", "set", "demo", "--add-path", checkout, "--config", cfgPath); code != 0 {
		t.Fatalf("add-path: %s", stderr)
	}
	if code, _, stderr := runForTest(t, "session", "end", "--hook", "opencode", "--cwd", checkout, "--config", cfgPath); code != 0 {
		t.Fatalf("opencode end: %s", stderr)
	}
	handoffs, _ := filepath.Glob(filepath.Join(dataDir, "projects", "demo", "handoffs", "*.md"))
	if len(handoffs) != 1 {
		t.Fatalf("handoffs = %v", handoffs)
	}
	raw, _ := os.ReadFile(handoffs[0])
	if !strings.Contains(string(raw), "[auto] session activity: 1 notes (latest: Found the bug)") {
		t.Fatalf("handoff = %s", raw)
	}
}
