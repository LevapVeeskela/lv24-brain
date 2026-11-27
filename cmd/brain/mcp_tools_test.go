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
	Profile struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Strict  bool   `json:"strict"`
	} `json:"profile"`
	ProtocolVersions []string `json:"protocol_versions"`
	Tools            []struct {
		Name         string `json:"name"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		Policy       string `json:"policy"`
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
	if catalogue.Profile.Name != "lv24" || catalogue.Profile.Version == "" {
		t.Fatalf("profile = %+v", catalogue.Profile)
	}
	if len(catalogue.ProtocolVersions) == 0 || catalogue.ProtocolVersions[0] != "2026-07-28" {
		t.Fatalf("protocol_versions = %v", catalogue.ProtocolVersions)
	}
	if len(catalogue.Tools) != 18 {
		t.Fatalf("tools = %d, want 18", len(catalogue.Tools))
	}
	for _, tool := range catalogue.Tools {
		if tool.Name == "" || tool.Title == "" || tool.Description == "" {
			t.Fatalf("incomplete tool: %+v", tool)
		}
		if tool.Policy != "read" && tool.Policy != "append" && tool.Policy != "destructive" {
			t.Fatalf("%s policy = %q", tool.Name, tool.Policy)
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
		if tool.Name != "brain_record" {
			continue
		}
		if tool.Limits["default_page_size"] != 50 || tool.Limits["max_page_size"] != 200 {
			t.Fatalf("brain_record limits = %v", tool.Limits)
		}
		return
	}
	t.Fatal("brain_record missing from the catalogue")
}

func TestMCPListToolsTextSummary(t *testing.T) {
	code, stdout, stderr := runForTest(t, "mcp", "--list-tools")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"brain_open", "brain_search", "brain_judge", "destructive"} {
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

func TestMCPProfileFlag(t *testing.T) {
	code, stdout, stderr := runForTest(t, "mcp", "--list-tools", "--json", "--profile", "lv24", "--profile-version", "1")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, `"name": "lv24"`) {
		t.Fatalf("stdout does not report the lv24 profile: %s", stdout)
	}
	code, stdout, stderr = runForTest(t, "mcp", "--list-tools", "--json", "--profile", "engram")
	if code != 0 {
		t.Fatalf("engram profile: code = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, `"name": "engram"`) || !strings.Contains(stdout, `"legacy": "mem_save"`) {
		t.Fatalf("engram catalogue misses its vocabulary: %s", stdout)
	}
	code, _, stderr = runForTest(t, "mcp", "--list-tools", "--profile", "bogus")
	if code != 2 || !strings.Contains(stderr, "unknown profile") {
		t.Fatalf("unknown profile: code = %d, stderr = %q", code, stderr)
	}
	code, _, stderr = runForTest(t, "mcp", "--list-tools", "--profile", "lv24", "--profile-version", "9")
	if code != 2 || !strings.Contains(stderr, "unsupported profile version") {
		t.Fatalf("unknown version: code = %d, stderr = %q", code, stderr)
	}
}

func TestMCPListToolsReportsProfileGradesAndDigest(t *testing.T) {
	code, stdout, stderr := runForTest(t, "mcp", "--list-tools", "--json", "--profile", "engram", "--strict")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	var doc struct {
		Profile struct {
			Name   string `json:"name"`
			Strict bool   `json:"strict"`
			Native bool   `json:"native"`
		} `json:"profile"`
		Tools []struct {
			Name   string `json:"name"`
			Legacy bool   `json:"legacy"`
			Target string `json:"target"`
			Grade  string `json:"grade"`
		} `json:"tools"`
		Aliases []struct {
			Legacy string `json:"legacy"`
			Grade  string `json:"grade"`
		} `json:"aliases"`
		Capabilities struct {
			Tools struct {
				Total   int            `json:"total"`
				Aliased int            `json:"aliased"`
				Grades  map[string]int `json:"grades"`
			} `json:"tools"`
			Gaps []struct {
				Tool string `json:"tool"`
			} `json:"gaps"`
		} `json:"capabilities"`
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if doc.Profile.Name != "engram" || !doc.Profile.Strict || doc.Profile.Native {
		t.Fatalf("profile = %+v", doc.Profile)
	}
	if len(doc.Tools) != 28 || doc.Capabilities.Tools.Total != 28 || doc.Capabilities.Tools.Aliased != 27 {
		t.Fatalf("tools = %d, total = %d, aliased = %d", len(doc.Tools), doc.Capabilities.Tools.Total, doc.Capabilities.Tools.Aliased)
	}
	if len(doc.Aliases) != 27 {
		t.Fatalf("aliases = %d, want 27", len(doc.Aliases))
	}
	wantGrades := map[string]int{"A": 3, "B": 15, "C": 7, "D": 2}
	for grade, want := range wantGrades {
		if doc.Capabilities.Tools.Grades[grade] != want {
			t.Fatalf("grade %s = %d, want %d (%v)", grade, doc.Capabilities.Tools.Grades[grade], want, doc.Capabilities.Tools.Grades)
		}
	}
	if len(doc.Capabilities.Gaps) == 0 {
		t.Fatal("engram capabilities has no gaps")
	}
	if !strings.HasPrefix(doc.Digest, "sha256:") {
		t.Fatalf("digest = %q", doc.Digest)
	}
}

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
	records := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"brain_record","arguments":{"action":"list","project":"modern","limit":1},` + modernEnvelope + `}}`

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
