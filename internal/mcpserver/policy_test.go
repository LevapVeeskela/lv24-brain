package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// policyFake is the two-project backend the policy tests use: it reports alpha
// and beta, records every write, and can pretend the working directory is a
// workspace root.
type policyFake struct {
	fullFake
	reviewed  []ReviewRecordInput
	workspace bool
}

func (f *policyFake) Resolve(ctx context.Context, selector string) (Target, error) {
	if f.workspace && selector == "" {
		return Target{}, &WorkspaceOverview{Text: "# workspace\nalpha\nbeta", Message: "the working directory holds several projects"}
	}
	return f.fullFake.Resolve(ctx, selector)
}

func (f *policyFake) Projects(context.Context) ([]Project, error) {
	return []Project{
		{ID: "alpha", Name: "Alpha", Kind: "repository", Paths: []string{"C:/work/alpha"}},
		{ID: "beta", Name: "Beta", Kind: "repository", Paths: []string{"C:/work/beta"}},
	}, nil
}

func (f *policyFake) ReviewRecord(_ context.Context, in ReviewRecordInput) (ReviewRecordResult, error) {
	f.reviewed = append(f.reviewed, in)
	return ReviewRecordResult{ProjectID: in.ProjectID, Reviewed: []RecordSummary{{
		Kind: "decision", ID: in.ID, ProjectID: in.ProjectID, Title: "Use fsstore",
	}}}, nil
}

// policyCall runs one legacy tools/call against a server with a policy and
// returns the result object.
func policyCall(t *testing.T, backend Backend, policy Policy, name, args string) map[string]any {
	t.Helper()
	request := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}` + "\n"
	var out strings.Builder
	if err := NewWithPolicy(backend, "test", policy).Serve(context.Background(), strings.NewReader(request), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var response struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &response); err != nil {
		t.Fatalf("decode %q: %v", lines[len(lines)-1], err)
	}
	if response.Error != nil {
		t.Fatalf("%s answered a JSON-RPC error: %v", name, response.Error)
	}
	return response.Result
}

// policyTools lists the tools a server with a policy reports.
func policyTools(t *testing.T, backend Backend, policy Policy) map[string]bool {
	t.Helper()
	responses := policySession(t, backend, policy, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	names := map[string]bool{}
	for _, raw := range resultOf(t, responses[0])["tools"].([]any) {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	return names
}

// policySession feeds messages to a server with a policy.
func policySession(t *testing.T, backend Backend, policy Policy, messages ...string) []map[string]any {
	t.Helper()
	var out strings.Builder
	if err := NewWithPolicy(backend, "test", policy).Serve(context.Background(), strings.NewReader(strings.Join(messages, "\n")+"\n"), &out); err != nil {
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

// writeTools are every tool whose annotations say it writes.
var writeTools = []string{
	"brain_note", "brain_decision", "brain_attempt", "brain_handoff",
	"brain_state_set", "brain_decision_status", "brain_review_record",
}

func TestReadOnlyPolicyOmitsEveryWriteTool(t *testing.T) {
	names := policyTools(t, &policyFake{}, Policy{ReadOnly: true})
	for _, tool := range writeTools {
		if names[tool] {
			t.Fatalf("read-only server lists write tool %s", tool)
		}
	}
	for _, tool := range []string{"brain_open", "brain_context", "brain_search", "brain_records", "brain_record"} {
		if !names[tool] {
			t.Fatalf("read-only server dropped read tool %s", tool)
		}
	}
	// Nothing was hidden by accident: the same backend lists every write tool
	// without the policy.
	all := policyTools(t, &policyFake{}, Policy{})
	for _, tool := range writeTools {
		if !all[tool] {
			t.Fatalf("unrestricted server dropped %s", tool)
		}
	}
}

func TestReadOnlyPolicyRefusesHiddenWriteTool(t *testing.T) {
	backend := &policyFake{}
	result := policyCall(t, backend, Policy{ReadOnly: true}, "brain_note", `{"text":"poison","project":"alpha"}`)
	if result["isError"] != true {
		t.Fatalf("brain_note was not refused: %v", result)
	}
	refusal := structured(t, result)
	if refusal["code"] != RefusalUnsupported {
		t.Fatalf("code = %v, want %s", refusal["code"], RefusalUnsupported)
	}
	tryInstead, _ := refusal["try_instead"].(string)
	if !strings.Contains(tryInstead, "brain_search") || !strings.Contains(tryInstead, "brain_context") {
		t.Fatalf("try_instead = %q", tryInstead)
	}
	if len(backend.notes) != 0 {
		t.Fatalf("the refused write reached the backend: %+v", backend.notes)
	}
}

func TestUnknownToolIsStillAProtocolError(t *testing.T) {
	responses := policySession(t, &policyFake{}, Policy{ReadOnly: true},
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"brain_bogus","arguments":{}}}`)
	if _, ok := responses[0]["error"]; !ok {
		t.Fatalf("unknown tool answered a result: %v", responses[0])
	}
}

func TestProjectAllowlistGovernsReadsAndWrites(t *testing.T) {
	policy := Policy{Projects: []string{"alpha"}}

	open := policyCall(t, &policyFake{}, policy, "brain_open", `{"project":"beta"}`)
	if open["isError"] != true {
		t.Fatalf("reading an unlisted project was accepted: %v", open)
	}
	refusal := structured(t, open)
	if refusal["code"] != RefusalInvalidArguments {
		t.Fatalf("code = %v, want %s", refusal["code"], RefusalInvalidArguments)
	}
	if message, _ := refusal["message"].(string); !strings.Contains(message, "alpha") {
		t.Fatalf("message does not name the allowed set: %q", message)
	}

	listed := policyCall(t, &policyFake{}, policy, "brain_open", `{"project":"alpha"}`)
	if listed["isError"] != false {
		t.Fatalf("allowed project refused: %v", listed)
	}

	write := policyCall(t, &policyFake{}, Policy{Projects: []string{"alpha"}, RestrictWrites: true, AllowWrite: true}, "brain_note", `{"text":"x","project":"beta"}`)
	if write["isError"] != true || structured(t, write)["code"] != RefusalInvalidArguments {
		t.Fatalf("write outside the allowlist = %v", write)
	}
}

func TestProjectAllowlistFiltersTheProjectList(t *testing.T) {
	result := policyCall(t, &policyFake{}, Policy{Projects: []string{"alpha"}}, "brain_projects", `{}`)
	projects, _ := structured(t, result)["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("projects = %v, want only alpha", projects)
	}
	if id := projects[0].(map[string]any)["id"]; id != "alpha" {
		t.Fatalf("project = %v", id)
	}
}

func TestUnscopedReadUsesTheSingleAllowedProject(t *testing.T) {
	backend := &policyFake{}
	result := policyCall(t, backend, Policy{Projects: []string{"alpha"}}, "brain_search", `{"query":"cache"}`)
	if result["isError"] != false {
		t.Fatalf("search = %v", result)
	}
	if backend.search.ProjectID != "alpha" {
		t.Fatalf("search project = %q, want alpha", backend.search.ProjectID)
	}
}

func TestUnscopedReadWithSeveralProjectsNeedsExplicitProject(t *testing.T) {
	result := policyCall(t, &policyFake{}, Policy{Projects: []string{"alpha", "beta"}}, "brain_search", `{"query":"cache"}`)
	if result["isError"] != true {
		t.Fatalf("unscoped search was accepted with two allowed projects: %v", result)
	}
	refusal := structured(t, result)
	if refusal["code"] != RefusalInvalidArguments {
		t.Fatalf("code = %v", refusal["code"])
	}
	if message, _ := refusal["message"].(string); !strings.Contains(message, "alpha, beta") {
		t.Fatalf("message does not name both projects: %q", message)
	}
	explicit := policyCall(t, &policyFake{}, Policy{Projects: []string{"alpha", "beta"}}, "brain_search", `{"query":"cache","project":"beta"}`)
	if explicit["isError"] != false {
		t.Fatalf("explicit project refused: %v", explicit)
	}
}

func TestWorkspaceOverviewIsRefusedUnderAnAllowlist(t *testing.T) {
	result := policyCall(t, &policyFake{workspace: true}, Policy{Projects: []string{"alpha"}}, "brain_open", `{}`)
	if result["isError"] != true || structured(t, result)["code"] != RefusalInvalidArguments {
		t.Fatalf("workspace overview under an allowlist = %v", result)
	}
	overview := policyCall(t, &policyFake{workspace: true}, Policy{}, "brain_open", `{}`)
	if overview["isError"] != false {
		t.Fatalf("workspace overview without a policy = %v", overview)
	}
}

func TestWriteScopeLimitsWritesToTheWorkingDirectoryProject(t *testing.T) {
	policy := Policy{RestrictWrites: true, DefaultProject: "alpha"}
	backend := &policyFake{}

	got := policyCall(t, backend, policy, "brain_note", `{"text":"x","project":"alpha"}`)
	if got["isError"] != false {
		t.Fatalf("write to the working-directory project refused: %v", got)
	}
	blocked := policyCall(t, backend, policy, "brain_note", `{"text":"x","project":"beta"}`)
	if blocked["isError"] != true {
		t.Fatalf("write to another project accepted: %v", blocked)
	}
	refusal := structured(t, blocked)
	if refusal["code"] != RefusalUnsupported {
		t.Fatalf("code = %v, want %s", refusal["code"], RefusalUnsupported)
	}
	if message, _ := refusal["message"].(string); !strings.Contains(message, "alpha") || !strings.Contains(message, "beta") {
		t.Fatalf("message does not explain the scope: %q", message)
	}
	if len(backend.notes) != 1 {
		t.Fatalf("writes = %+v, want only the allowed one", backend.notes)
	}
}

func TestWriteScopeAppliesToEveryWriteTool(t *testing.T) {
	calls := map[string]string{
		"brain_note":            `{"text":"x","project":"beta"}`,
		"brain_decision":        `{"title":"x","project":"beta"}`,
		"brain_attempt":         `{"title":"x","project":"beta"}`,
		"brain_handoff":         `{"summary":"x","project":"beta"}`,
		"brain_state_set":       `{"goal":"x","project":"beta"}`,
		"brain_decision_status": `{"id":"decision-alpha-1","status":"accepted","project":"beta"}`,
		"brain_review_record":   `{"id":"decision-alpha-1","project":"beta"}`,
	}
	policy := Policy{RestrictWrites: true, DefaultProject: "alpha"}
	for name, args := range calls {
		backend := &policyFake{}
		result := policyCall(t, backend, policy, name, args)
		if result["isError"] != true {
			t.Fatalf("%s outside the write scope was accepted", name)
		}
		if code := structured(t, result)["code"]; code != RefusalUnsupported {
			t.Fatalf("%s code = %v, want %s", name, code, RefusalUnsupported)
		}
		if len(backend.notes)+len(backend.decisions)+len(backend.attempts)+len(backend.handoffs) != 0 ||
			len(backend.statusChanges) != 0 || len(backend.reviewed) != 0 || backend.last.ProjectID != "" {
			t.Fatalf("%s reached the backend", name)
		}
	}
}

func TestAllowWriteWidensTheWriteScope(t *testing.T) {
	policy := Policy{RestrictWrites: true, DefaultProject: "alpha", AllowWrite: true}
	backend := &policyFake{}
	if got := policyCall(t, backend, policy, "brain_note", `{"text":"x","project":"beta"}`); got["isError"] != false {
		t.Fatalf("write with --allow-write refused: %v", got)
	}
	if len(backend.notes) != 1 || backend.notes[0].ProjectID != "beta" {
		t.Fatalf("notes = %+v", backend.notes)
	}
}

func TestWriteScopeWithoutAWorkingDirectoryProject(t *testing.T) {
	policy := Policy{RestrictWrites: true}
	backend := &policyFake{}
	got := policyCall(t, backend, policy, "brain_note", `{"text":"x","project":"alpha"}`)
	if got["isError"] != true {
		t.Fatalf("write without a default project accepted: %v", got)
	}
	refusal := structured(t, got)
	if refusal["code"] != RefusalUnsupported {
		t.Fatalf("code = %v", refusal["code"])
	}
	if message, _ := refusal["message"].(string); !strings.Contains(message, "--allow-write") {
		t.Fatalf("message does not point at --allow-write: %q", message)
	}
	if len(backend.notes) != 0 {
		t.Fatalf("refused write reached the backend: %+v", backend.notes)
	}
	allowed := Policy{RestrictWrites: true, AllowWrite: true}
	if got := policyCall(t, &policyFake{}, allowed, "brain_note", `{"text":"x","project":"beta"}`); got["isError"] != false {
		t.Fatalf("write with --allow-write and no default project refused: %v", got)
	}
}

func TestReadOnlyWinsOverAllowWrite(t *testing.T) {
	policy := Policy{ReadOnly: true, AllowWrite: true, RestrictWrites: true, DefaultProject: "alpha"}
	if info := policy.Info(); info.AllowWrite {
		t.Fatalf("Info reports allow_write under --read-only: %+v", info)
	}
	if policy.allowsWrite("alpha") || policy.allowsWrite("beta") {
		t.Fatal("read-only policy allowed a write")
	}
}

func TestPolicyInfoShape(t *testing.T) {
	info := Policy{}.Info()
	if info.ReadOnly || info.AllowWrite || !info.AllProjects || info.Projects == nil {
		t.Fatalf("default policy info = %+v", info)
	}
	info = Policy{ReadOnly: true, Projects: []string{"alpha", "beta"}}.Info()
	if !info.ReadOnly || info.AllProjects || !slices.Equal(info.Projects, []string{"alpha", "beta"}) {
		t.Fatalf("restricted policy info = %+v", info)
	}
}

func TestCatalogForPolicySplitsVisibleAndOmitted(t *testing.T) {
	visible, omitted := CatalogForPolicy("test", Policy{ReadOnly: true})
	if len(visible)+len(omitted) != len(Catalog("test")) {
		t.Fatalf("catalogue lost tools: %d visible, %d omitted", len(visible), len(omitted))
	}
	for _, tool := range writeTools {
		if !slices.Contains(omitted, tool) {
			t.Fatalf("%s is not omitted: %v", tool, omitted)
		}
	}
	for _, spec := range visible {
		if !spec.Annotations.ReadOnlyHint {
			t.Fatalf("read-only catalogue lists %s", spec.Name)
		}
	}
	visible, omitted = CatalogForPolicy("test", Policy{})
	if len(omitted) != 0 || len(visible) != len(Catalog("test")) {
		t.Fatalf("unrestricted catalogue = %d visible, %d omitted", len(visible), len(omitted))
	}
}

func TestReadOnlyToolListIsNotPubliclyCacheable(t *testing.T) {
	readOnly := policySession(t, &policyFake{}, Policy{ReadOnly: true},
		modernRequest(1, "tools/list", "", modernMeta))
	if scope := resultOf(t, readOnly[0])["cacheScope"]; scope != cacheScopePrivate {
		t.Fatalf("read-only tools/list cacheScope = %v", scope)
	}
	writable := policySession(t, &policyFake{}, Policy{},
		modernRequest(1, "tools/list", "", modernMeta))
	if scope := resultOf(t, writable[0])["cacheScope"]; scope != cacheScopePublic {
		t.Fatalf("writable tools/list cacheScope = %v", scope)
	}
}
