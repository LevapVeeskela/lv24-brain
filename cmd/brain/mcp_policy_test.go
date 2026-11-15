package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpPolicyEnv is one initialized data directory with three registered
// projects: "cwd" (this test's working directory), "alpha" (with the alias
// "a") and "beta".
type mcpPolicyEnv struct {
	cfgPath string
}

func newMCPPolicyEnv(t *testing.T) mcpPolicyEnv {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []struct {
		id, path, alias string
	}{
		{id: "cwd", path: workingDir},
		{id: "alpha", path: filepath.Join(t.TempDir(), "alpha"), alias: "a"},
		{id: "beta", path: filepath.Join(t.TempDir(), "beta")},
	} {
		args := []string{"project", "set", project.id, "--config", cfgPath, "--kind", "repository", "--path", project.path}
		if project.alias != "" {
			args = append(args, "--alias", project.alias)
		}
		if code, _, stderr := runForTest(t, args...); code != 0 {
			t.Fatalf("project set %s exit code = %d (stderr: %s)", project.id, code, stderr)
		}
	}
	return mcpPolicyEnv{cfgPath: cfgPath}
}

// run feeds messages to `brain mcp` with extra flags and decodes the responses.
func (e mcpPolicyEnv) run(t *testing.T, flags []string, messages ...string) (int, []map[string]any, string) {
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
	args := append([]string{"--config", e.cfgPath}, flags...)
	var stdout, stderr bytes.Buffer
	code := runMCP(args, pr, &stdout, &stderr)
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

// toolNames returns the names of the tools one server lists.
func (e mcpPolicyEnv) toolNames(t *testing.T, flags []string) map[string]bool {
	t.Helper()
	code, responses, stderr := e.run(t, flags, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	tools, _ := mcpResult(t, responses[0])["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	return names
}

func TestMCPReadOnlyFlagHidesWriteToolsAndRefusesCalls(t *testing.T) {
	env := newMCPPolicyEnv(t)
	names := env.toolNames(t, []string{"--read-only"})
	for _, tool := range []string{"brain_note", "brain_decision", "brain_attempt", "brain_handoff", "brain_state_set", "brain_decision_status", "brain_review_record"} {
		if names[tool] {
			t.Fatalf("--read-only lists %s", tool)
		}
	}
	for _, tool := range []string{"brain_open", "brain_search", "brain_context", "brain_records"} {
		if !names[tool] {
			t.Fatalf("--read-only dropped read tool %s", tool)
		}
	}
	code, responses, stderr := env.run(t, []string{"--read-only"},
		mcpToolCall(1, "brain_note", `{"text":"poison","project":"alpha"}`))
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	result := mcpResult(t, responses[0])
	if result["isError"] != true {
		t.Fatalf("brain_note was not refused: %v", result)
	}
	refusal := mcpStructured(t, result)
	if refusal["code"] != "UNSUPPORTED" {
		t.Fatalf("code = %v, want UNSUPPORTED", refusal["code"])
	}
	tryInstead, _ := refusal["try_instead"].(string)
	if !strings.Contains(tryInstead, "brain_search") || !strings.Contains(tryInstead, "brain_context") {
		t.Fatalf("try_instead = %q", tryInstead)
	}
}

func TestMCPProjectsAllowlistGovernsReads(t *testing.T) {
	env := newMCPPolicyEnv(t)
	code, responses, stderr := env.run(t, []string{"--projects", "alpha"},
		mcpToolCall(1, "brain_open", `{"project":"beta"}`),
		mcpToolCall(2, "brain_open", `{"project":"alpha"}`),
		mcpToolCall(3, "brain_projects", `{}`),
	)
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	refused := mcpResult(t, responses[0])
	if refused["isError"] != true {
		t.Fatalf("reading beta was accepted: %v", refused)
	}
	refusal := mcpStructured(t, refused)
	if refusal["code"] != "INVALID_ARGUMENTS" {
		t.Fatalf("code = %v, want INVALID_ARGUMENTS", refusal["code"])
	}
	if message, _ := refusal["message"].(string); !strings.Contains(message, "alpha") {
		t.Fatalf("message does not name the allowed set: %q", message)
	}
	if listed := mcpResult(t, responses[1]); listed["isError"] != false {
		t.Fatalf("allowed project refused: %v", listed)
	}
	if id := mcpStructured(t, mcpResult(t, responses[1]))["project_id"]; id != "alpha" {
		t.Fatalf("project_id = %v", id)
	}
	projects, _ := mcpStructured(t, mcpResult(t, responses[2]))["projects"].([]any)
	if len(projects) != 1 || projects[0].(map[string]any)["id"] != "alpha" {
		t.Fatalf("brain_projects under --projects = %v", projects)
	}
}

func TestMCPProjectsResolvesAliases(t *testing.T) {
	env := newMCPPolicyEnv(t)
	code, responses, stderr := env.run(t, []string{"--projects", "a"},
		mcpToolCall(1, "brain_open", `{"project":"alpha"}`),
		mcpToolCall(2, "brain_open", `{"project":"beta"}`),
	)
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	if got := mcpResult(t, responses[0]); got["isError"] != false {
		t.Fatalf("alias project refused: %v", got)
	}
	if got := mcpResult(t, responses[1]); got["isError"] != true {
		t.Fatalf("unlisted project accepted: %v", got)
	}
}

func TestMCPProjectsRejectsUnknownEntry(t *testing.T) {
	env := newMCPPolicyEnv(t)
	code, _, stderr := env.run(t, []string{"--projects", "alpha,ghost"})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "ghost") || !strings.Contains(stderr, "--projects") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestMCPProjectsRejectsEmptyEntry(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := runMCP([]string{"--projects", "alpha,,beta"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "--projects") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestMCPDefaultWriteScopeIsTheWorkingDirectory(t *testing.T) {
	env := newMCPPolicyEnv(t)
	code, responses, stderr := env.run(t, nil,
		mcpToolCall(1, "brain_note", `{"text":"in the checkout","project":"cwd"}`),
		mcpToolCall(2, "brain_note", `{"text":"another project","project":"beta"}`),
		mcpToolCall(3, "brain_open", `{"project":"beta"}`),
	)
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	if got := mcpResult(t, responses[0]); got["isError"] != false {
		t.Fatalf("write to the working-directory project refused: %v", got)
	}
	blocked := mcpResult(t, responses[1])
	if blocked["isError"] != true {
		t.Fatalf("write to another project accepted: %v", blocked)
	}
	refusal := mcpStructured(t, blocked)
	if refusal["code"] != "UNSUPPORTED" {
		t.Fatalf("code = %v, want UNSUPPORTED", refusal["code"])
	}
	if message, _ := refusal["message"].(string); !strings.Contains(message, "cwd") || !strings.Contains(message, "beta") {
		t.Fatalf("message does not explain the scope: %q", message)
	}
	if read := mcpResult(t, responses[2]); read["isError"] != false {
		t.Fatalf("reads are unaffected by the write scope: %v", read)
	}
}

func TestMCPAllowWriteWidensTheWriteScope(t *testing.T) {
	env := newMCPPolicyEnv(t)
	code, responses, stderr := env.run(t, []string{"--allow-write"},
		mcpToolCall(1, "brain_note", `{"text":"another project","project":"beta"}`))
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	if got := mcpResult(t, responses[0]); got["isError"] != false {
		t.Fatalf("write with --allow-write refused: %v", got)
	}
	if id := mcpStructured(t, mcpResult(t, responses[0]))["project_id"]; id != "beta" {
		t.Fatalf("project_id = %v", id)
	}
}

func TestMCPAllowWriteIsBoundedByProjects(t *testing.T) {
	env := newMCPPolicyEnv(t)
	code, responses, stderr := env.run(t, []string{"--allow-write", "--projects", "alpha"},
		mcpToolCall(1, "brain_note", `{"text":"x","project":"beta"}`),
		mcpToolCall(2, "brain_note", `{"text":"x","project":"alpha"}`),
	)
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	if got := mcpResult(t, responses[0]); got["isError"] != true {
		t.Fatalf("write outside --projects accepted: %v", got)
	}
	if got := mcpResult(t, responses[1]); got["isError"] != false {
		t.Fatalf("write inside --projects refused: %v", got)
	}
}

func TestMCPReadOnlyWinsOverAllowWrite(t *testing.T) {
	env := newMCPPolicyEnv(t)
	names := env.toolNames(t, []string{"--read-only", "--allow-write"})
	if names["brain_note"] {
		t.Fatal("--read-only --allow-write still lists brain_note")
	}
	code, responses, stderr := env.run(t, []string{"--read-only", "--allow-write"},
		mcpToolCall(1, "brain_note", `{"text":"x","project":"alpha"}`))
	if code != 0 {
		t.Fatalf("runMCP exit code = %d (stderr: %s)", code, stderr)
	}
	if got := mcpResult(t, responses[0]); got["isError"] != true {
		t.Fatalf("write accepted with --read-only --allow-write: %v", got)
	}
}

func TestMCPListToolsReportsTheEffectivePolicy(t *testing.T) {
	code, stdout, stderr := runForTest(t, "mcp", "--list-tools", "--json", "--read-only")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	var catalogue struct {
		Policy struct {
			ReadOnly    bool     `json:"read_only"`
			AllowWrite  bool     `json:"allow_write"`
			Projects    []string `json:"projects"`
			AllProjects bool     `json:"all_projects"`
		} `json:"policy"`
		Tools        []struct{ Name string }
		OmittedTools []string `json:"omitted_tools"`
	}
	if err := json.Unmarshal([]byte(stdout), &catalogue); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if !catalogue.Policy.ReadOnly || catalogue.Policy.AllowWrite || !catalogue.Policy.AllProjects || catalogue.Policy.Projects == nil {
		t.Fatalf("policy = %+v", catalogue.Policy)
	}
	if len(catalogue.OmittedTools) != 7 {
		t.Fatalf("omitted_tools = %v", catalogue.OmittedTools)
	}
	for _, tool := range catalogue.Tools {
		for _, write := range catalogue.OmittedTools {
			if tool.Name == write {
				t.Fatalf("tool %s is both listed and omitted", tool.Name)
			}
		}
	}
	if len(catalogue.Tools)+len(catalogue.OmittedTools) != 17 {
		t.Fatalf("tools = %d listed + %d omitted, want 17", len(catalogue.Tools), len(catalogue.OmittedTools))
	}
}

func TestMCPListToolsDefaultPolicyIsUnrestricted(t *testing.T) {
	code, stdout, stderr := runForTest(t, "mcp", "--list-tools", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	var catalogue struct {
		Policy struct {
			ReadOnly    bool     `json:"read_only"`
			AllowWrite  bool     `json:"allow_write"`
			Projects    []string `json:"projects"`
			AllProjects bool     `json:"all_projects"`
		} `json:"policy"`
		Tools        []map[string]any
		OmittedTools []string `json:"omitted_tools"`
	}
	if err := json.Unmarshal([]byte(stdout), &catalogue); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if catalogue.Policy.ReadOnly || catalogue.Policy.AllowWrite || !catalogue.Policy.AllProjects {
		t.Fatalf("policy = %+v", catalogue.Policy)
	}
	if len(catalogue.Tools) != 17 || len(catalogue.OmittedTools) != 0 {
		t.Fatalf("tools = %d, omitted = %v", len(catalogue.Tools), catalogue.OmittedTools)
	}
}

func TestMCPListToolsEchoesTheRequestedAllowlist(t *testing.T) {
	code, stdout, stderr := runForTest(t, "mcp", "--list-tools", "--json", "--projects", "alpha,beta", "--allow-write")
	if code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr)
	}
	var catalogue struct {
		Policy struct {
			AllowWrite  bool     `json:"allow_write"`
			Projects    []string `json:"projects"`
			AllProjects bool     `json:"all_projects"`
		} `json:"policy"`
	}
	if err := json.Unmarshal([]byte(stdout), &catalogue); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if !catalogue.Policy.AllowWrite || catalogue.Policy.AllProjects || fmt.Sprint(catalogue.Policy.Projects) != "[alpha beta]" {
		t.Fatalf("policy = %+v", catalogue.Policy)
	}
}
