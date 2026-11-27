package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// sessionIdeasFixture is a local brain with a demo project and one checkout.
func sessionIdeasFixture(t *testing.T) (cfgPath, dataDir, checkout string) {
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

func addSessionIdeas(t *testing.T, cfgPath string, titles ...string) {
	t.Helper()
	for _, title := range titles {
		if code, _, stderr := runForTest(t, "idea", "add", "--config", cfgPath, "--project", "demo", title); code != 0 {
			t.Fatalf("idea add %q: %s", title, stderr)
		}
	}
}

func runClaudeSessionStart(t *testing.T, cfgPath, cwd string, input map[string]string) string {
	t.Helper()
	if input == nil {
		input = map[string]string{}
	}
	input["cwd"] = cwd
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runWithInputForTest(t, string(raw), "session", "start", "--hook", "claude", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("session start exit %d: %s", code, stderr)
	}
	var out claudeStartOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode session start %q: %v", stdout, err)
	}
	return out.HookSpecificOutput.AdditionalContext
}

// ideasBlock extracts the single `## Ideas` block from the session context.
func ideasBlock(text string) string {
	start := strings.Index(text, "## Ideas")
	if start < 0 {
		return ""
	}
	rest := text[start:]
	if end := strings.Index(rest[1:], "\n## "); end >= 0 {
		rest = rest[:end+1]
	}
	return rest
}

func TestSessionStartShowsTheIdeasBlock(t *testing.T) {
	cfgPath, _, checkout := sessionIdeasFixture(t)
	addSessionIdeas(t, cfgPath, "Ship the parser", "Write the docs", "Fix the footer", "Later idea")

	text := runClaudeSessionStart(t, cfgPath, checkout, map[string]string{"source": "startup"})
	block := ideasBlock(text)
	if block == "" {
		t.Fatalf("no Ideas block in the session context:\n%s", text)
	}
	if !strings.Contains(block, "## Ideas — 4 open") {
		t.Fatalf("block header = %q", block)
	}
	if items := strings.Count(block, "\n- "); items < 1 || items > 3 {
		t.Fatalf("block items = %d, want 1..3:\n%s", items, block)
	}
	if utf8.RuneCountInString(block) > 700 {
		t.Fatalf("block = %d runes, cap 700:\n%s", utf8.RuneCountInString(block), block)
	}
	handoff := strings.Index(text, "## Latest handoff")
	ideas := strings.Index(text, "## Ideas")
	decisions := strings.Index(text, "## Recent decisions")
	if handoff < 0 || ideas < 0 || decisions < 0 || !(handoff < ideas && ideas < decisions) {
		t.Fatalf("block position = handoff %d, ideas %d, decisions %d:\n%s", handoff, ideas, decisions, text)
	}
}

func TestSessionStartSkipsIdeasOnCompactAndResume(t *testing.T) {
	cfgPath, _, checkout := sessionIdeasFixture(t)
	addSessionIdeas(t, cfgPath, "Ship the parser")

	for _, source := range []string{"compact", "resume"} {
		text := runClaudeSessionStart(t, cfgPath, checkout, map[string]string{"source": source})
		if block := ideasBlock(text); block != "" {
			t.Fatalf("source=%s carried an Ideas block:\n%s", source, block)
		}
	}
	// A manual run without a source still behaves as a startup.
	text := runClaudeSessionStart(t, cfgPath, checkout, nil)
	if block := ideasBlock(text); block == "" {
		t.Fatalf("a source-less start has no Ideas block:\n%s", text)
	}
}

func TestSessionStartIdeasMute(t *testing.T) {
	cfgPath, dataDir, checkout := sessionIdeasFixture(t)
	addSessionIdeas(t, cfgPath, "Ship the parser")
	if code, _, stderr := runForTest(t, "idea", "mute", "--days", "7", "--config", cfgPath); code != 0 {
		t.Fatalf("idea mute: %s", stderr)
	}
	text := runClaudeSessionStart(t, cfgPath, checkout, map[string]string{"source": "startup"})
	if !strings.Contains(text, "ideas: 1 open (greeting muted)") {
		t.Fatalf("muted block is missing:\n%s", text)
	}
	if strings.Contains(text, "Ship the parser") || strings.Contains(text, "## Ideas") {
		t.Fatalf("muted block still shows items or the heading:\n%s", text)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "agent", "ideas.json")); err != nil {
		t.Fatalf("mute state file: %v", err)
	}
}

func TestSessionStartIdeasAtWorkspaceRoot(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	workspace := filepath.Join(root, "workspace")
	alpha := filepath.Join(workspace, "alpha")
	beta := filepath.Join(workspace, "beta")
	for _, dir := range []string{alpha, beta} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init: %s", stderr)
	}
	for _, step := range [][]string{
		{"note", "--create", "--config", cfgPath, "--project", "alpha", "--title", "Seed", "--body", "x"},
		{"project", "set", "alpha", "--add-path", alpha, "--config", cfgPath},
		{"project", "set", "beta", "--add-path", beta, "--config", cfgPath},
		{"idea", "add", "--config", cfgPath, "--project", "alpha", "Workspace idea"},
	} {
		if code, _, stderr := runForTest(t, step...); code != 0 {
			t.Fatalf("%v: %s", step, stderr)
		}
	}
	text := runClaudeSessionStart(t, cfgPath, workspace, map[string]string{"source": "startup"})
	if !strings.Contains(text, "alpha") || !strings.Contains(text, "beta") {
		t.Fatalf("workspace overview is missing:\n%s", text)
	}
	if !strings.Contains(text, "## Ideas — 1 open") {
		t.Fatalf("workspace has no global Ideas block:\n%s", text)
	}
}

func TestSessionIdeasFailureIsNotFatal(t *testing.T) {
	cfgPath, _, checkout := sessionIdeasFixture(t)
	_, application, closeApp, code := newIdeaApp(cfgPath, io.Discard, "test")
	if code != 0 {
		t.Fatalf("newIdeaApp exit %d", code)
	}
	defer closeApp()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if block := sessionIdeasBlock(ctx, application, "demo"); block != "" {
		t.Fatalf("a canceled lookup returned a block: %q", block)
	}
	// The session still renders its project context without the block.
	text := runClaudeSessionStart(t, cfgPath, checkout, map[string]string{"source": "startup"})
	if !strings.Contains(text, "Company Brain: demo") {
		t.Fatalf("session context was lost:\n%s", text)
	}
}
