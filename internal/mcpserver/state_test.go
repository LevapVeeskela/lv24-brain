package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type stateFake struct {
	fakeBackend
	state State
	last  StateInput
}

func (f *stateFake) GetState(_ context.Context, projectID string) (State, error) {
	state := f.state
	state.ProjectID = projectID
	return state, nil
}

func (f *stateFake) SetState(_ context.Context, in StateInput) (State, error) {
	f.last = in
	if in.Goal != nil {
		f.state.Goal = *in.Goal
	}
	if in.NextAction != nil {
		f.state.NextAction = *in.NextAction
	}
	if in.OpenQuestions != nil {
		f.state.OpenQuestions = *in.OpenQuestions
	}
	f.state.ProjectID = in.ProjectID
	return f.state, nil
}

func callTool(t *testing.T, backend Backend, name, args string) map[string]any {
	t.Helper()
	request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}` + "\n"
	var out strings.Builder
	if err := New(backend, "test").Serve(context.Background(), strings.NewReader(request), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var response struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &response); err != nil {
		t.Fatalf("decode %q: %v", lines[len(lines)-1], err)
	}
	return response.Result
}

func TestMCPServerStateTools(t *testing.T) {
	backend := &stateFake{state: State{Goal: "old goal", OpenQuestions: []string{"q1"}}}

	got := callTool(t, backend, "brain_state_get", `{"project":"alpha"}`)
	if got["isError"] == true || got["structuredContent"].(map[string]any)["goal"] != "old goal" {
		t.Fatalf("state_get = %+v", got)
	}

	got = callTool(t, backend, "brain_state_set", `{"next":"ship it","open_questions":[]}`)
	if got["isError"] == true {
		t.Fatalf("state_set error: %+v", got)
	}
	if backend.last.ProjectID != "alpha" || backend.last.Goal != nil || backend.last.NextAction == nil ||
		*backend.last.NextAction != "ship it" || backend.last.OpenQuestions == nil || len(*backend.last.OpenQuestions) != 0 {
		t.Fatalf("state_set input = %+v", backend.last)
	}

	if got = callTool(t, backend, "brain_state_set", `{}`); got["isError"] != true {
		t.Fatalf("empty state_set accepted: %+v", got)
	}
	if got = callTool(t, backend, "brain_state_set", `{"goal":"x","project":"nope"}`); got["isError"] != true {
		t.Fatalf("unknown project accepted: %+v", got)
	}
}

func TestMCPServerStateToolsOnlyWithStateBackend(t *testing.T) {
	names := func(backend Backend) string {
		var list []string
		for _, tool := range New(backend, "test").toolList() {
			list = append(list, tool.Name)
		}
		return strings.Join(list, ",")
	}
	if strings.Contains(names(&fakeBackend{}), "brain_state") {
		t.Fatal("state tools listed for a backend without state support")
	}
	if !strings.Contains(names(&stateFake{}), "brain_state_set") {
		t.Fatal("state tools missing for a state backend")
	}
}
