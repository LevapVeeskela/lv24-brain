package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupLimitsMCPBrain creates a local brain with two projects and appends a
// limits block to brain.yaml.
func setupLimitsMCPBrain(t *testing.T, limits string) string {
	t.Helper()
	t.Setenv("BRAIN_CONFIG", "")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit %d: %s", code, stderr)
	}
	for _, project := range []string{"demo", "global"} {
		if code, _, stderr := runForTest(t, "project", "set", project, "--config", cfgPath, "--kind", "repository"); code != 0 {
			t.Fatalf("project set %s exit %d: %s", project, code, stderr)
		}
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.WriteFile(cfgPath, []byte(content+limits), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// runMCPArgsForTest feeds newline-delimited requests to runMCP with explicit
// arguments and decodes the stdout responses.
func runMCPArgsForTest(t *testing.T, args []string, messages ...string) (int, []map[string]any, string) {
	t.Helper()
	pr, pw := io.Pipe()
	go func() {
		for _, msg := range messages {
			if _, err := io.WriteString(pw, msg+"\n"); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()
	var stdout, stderr bytes.Buffer
	code := runMCP(append([]string{"--config"}, args...), pr, &stdout, &stderr)
	_ = pr.Close()
	var responses []map[string]any
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("stdout line is not JSON: %q", line)
		}
		responses = append(responses, msg)
	}
	return code, responses, stderr.String()
}

// toolErrorText returns isError and the human text of a tool result.
func toolErrorText(t *testing.T, response map[string]any) (bool, string) {
	t.Helper()
	result := mcpResult(t, response)
	isError, _ := result["isError"].(bool)
	content, _ := result["content"].([]any)
	var text strings.Builder
	for _, item := range content {
		entry, _ := item.(map[string]any)
		if value, ok := entry["text"].(string); ok {
			text.WriteString(value)
		}
	}
	if structured, ok := result["structuredContent"].(map[string]any); ok {
		if message, ok := structured["message"].(string); ok {
			text.WriteString("\n" + message)
		}
	}
	return isError, text.String()
}

func TestMCPWriteLimitsAndProtectedProject(t *testing.T) {
	cfgPath := setupLimitsMCPBrain(t, "limits:\n  max_body_bytes: 64\n  max_metadata_bytes: 8\n  protected_projects:\n    - global\n")
	code, responses, stderr := runMCPArgsForTest(t, []string{cfgPath, "--allow-write"},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		mcpToolCall(2, "brain_note", `{"text":"agent try","title":"Blocked","project":"global"}`),
		mcpToolCall(3, "brain_note", `{"text":"`+strings.Repeat("x", 200)+`","title":"Too big","project":"demo"}`),
		mcpToolCall(4, "brain_decision", `{"title":"Metadata","alternatives":["0123456789"],"project":"demo"}`),
		mcpToolCall(5, "brain_search", `{"query":"xxxxxxxx","project":"demo"}`),
	)
	if code != 0 {
		t.Fatalf("runMCP exit %d: %s", code, stderr)
	}
	if len(responses) != 5 {
		t.Fatalf("responses = %d, want 5: %v", len(responses), responses)
	}

	protectedIsError, protectedText := toolErrorText(t, responses[1])
	if !protectedIsError || !strings.Contains(protectedText, "limits.protected_projects") ||
		!strings.Contains(protectedText, "global") {
		t.Fatalf("protected MCP write = isError %v, text %q", protectedIsError, protectedText)
	}
	bodyIsError, bodyText := toolErrorText(t, responses[2])
	if !bodyIsError || !strings.Contains(bodyText, "limits.max_body_bytes") ||
		!strings.Contains(bodyText, "200") {
		t.Fatalf("oversized MCP write = isError %v, text %q", bodyIsError, bodyText)
	}
	metadataIsError, metadataText := toolErrorText(t, responses[3])
	if !metadataIsError || !strings.Contains(metadataText, "limits.max_metadata_bytes") {
		t.Fatalf("metadata MCP write = isError %v, text %q", metadataIsError, metadataText)
	}

	search := mcpStructured(t, mcpResult(t, responses[4]))
	if hits, _ := search["hits"].([]any); len(hits) != 0 {
		t.Fatalf("refused writes reached the index: %v", hits)
	}
	dataDir := filepath.Dir(cfgPath)
	for _, project := range []string{"demo", "global"} {
		for _, dir := range []string{"notes", "decisions", "attempts", "handoffs"} {
			files, err := filepath.Glob(filepath.Join(dataDir, "projects", project, dir, "*.md"))
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Fatalf("refused MCP writes left files for %s/%s: %v", project, dir, files)
			}
		}
	}
}

func TestMCPDefaultsStillAcceptOrdinaryWrites(t *testing.T) {
	cfgPath := setupLimitsMCPBrain(t, "")
	code, responses, stderr := runMCPArgsForTest(t, []string{cfgPath, "--allow-write"},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		mcpToolCall(2, "brain_note", `{"text":"A normal note","title":"Normal","project":"demo"}`),
	)
	if code != 0 {
		t.Fatalf("runMCP exit %d: %s", code, stderr)
	}
	isError, text := toolErrorText(t, responses[1])
	if isError {
		t.Fatalf("default limits refused an ordinary MCP note: %s", text)
	}
}
