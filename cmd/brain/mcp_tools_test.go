package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// mcpCatalogueJSON is the shape `brain mcp --list-tools --json` prints.
type mcpCatalogueJSON struct {
	Server struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"server"`
	ProtocolVersions []string `json:"protocol_versions"`
	Tools            []struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Annotations  map[string]any
		InputSchema  map[string]any `json:"input_schema"`
		OutputSchema map[string]any `json:"output_schema"`
		Limits       map[string]int `json:"limits"`
	} `json:"tools"`
}

func TestMCPListToolsJSONNeedsNoConfig(t *testing.T) {
	// No --config and no brain.yaml in the working directory: listing the tool
	// contract must not require an instance.
	code, stdout, stderr := runForTest(t, "mcp", "--list-tools", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	var catalogue mcpCatalogueJSON
	if err := json.Unmarshal([]byte(stdout), &catalogue); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if catalogue.Server.Name != "lv24-brain" || catalogue.Server.Version == "" {
		t.Fatalf("server = %+v", catalogue.Server)
	}
	if len(catalogue.ProtocolVersions) == 0 || catalogue.ProtocolVersions[0] != "2026-07-28" {
		t.Fatalf("protocol_versions = %v", catalogue.ProtocolVersions)
	}
	if len(catalogue.Tools) != 16 {
		t.Fatalf("tools = %d, want 16", len(catalogue.Tools))
	}
	for _, tool := range catalogue.Tools {
		if tool.Name == "" || tool.Title == "" || tool.Description == "" {
			t.Fatalf("incomplete tool: %+v", tool)
		}
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint", "title"} {
			if _, ok := tool.Annotations[hint]; !ok {
				t.Fatalf("%s annotations.%s missing: %v", tool.Name, hint, tool.Annotations)
			}
		}
		if tool.InputSchema["type"] != "object" {
			t.Fatalf("%s input_schema = %v", tool.Name, tool.InputSchema)
		}
		if tool.OutputSchema["type"] != "object" {
			t.Fatalf("%s output_schema = %v", tool.Name, tool.OutputSchema)
		}
	}
	for _, tool := range catalogue.Tools {
		if tool.Name != "brain_records" {
			continue
		}
		if tool.Limits["default_page_size"] != 50 || tool.Limits["max_page_size"] != 200 {
			t.Fatalf("brain_records limits = %v", tool.Limits)
		}
		return
	}
	t.Fatal("brain_records missing from the catalogue")
}

func TestMCPListToolsTextSummary(t *testing.T) {
	code, stdout, stderr := runForTest(t, "mcp", "--list-tools")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"brain_open", "brain_search", "brain_decision_status", "read"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q: %s", want, stdout)
		}
	}
	if strings.Contains(stdout, "{\"") {
		t.Fatalf("text summary looks like JSON: %s", stdout)
	}
}

func TestMCPJSONWithoutListToolsIsUsageError(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := runMCP([]string{"--json"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("usage error wrote to stdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--list-tools") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestMCPListToolsJSONIsStable(t *testing.T) {
	// Hosts pin the tool contract by hash; the same binary must print the same
	// bytes twice.
	_, first, _ := runForTest(t, "mcp", "--list-tools", "--json")
	_, second, _ := runForTest(t, "mcp", "--list-tools", "--json")
	if first != second {
		t.Fatal("--list-tools --json is not deterministic")
	}
}

// modernEnvelope is the _meta a 2026 client sends on every request.
const modernEnvelope = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

func TestMCPModernSessionAgainstRealBackend(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	if code, _, stderr := runForTest(t, "project", "set", "modern", "--config", cfgPath, "--kind", "repository"); code != 0 {
		t.Fatalf("project set exit code = %d (stderr: %s)", code, stderr)
	}

	discover := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{` + modernEnvelope + `}}`
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{` + modernEnvelope + `}}`
	note := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"brain_note","arguments":{"text":"modern client note","project":"modern"},` + modernEnvelope + `}}`
	records := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"brain_records","arguments":{"project":"modern","limit":1},` + modernEnvelope + `}}`

	code, responses, stderr := runMCPForTest(t, cfgPath, discover, list, note, records)
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	if len(responses) != 4 {
		t.Fatalf("responses = %d, want 4", len(responses))
	}
	if got := mcpResult(t, responses[0])["resultType"]; got != "complete" {
		t.Fatalf("discover resultType = %v", got)
	}
	if got := mcpResult(t, responses[1])["resultType"]; got != "complete" {
		t.Fatalf("tools/list resultType = %v", got)
	}
	if got := mcpResult(t, responses[2])["isError"]; got != false {
		t.Fatalf("brain_note isError = %v", got)
	}
	obj := mcpStructured(t, mcpResult(t, responses[3]))
	if obj["page"] != float64(1) || obj["page_size"] != float64(1) || obj["has_more"] != false {
		t.Fatalf("pagination envelope = %v", obj)
	}
	if ids, _ := obj["records"].([]any); len(ids) != 1 {
		t.Fatalf("records = %v", obj["records"])
	}
}
