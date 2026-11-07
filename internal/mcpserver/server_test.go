package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeBackend records calls and returns canned data.
type fakeBackend struct {
	notes        []NoteInput
	decisions    []DecisionInput
	attempts     []AttemptInput
	handoffs     []HandoffInput
	journalQuery JournalQuery
	budget       int
	search       SearchInput
}

func (f *fakeBackend) Resolve(_ context.Context, selector string) (Target, error) {
	switch selector {
	case "", "alpha", "Alpha-alias":
		return Target{ID: "alpha", Path: "C:/work/alpha"}, nil
	case "beta":
		return Target{ID: "beta"}, nil
	default:
		return Target{}, fmt.Errorf("%q is not a registered project id or alias", selector)
	}
}

func (f *fakeBackend) Bootstrap(_ context.Context, projectID string, budget int) (string, error) {
	f.budget = budget
	return "# " + projectID + " — bootstrap\nstate: ok", nil
}

func (f *fakeBackend) Search(_ context.Context, in SearchInput) ([]Hit, error) {
	f.search = in
	return []Hit{{ID: "note-alpha-1", ProjectID: "alpha", Kind: "note", Title: "Cache fix", Snippet: "fixed the cache"}}, nil
}

func (f *fakeBackend) Note(_ context.Context, in NoteInput) (Record, error) {
	f.notes = append(f.notes, in)
	return Record{ID: "note-" + in.ProjectID + "-1", ProjectID: in.ProjectID}, nil
}

func (f *fakeBackend) Decision(_ context.Context, in DecisionInput) (Record, error) {
	f.decisions = append(f.decisions, in)
	return Record{ID: "decision-" + in.ProjectID + "-1", ProjectID: in.ProjectID}, nil
}

func (f *fakeBackend) Attempt(_ context.Context, in AttemptInput) (Record, error) {
	f.attempts = append(f.attempts, in)
	return Record{ID: "attempt-" + in.ProjectID + "-1", ProjectID: in.ProjectID}, nil
}

func (f *fakeBackend) Handoff(_ context.Context, in HandoffInput) (Record, error) {
	f.handoffs = append(f.handoffs, in)
	return Record{ID: "handoff-" + in.ProjectID + "-1", ProjectID: in.ProjectID}, nil
}

func (f *fakeBackend) Journal(_ context.Context, query JournalQuery) ([]JournalEntry, error) {
	f.journalQuery = query
	return []JournalEntry{{
		Time:    time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC),
		User:    "veeskela",
		Event:   "note",
		Project: "alpha",
		ID:      "note-alpha-1",
		Title:   "Cache fix",
	}}, nil
}

func (f *fakeBackend) Projects(context.Context) ([]Project, error) {
	return []Project{{ID: "alpha", Name: "Alpha", Kind: "repository", Paths: []string{"C:/work/alpha"}, RepoURL: "https://example.test/alpha.git"}}, nil
}

// session feeds messages to a fresh server and returns the decoded responses.
func session(t *testing.T, backend Backend, messages ...string) []map[string]any {
	t.Helper()
	server := New(backend, "test-version")
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(messages, "\n") + "\n")
	if err := server.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var responses []map[string]any
	for _, line := range strings.Split(out.String(), "\n") {
		if line == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", line, err)
		}
		responses = append(responses, msg)
	}
	return responses
}

func callMessage(id int, name string, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
}

// toolCall runs one tools/call and returns its result object.
func toolCall(t *testing.T, backend Backend, name, args string) map[string]any {
	t.Helper()
	responses := session(t, backend, callMessage(1, name, args))
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1", len(responses))
	}
	if errObj, ok := responses[0]["error"]; ok {
		t.Fatalf("unexpected JSON-RPC error: %v", errObj)
	}
	result, ok := responses[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing: %v", responses[0])
	}
	return result
}

func errorOf(t *testing.T, response map[string]any) (int, string) {
	t.Helper()
	errObj, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error, got %v", response)
	}
	code, _ := errObj["code"].(float64)
	message, _ := errObj["message"].(string)
	return int(code), message
}

func structured(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	obj, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("structuredContent = %v, want object", result["structuredContent"])
	}
	return obj
}

func TestMCPServerInitializeEchoesSupportedVersion(t *testing.T) {
	for _, version := range []string{"2025-03-26", "2024-11-05", "2025-06-18"} {
		responses := session(t, &fakeBackend{}, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, version))
		result := responses[0]["result"].(map[string]any)
		if got := result["protocolVersion"]; got != version {
			t.Fatalf("protocolVersion for %s = %v", version, got)
		}
	}
}

func TestMCPServerInitializeFallsBackForUnknownVersion(t *testing.T) {
	responses := session(t, &fakeBackend{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	result := responses[0]["result"].(map[string]any)
	if got := result["protocolVersion"]; got != ProtocolVersion {
		t.Fatalf("protocolVersion = %v, want %s", got, ProtocolVersion)
	}
}

func TestMCPServerInitializeResultShape(t *testing.T) {
	responses := session(t, &fakeBackend{}, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if responses[0]["id"] != "init" {
		t.Fatalf("id not echoed: %v", responses[0]["id"])
	}
	result := responses[0]["result"].(map[string]any)
	caps := result["capabilities"].(map[string]any)
	tools := caps["tools"].(map[string]any)
	if tools["listChanged"] != false {
		t.Fatalf("tools.listChanged = %v, want false", tools["listChanged"])
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "lv24-brain" || info["version"] != "test-version" {
		t.Fatalf("serverInfo = %v", info)
	}
	instructions, _ := result["instructions"].(string)
	for _, want := range []string{"brain_open", "brain_handoff"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("instructions missing %q: %s", want, instructions)
		}
	}
}

func TestMCPServerNotificationsGetNoResponse(t *testing.T) {
	responses := session(t, &fakeBackend{},
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"brain_note","arguments":{"text":"x"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
	)
	if len(responses) != 1 {
		t.Fatalf("responses = %d, want 1 (notifications must not be answered or run)", len(responses))
	}
	if responses[0]["id"] != float64(7) {
		t.Fatalf("response id = %v, want 7", responses[0]["id"])
	}
}

func TestMCPServerPing(t *testing.T) {
	responses := session(t, &fakeBackend{}, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	result, ok := responses[0]["result"].(map[string]any)
	if !ok || len(result) != 0 {
		t.Fatalf("ping result = %v, want {}", responses[0]["result"])
	}
}

func TestMCPServerToolsListSchemas(t *testing.T) {
	responses := session(t, &fakeBackend{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := responses[0]["result"].(map[string]any)["tools"].([]any)
	want := []string{"brain_open", "brain_search", "brain_note", "brain_decision", "brain_attempt", "brain_handoff", "brain_journal", "brain_projects"}
	if len(tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(tools), len(want))
	}
	for i, raw := range tools {
		tool := raw.(map[string]any)
		if tool["name"] != want[i] {
			t.Fatalf("tool %d = %v, want %s", i, tool["name"], want[i])
		}
		if desc, _ := tool["description"].(string); desc == "" {
			t.Fatalf("%s has no description", want[i])
		}
		schema := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Fatalf("%s schema = %v", want[i], schema)
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s properties missing", want[i])
		}
		required, _ := schema["required"].([]any)
		for _, name := range required {
			if _, ok := props[name.(string)]; !ok {
				t.Fatalf("%s requires undeclared property %v", want[i], name)
			}
		}
	}
}

func TestMCPServerToolsCallOpen(t *testing.T) {
	backend := &fakeBackend{}
	result := toolCall(t, backend, "brain_open", `{"project":"beta","budget":500}`)
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	content := result["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || !strings.Contains(content["text"].(string), "beta — bootstrap") {
		t.Fatalf("content = %v", content)
	}
	obj := structured(t, result)
	if obj["project_id"] != "beta" || obj["summary"] == "" {
		t.Fatalf("structured = %v", obj)
	}
	if backend.budget != 500 {
		t.Fatalf("budget = %d", backend.budget)
	}
}

func TestMCPServerToolsCallOpenDefaultsToWorkingDirectory(t *testing.T) {
	result := toolCall(t, &fakeBackend{}, "brain_open", `{}`)
	obj := structured(t, result)
	if obj["project_id"] != "alpha" || obj["path"] != "C:/work/alpha" {
		t.Fatalf("structured = %v", obj)
	}
}

func TestMCPServerToolsCallSearch(t *testing.T) {
	backend := &fakeBackend{}
	result := toolCall(t, backend, "brain_search", `{"query":"cache","project":"alpha","limit":5}`)
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	hits := structured(t, result)["hits"].([]any)
	hit := hits[0].(map[string]any)
	if hit["id"] != "note-alpha-1" || hit["project_id"] != "alpha" || hit["kind"] != "note" || hit["snippet"] == "" {
		t.Fatalf("hit = %v", hit)
	}
	if backend.search.Query != "cache" || backend.search.ProjectID != "alpha" || backend.search.Limit != 5 {
		t.Fatalf("search call = %+v", backend.search)
	}
	if len(backend.search.Kinds) != 0 || backend.search.IncludeDumps {
		t.Fatalf("default search passed filters = %+v", backend.search)
	}
}

func TestMCPServerToolsCallSearchWithoutProjectSearchesAll(t *testing.T) {
	backend := &fakeBackend{}
	toolCall(t, backend, "brain_search", `{"query":"cache"}`)
	if backend.search.ProjectID != "" {
		t.Fatalf("project = %q, want all projects", backend.search.ProjectID)
	}
}

func TestMCPServerToolsCallSearchPassesKindsAndDumps(t *testing.T) {
	backend := &fakeBackend{}
	toolCall(t, backend, "brain_search", `{"query":"cache","kinds":["note","dump"],"include_dumps":true}`)
	if got := backend.search.Kinds; len(got) != 2 || got[0] != "note" || got[1] != "dump" {
		t.Fatalf("Kinds = %v, want [note dump]", got)
	}
	if !backend.search.IncludeDumps {
		t.Fatalf("IncludeDumps = false, want true")
	}
}

func TestMCPServerToolsCallSearchRejectsUnknownArgs(t *testing.T) {
	backend := &fakeBackend{}
	result := toolCall(t, backend, "brain_search", `{"query":"cache","kinds":"note"}`)
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true for non-array kinds", result["isError"])
	}
}

func TestMCPServerToolsCallNote(t *testing.T) {
	backend := &fakeBackend{}
	result := toolCall(t, backend, "brain_note", `{"text":"Fixed the cache","title":"Cache","project":"beta"}`)
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	obj := structured(t, result)
	if obj["id"] != "note-beta-1" || obj["project_id"] != "beta" {
		t.Fatalf("structured = %v", obj)
	}
	if len(backend.notes) != 1 || backend.notes[0].Body != "Fixed the cache" || backend.notes[0].Title != "Cache" || backend.notes[0].ProjectID != "beta" {
		t.Fatalf("notes = %+v", backend.notes)
	}
}

func TestMCPServerToolsCallDecision(t *testing.T) {
	backend := &fakeBackend{}
	result := toolCall(t, backend, "brain_decision",
		`{"title":"Adopt Go","context":"speed","alternatives":["Rust","Node"],"consequences":["one binary"],"status":"accepted"}`)
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	if obj := structured(t, result); obj["id"] != "decision-alpha-1" {
		t.Fatalf("structured = %v", obj)
	}
	got := backend.decisions[0]
	if got.Title != "Adopt Go" || got.Status != "accepted" || len(got.Alternatives) != 2 || got.Consequences[0] != "one binary" || got.ProjectID != "alpha" {
		t.Fatalf("decision input = %+v", got)
	}
}

func TestMCPServerToolsCallAttempt(t *testing.T) {
	backend := &fakeBackend{}
	result := toolCall(t, backend, "brain_attempt", `{"title":"Try cache","hypothesis":"faster","result":"no gain","reason":"reverted"}`)
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	if obj := structured(t, result); obj["id"] != "attempt-alpha-1" || obj["project_id"] != "alpha" {
		t.Fatalf("structured = %v", obj)
	}
	if backend.attempts[0].Reason != "reverted" {
		t.Fatalf("attempt input = %+v", backend.attempts[0])
	}
}

func TestMCPServerToolsCallHandoff(t *testing.T) {
	backend := &fakeBackend{}
	result := toolCall(t, backend, "brain_handoff", `{"summary":"Session done","next":"ship it"}`)
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	if obj := structured(t, result); obj["id"] != "handoff-alpha-1" {
		t.Fatalf("structured = %v", obj)
	}
	if backend.handoffs[0].NextAction != "ship it" || backend.handoffs[0].Summary != "Session done" {
		t.Fatalf("handoff input = %+v", backend.handoffs[0])
	}
}

func TestMCPServerToolsCallJournal(t *testing.T) {
	backend := &fakeBackend{}
	result := toolCall(t, backend, "brain_journal", `{"project":"alpha"}`)
	if result["isError"] != false {
		t.Fatalf("isError = %v", result["isError"])
	}
	entries := structured(t, result)["entries"].([]any)
	entry := entries[0].(map[string]any)
	if entry["event"] != "note" || entry["id"] != "note-alpha-1" || entry["time"] != "2026-11-07T10:00:00Z" {
		t.Fatalf("entry = %v", entry)
	}
	if backend.journalQuery.Limit != DefaultJournalLimit || backend.journalQuery.ProjectID != "alpha" {
		t.Fatalf("journal query = %+v", backend.journalQuery)
	}
}

func TestMCPServerToolsCallProjects(t *testing.T) {
	result := toolCall(t, &fakeBackend{}, "brain_projects", `{}`)
	projects := structured(t, result)["projects"].([]any)
	project := projects[0].(map[string]any)
	if project["id"] != "alpha" || project["repo_url"] != "https://example.test/alpha.git" {
		t.Fatalf("project = %v", project)
	}
}

func TestMCPServerToolFailuresAreReportedAsIsError(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"missing required", `{"project":"alpha"}`, "text is required"},
		{"blank required", `{"text":"   "}`, "text is required"},
		{"unknown field", `{"text":"x","colour":"red"}`, "unknown field"},
		{"wrong type", `{"text":5}`, "invalid arguments"},
		{"unregistered project", `{"text":"x","project":"ghost"}`, "not a registered project"},
		{"bad status", `{"title":"t","status":"maybe"}`, "status must be one of"},
		{"negative limit", `{"query":"q","limit":-1}`, "limit must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeBackend{}
			tool := "brain_note"
			if strings.Contains(tc.name, "status") {
				tool = "brain_decision"
			}
			if strings.Contains(tc.name, "limit") {
				tool = "brain_search"
			}
			result := toolCall(t, backend, tool, tc.args)
			if result["isError"] != true {
				t.Fatalf("isError = %v, want true", result["isError"])
			}
			text := result["content"].([]any)[0].(map[string]any)["text"].(string)
			if !strings.Contains(text, tc.want) {
				t.Fatalf("text = %q, want it to contain %q", text, tc.want)
			}
			if len(backend.notes) != 0 || len(backend.decisions) != 0 {
				t.Fatalf("a failed call must not write: %+v %+v", backend.notes, backend.decisions)
			}
		})
	}
}

func TestMCPServerUnresolvedProjectNeverWrites(t *testing.T) {
	backend := &fakeBackend{}
	result := toolCall(t, backend, "brain_handoff", `{"summary":"s","project":"ghost"}`)
	if result["isError"] != true {
		t.Fatalf("isError = %v, want true", result["isError"])
	}
	if len(backend.handoffs) != 0 {
		t.Fatalf("handoff written for unresolved project")
	}
}

func TestMCPServerUnknownToolIsInvalidParams(t *testing.T) {
	responses := session(t, &fakeBackend{}, callMessage(3, "brain_delete", `{}`))
	code, message := errorOf(t, responses[0])
	if code != -32602 || !strings.Contains(message, "brain_delete") {
		t.Fatalf("error = %d %q", code, message)
	}
}

func TestMCPServerToolsCallWithBadParamsIsInvalidParams(t *testing.T) {
	cases := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"nope"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`,
	}
	for _, msg := range cases {
		responses := session(t, &fakeBackend{}, msg)
		if code, _ := errorOf(t, responses[0]); code != -32602 {
			t.Fatalf("code = %d for %s, want -32602", code, msg)
		}
	}
}

func TestMCPServerInitializeBadParamsIsInvalidParams(t *testing.T) {
	responses := session(t, &fakeBackend{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":[1]}`)
	if code, _ := errorOf(t, responses[0]); code != -32602 {
		t.Fatalf("code = %d, want -32602", code)
	}
}

func TestMCPServerUnknownMethodIsMethodNotFound(t *testing.T) {
	responses := session(t, &fakeBackend{}, `{"jsonrpc":"2.0","id":9,"method":"resources/list"}`)
	code, _ := errorOf(t, responses[0])
	if code != -32601 || responses[0]["id"] != float64(9) {
		t.Fatalf("response = %v", responses[0])
	}
}

func TestMCPServerMalformedJSONIsParseError(t *testing.T) {
	responses := session(t, &fakeBackend{}, `{"jsonrpc":"2.0","id":1,`, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if len(responses) != 2 {
		t.Fatalf("responses = %d, want 2", len(responses))
	}
	code, _ := errorOf(t, responses[0])
	if code != -32700 || responses[0]["id"] != nil {
		t.Fatalf("parse error response = %v", responses[0])
	}
	if responses[1]["id"] != float64(2) {
		t.Fatalf("server did not continue after a parse error: %v", responses[1])
	}
}

func TestMCPServerNonObjectMessageIsInvalidRequest(t *testing.T) {
	responses := session(t, &fakeBackend{}, `[1,2]`)
	if code, _ := errorOf(t, responses[0]); code != -32600 {
		t.Fatalf("code = %d, want -32600", code)
	}
}

func TestMCPServerStdoutContainsOnlyJSONLines(t *testing.T) {
	server := New(&fakeBackend{}, "test-version")
	var out bytes.Buffer
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`garbage`,
		``,
		callMessage(3, "brain_projects", `{}`),
	}, "\n"))
	if err := server.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines = %d, want 4:\n%s", len(lines), out.String())
	}
	for _, line := range lines {
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("non-JSON line on stdout: %q", line)
		}
		if msg["jsonrpc"] != "2.0" {
			t.Fatalf("missing jsonrpc in %q", line)
		}
	}
}

func TestMCPServerServeStopsOnWriteError(t *testing.T) {
	server := New(&fakeBackend{}, "test-version")
	err := server.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), failingWriter{})
	if !errors.Is(err, errWrite) {
		t.Fatalf("err = %v, want write error", err)
	}
}

var errWrite = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

func TestMCPServerServeReturnsOnCancelledContext(t *testing.T) {
	server := New(&fakeBackend{}, "test-version")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.Serve(ctx, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
