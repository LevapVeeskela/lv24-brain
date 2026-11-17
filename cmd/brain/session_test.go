package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// HOK-4: the OpenCode plugin passes the same stdin JSON contract as Claude
// Code and Codex, so "Decision:" lines in the final message are captured.
func TestOpenCodeSessionEndReadsFinalMessage(t *testing.T) {
	cfgPath, _, checkout := codexFixture(t)
	message := "Done with the plugin.\nDecision: OpenCode hooks pass stdin JSON"
	input := codexInput(t, map[string]string{
		"session_id": "oc1", "cwd": checkout, "hook_event_name": "SessionEnd",
		"last_assistant_message": message,
	})
	if code, _, stderr := runWithInputForTest(t, input, "session", "end", "--hook", "opencode", "--config", cfgPath); code != 0 {
		t.Fatalf("opencode session end: %s", stderr)
	}
	code, stdout, stderr := runForTest(t, "record", "list", "demo", "--kind", "decision", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("record list: %s", stderr)
	}
	if !strings.Contains(stdout, "OpenCode hooks pass stdin JSON") {
		t.Fatalf("decision not captured from the OpenCode message:\n%s", stdout)
	}
}

// HOK-2: every hook invocation appends one liveness line with host, event,
// duration, exit and the first stderr line.
func TestSessionHookLogRecordsRuns(t *testing.T) {
	t.Setenv("BRAIN_HOOK_OFF", "")
	cfgPath, dataDir, checkout := codexFixture(t)

	if code, _, stderr := runWithInputForTest(t, codexInput(t, map[string]string{
		"session_id": "s1", "cwd": checkout, "hook_event_name": "SessionStart",
	}), "session", "start", "--hook", "claude", "--config", cfgPath); code != 0 {
		t.Fatalf("start: %s", stderr)
	}
	if code, _, stderr := runWithInputForTest(t, codexInput(t, map[string]string{
		"session_id": "s1", "cwd": checkout, "hook_event_name": "SessionEnd",
		"last_assistant_message": "Done: shipped the log.",
	}), "session", "end", "--hook", "claude", "--config", cfgPath); code != 0 {
		t.Fatalf("end: %s", stderr)
	}

	entries := readHookLog(dataDir)
	if len(entries) != 2 {
		t.Fatalf("hook log entries = %d, want 2: %+v", len(entries), entries)
	}
	if entries[0].Host != "claude" || entries[0].Event != "session.start" ||
		entries[1].Event != "session.end" {
		t.Errorf("entries = %+v", entries)
	}
	for _, entry := range entries {
		if entry.Exit != 0 || entry.DurationMS < 0 || entry.Time.IsZero() {
			t.Errorf("entry fields = %+v", entry)
		}
	}
	raw, err := os.ReadFile(hookLogPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines != 2 {
		t.Errorf("log lines = %d, want one per invocation", lines)
	}

	// The first stderr line is logged with credentials redacted.
	rec := &firstLineWriter{w: io.Discard}
	fmt.Fprint(rec, "brain session end: token=supersecret exploded\nignored second line")
	logSessionHookRun(sessionOptions{hook: "claude", cfgPath: cfgPath}, "end", time.Now(), 0, rec.firstLine())
	entries = readHookLog(dataDir)
	last := entries[len(entries)-1]
	if strings.Contains(last.Error, "supersecret") || !strings.Contains(last.Error, "<redacted>") {
		t.Errorf("logged error = %q", last.Error)
	}
	if strings.Contains(last.Error, "ignored second line") {
		t.Errorf("only the first stderr line may be logged: %q", last.Error)
	}
}
