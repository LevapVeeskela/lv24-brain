package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// runMCPForTest feeds newline-delimited requests to runMCP through a pipe and
// returns the exit code and the decoded stdout responses.
func runMCPForTest(t *testing.T, cfgPath string, messages ...string) (int, []map[string]any, string) {
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
	code := runMCP([]string{"--config", cfgPath}, pr, &stdout, &stderr)
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

func mcpToolCall(id int, name, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
}

func mcpResult(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	if errObj, ok := response["error"]; ok {
		t.Fatalf("JSON-RPC error: %v", errObj)
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", response)
	}
	return result
}

func mcpStructured(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	obj, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("structuredContent = %v", result["structuredContent"])
	}
	return obj
}

func TestMCPServesBrainTools(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	if code, _, stderr := runForTest(t, "project", "set", "demo", "--config", cfgPath, "--kind", "repository"); code != 0 {
		t.Fatalf("project set exit code = %d (stderr: %s)", code, stderr)
	}

	code, responses, stderr := runMCPForTest(t, cfgPath,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		mcpToolCall(3, "brain_note", `{"text":"Fixed the search cache layer","title":"Search cache","project":"demo"}`),
		mcpToolCall(4, "brain_search", `{"query":"cache","project":"demo"}`),
		mcpToolCall(5, "brain_open", `{"project":"demo"}`),
		mcpToolCall(6, "brain_note", `{"text":"unregistered","project":"ghost"}`),
	)
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	if len(responses) != 6 {
		t.Fatalf("responses = %d, want 6: %v", len(responses), responses)
	}

	initResult := mcpResult(t, responses[0])
	if initResult["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", initResult["protocolVersion"])
	}
	if info := initResult["serverInfo"].(map[string]any); info["name"] != "lv24-brain" {
		t.Fatalf("serverInfo = %v", info)
	}

	tools := mcpResult(t, responses[1])["tools"].([]any)
	if len(tools) != 16 { // 8 core tools + brain_context + brain_state_get/brain_state_set + brain_catchup + brain_review + brain_records/brain_record/brain_decision_status
		t.Fatalf("tools = %d, want 16", len(tools))
	}
	if name := tools[1].(map[string]any)["name"]; name != "brain_context" {
		t.Fatalf("tool 1 = %v, want brain_context next to brain_open", name)
	}

	noteResult := mcpResult(t, responses[2])
	if noteResult["isError"] != false {
		t.Fatalf("brain_note isError = %v", noteResult["isError"])
	}
	noteID, _ := mcpStructured(t, noteResult)["id"].(string)
	if !strings.HasPrefix(noteID, "note-demo-") {
		t.Fatalf("note id = %q", noteID)
	}

	searchResult := mcpResult(t, responses[3])
	hits, _ := mcpStructured(t, searchResult)["hits"].([]any)
	found := false
	for _, raw := range hits {
		if hit := raw.(map[string]any); hit["id"] == noteID {
			found = true
		}
	}
	if !found {
		t.Fatalf("brain_search did not find %s: %v", noteID, hits)
	}

	openResult := mcpResult(t, responses[4])
	openObj := mcpStructured(t, openResult)
	if openObj["project_id"] != "demo" || !strings.Contains(openObj["summary"].(string), "demo") {
		t.Fatalf("brain_open structured = %v", openObj)
	}
	if text := openResult["content"].([]any)[0].(map[string]any)["text"]; text != openObj["summary"] {
		t.Fatalf("brain_open text does not match summary")
	}

	unknown := mcpResult(t, responses[5])
	if unknown["isError"] != true {
		t.Fatalf("write to unregistered project must be isError: %v", unknown)
	}
}

func TestMCPResolvesSoleProjectFromWorkingDirectory(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	if code, _, stderr := runForTest(t, "project", "set", "solo", "--config", cfgPath, "--kind", "repository"); code != 0 {
		t.Fatalf("project set exit code = %d (stderr: %s)", code, stderr)
	}
	code, responses, stderr := runMCPForTest(t, cfgPath, mcpToolCall(1, "brain_open", `{}`))
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	obj := mcpStructured(t, mcpResult(t, responses[0]))
	if obj["project_id"] != "solo" {
		t.Fatalf("project_id = %v, want solo", obj["project_id"])
	}
}

func TestMCPRejectsExtraArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := runMCP([]string{"extra"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("usage error must not write to stdout: %q", stdout.String())
	}
}
