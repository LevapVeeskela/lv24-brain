package mcpserver

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// surfaceFake implements every optional backend interface, so the merged
// actions can be routed and observed without a data directory.
type surfaceFake struct {
	quarantineReviewFake
	historyCalls  []string
	timelineCalls []string
	dueCalls      []string
	doctorCalls   int
	registerCalls []ProjectInput
	promptsOn     bool
	promptSaves   []PromptInput
	promptList    []PromptHit
}

func (f *surfaceFake) GetRecord(_ context.Context, projectID, id string) (RecordDetail, error) {
	return RecordDetail{
		Kind: "decision", ID: id, ProjectID: projectID, Title: "Use fsstore", Status: "proposed",
		Links: []RecordLink{{
			Field: "evidence", Ref: "note-alpha-1", ProjectID: projectID, ID: "note-alpha-1",
			State: "found", Kind: "note", Title: "cache",
		}},
		Backlinks: []RecordBacklink{},
	}, nil
}

func (f *surfaceFake) RecordHistory(_ context.Context, projectID, id string) ([]RecordSummary, error) {
	f.historyCalls = append(f.historyCalls, projectID+"/"+id)
	return []RecordSummary{
		{Kind: "decision", ID: id, ProjectID: projectID, Title: "old"},
		{Kind: "decision", ID: "decision-alpha-2", ProjectID: projectID, Title: "new"},
	}, nil
}

func (f *surfaceFake) Timeline(_ context.Context, projectID, id string, window int) (Timeline, error) {
	f.timelineCalls = append(f.timelineCalls, projectID+"/"+id)
	anchor := RecordSummary{Kind: "decision", ID: id, ProjectID: projectID, Title: "anchor"}
	return Timeline{
		Anchor: anchor,
		Records: []RecordSummary{
			{Kind: "decision", ID: "decision-alpha-0", ProjectID: projectID, Title: "before"},
			anchor,
			{Kind: "decision", ID: "decision-alpha-2", ProjectID: projectID, Title: "after"},
		},
		AnchorIndex: 1, Before: 1, After: 1, Total: 3,
	}, nil
}

func (f *surfaceFake) DueReviews(_ context.Context, projectID string) ([]DueReview, error) {
	f.dueCalls = append(f.dueCalls, projectID)
	return []DueReview{{
		Kind: "decision", ID: "decision-alpha-1", Title: "Use fsstore",
		ReviewAfter: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), OverdueDays: 14, Source: "frontmatter",
	}}, nil
}

func (f *surfaceFake) Doctor(context.Context) ([]DoctorCheck, error) {
	f.doctorCalls++
	return []DoctorCheck{{Name: "schema", Status: "ok", Message: "schema 1"}}, nil
}

func (f *surfaceFake) RegisterProject(_ context.Context, in ProjectInput) (Project, error) {
	f.registerCalls = append(f.registerCalls, in)
	return Project{ID: in.ID, Name: in.ID, Kind: "other", Paths: in.Paths}, nil
}

func (f *surfaceFake) PromptsEnabled() bool { return f.promptsOn }

func (f *surfaceFake) SavePrompt(_ context.Context, in PromptInput) (PromptHit, error) {
	f.promptSaves = append(f.promptSaves, in)
	return PromptHit{ProjectID: in.ProjectID, Text: in.Text, Session: in.Session, At: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)}, nil
}

func (f *surfaceFake) ListPrompts(context.Context, PromptQuery) ([]PromptHit, error) {
	return f.promptList, nil
}

func newSurfaceFake() *surfaceFake {
	return &surfaceFake{
		promptsOn: true,
		promptList: []PromptHit{
			{ProjectID: "alpha", Text: "remember the handoff", At: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)},
			{ProjectID: "alpha", Text: "unrelated", At: time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)},
		},
	}
}

var nativeToolNames = []string{
	"brain_open", "brain_context", "brain_search", "brain_record", "brain_note",
	"brain_decision", "brain_attempt", "brain_handoff", "brain_state", "brain_catchup",
	"brain_review", "brain_judge", "brain_projects", "brain_prompt", "brain_code",
	"brain_graph", "brain_capabilities",
}

var retiredToolNames = []string{
	"brain_state_get", "brain_state_set", "brain_journal", "brain_records",
	"brain_relations", "brain_review_record", "brain_decision_status",
}

func TestSurfaceListsTheNativeTools(t *testing.T) {
	result := listToolsWith(t, newSurfaceFake())
	tools := result["tools"].([]any)
	if len(tools) != len(nativeToolNames) {
		t.Fatalf("tools = %d, want %d", len(tools), len(nativeToolNames))
	}
	for index, raw := range tools {
		tool := raw.(map[string]any)
		if tool["name"] != nativeToolNames[index] {
			t.Fatalf("tool %d = %v, want %s", index, tool["name"], nativeToolNames[index])
		}
		if title, _ := tool["title"].(string); title == "" {
			t.Fatalf("%s has no title", tool["name"])
		}
		annotations := tool["annotations"].(map[string]any)
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := annotations[hint].(bool); !ok {
				t.Fatalf("%s annotations.%s is not a boolean", tool["name"], hint)
			}
		}
		if schema, ok := tool["outputSchema"].(map[string]any); !ok || schema["type"] != "object" {
			t.Fatalf("%s outputSchema = %v", tool["name"], tool["outputSchema"])
		}
	}
}

func TestSurfaceRetiredNamesAreNotFound(t *testing.T) {
	listed := toolNameSet(listToolsWith(t, newSurfaceFake()))
	for _, name := range retiredToolNames {
		if listed[name] {
			t.Fatalf("%s is still listed in the default profile", name)
		}
		result := toolCall(t, newSurfaceFake(), name, `{}`)
		if result["isError"] != true {
			t.Fatalf("%s was not refused: %v", name, result)
		}
		refusal := structured(t, result)
		if refusal["code"] != RefusalNotFound {
			t.Fatalf("%s code = %v, want %s", name, refusal["code"], RefusalNotFound)
		}
		tryInstead, _ := refusal["try_instead"].(string)
		if !strings.Contains(tryInstead, "brain_") {
			t.Fatalf("%s try_instead = %q", name, tryInstead)
		}
	}
}

func TestSurfaceRecordActionsRoute(t *testing.T) {
	backend := newSurfaceFake()
	if got := toolCall(t, backend, "brain_record", `{"id":"decision-alpha-1"}`); got["isError"] != false {
		t.Fatalf("get = %v", got)
	}
	if got := toolCall(t, backend, "brain_record", `{"action":"list"}`); got["isError"] != false {
		t.Fatalf("list = %v", got)
	}
	if got := toolCall(t, backend, "brain_record", `{"action":"relations"}`); got["isError"] != false {
		t.Fatalf("relations = %v", got)
	}
	history := toolCall(t, backend, "brain_record", `{"action":"history","id":"decision-alpha-1"}`)
	if history["isError"] != false || len(backend.historyCalls) != 1 {
		t.Fatalf("history = %v, calls = %v", history, backend.historyCalls)
	}
	links := toolCall(t, backend, "brain_record", `{"action":"links","id":"decision-alpha-1"}`)
	if links["isError"] != false || !strings.Contains(textOf(t, links), "link:") {
		t.Fatalf("links = %v", links)
	}
	// The default action stays the historical get.
	if got := toolCall(t, backend, "brain_record", `{}`); got["isError"] != true {
		t.Fatal("brain_record without id accepted")
	}
}

func TestSurfaceCatchupModesRoute(t *testing.T) {
	backend := newSurfaceFake()
	changes := toolCall(t, backend, "brain_catchup", `{}`)
	if changes["isError"] != false || structured(t, changes)["project_id"] != "alpha" {
		t.Fatalf("changes = %v", changes)
	}
	journal := toolCall(t, backend, "brain_catchup", `{"mode":"journal","project":"alpha"}`)
	if journal["isError"] != false || len(structured(t, journal)["entries"].([]any)) != 1 {
		t.Fatalf("journal = %v", journal)
	}
	timeline := toolCall(t, backend, "brain_catchup", `{"mode":"timeline","record_id":"decision-alpha-1"}`)
	if timeline["isError"] != false || len(backend.timelineCalls) != 1 {
		t.Fatalf("timeline = %v, calls = %v", timeline, backend.timelineCalls)
	}
	if previews := structured(t, timeline)["previews"].([]any); len(previews) != 3 {
		t.Fatalf("previews = %v", previews)
	}
	if got := toolCall(t, backend, "brain_catchup", `{"mode":"timeline"}`); got["isError"] != true {
		t.Fatal("timeline without record_id accepted")
	}
	if got := toolCall(t, backend, "brain_catchup", `{"mode":"bogus"}`); got["isError"] != true {
		t.Fatal("unknown catchup mode accepted")
	}
}

func TestSurfaceReviewActionsRoute(t *testing.T) {
	backend := newSurfaceFake()
	findings := toolCall(t, backend, "brain_review", `{}`)
	if findings["isError"] != false || structured(t, findings)["action"] != "findings" {
		t.Fatalf("findings = %v", findings)
	}
	due := toolCall(t, backend, "brain_review", `{"action":"due","due_before":"2026-11-15"}`)
	if due["isError"] != false || len(backend.dueCalls) != 1 || len(structured(t, due)["due"].([]any)) != 1 {
		t.Fatalf("due = %v, calls = %v", due, backend.dueCalls)
	}
	doctor := toolCall(t, backend, "brain_review", `{"action":"doctor"}`)
	if doctor["isError"] != false || backend.doctorCalls != 1 || len(structured(t, doctor)["checks"].([]any)) != 1 {
		t.Fatalf("doctor = %v, calls = %d", doctor, backend.doctorCalls)
	}
	if got := toolCall(t, backend, "brain_review", `{"action":"doctor","project":"alpha"}`); got["isError"] != true {
		t.Fatal("doctor with a project accepted")
	}
	if got := toolCall(t, backend, "brain_review", `{"action":"due","due_before":"soon"}`); got["isError"] != true {
		t.Fatal("malformed due_before accepted")
	}
}

func TestSurfaceJudgeActionsRoute(t *testing.T) {
	backend := newSurfaceFake()
	if got := toolCall(t, backend, "brain_judge", `{"from":"a","to":"b","verdict":"relates_to","reason":"same topic"}`); got["isError"] != false {
		t.Fatalf("judge = %v", got)
	}
	if got := toolCall(t, backend, "brain_judge", `{"action":"list"}`); got["isError"] != false {
		t.Fatalf("list = %v", got)
	}
	many := toolCall(t, backend, "brain_judge",
		`{"action":"judge_many","judgments":[{"from":"a","to":"b","verdict":"relates_to","reason":"x"},{"from":"c","to":"d","verdict":"conflicts_with","reason":"y"}]}`)
	if many["isError"] != false || len(backend.judged) != 3 {
		t.Fatalf("judge_many = %v, judged = %v", many, backend.judged)
	}
	compare := toolCall(t, backend, "brain_judge", `{"action":"compare","from":"a","to":"b","reason":"same topic"}`)
	if compare["isError"] != false || backend.judged[len(backend.judged)-1].Verdict != "relates_to" {
		t.Fatalf("compare = %v", compare)
	}
	review := toolCall(t, backend, "brain_judge", `{"action":"review","ids":["*"]}`)
	if review["isError"] != false || len(backend.reviews) != 1 || !backend.reviews[0].All {
		t.Fatalf("review = %v, reviews = %v", review, backend.reviews)
	}
	accept := toolCall(t, backend, "brain_judge", `{"action":"accept","id":"decision-alpha-1"}`)
	if accept["isError"] != false || backend.statusChanges[0] != "decision-alpha-1=accepted" {
		t.Fatalf("accept = %v, status = %v", accept, backend.statusChanges)
	}
	reject := toolCall(t, backend, "brain_judge", `{"action":"reject","id":"decision-alpha-1"}`)
	if reject["isError"] != false || backend.statusChanges[1] != "decision-alpha-1=rejected" {
		t.Fatalf("reject = %v, status = %v", reject, backend.statusChanges)
	}
	supersede := toolCall(t, backend, "brain_judge", `{"action":"supersede","id":"decision-alpha-1","to":"decision-alpha-2"}`)
	if supersede["isError"] != false || len(backend.supersedes) != 1 {
		t.Fatalf("supersede = %v, supersedes = %v", supersede, backend.supersedes)
	}
}

func TestSurfaceProjectsActionsRoute(t *testing.T) {
	backend := newSurfaceFake()
	list := toolCall(t, backend, "brain_projects", `{}`)
	if list["isError"] != false || len(structured(t, list)["projects"].([]any)) != 1 {
		t.Fatalf("list = %v", list)
	}
	current := toolCall(t, backend, "brain_projects", `{"action":"current"}`)
	if current["isError"] != false {
		t.Fatalf("current = %v", current)
	}
	got := structured(t, current)["current"].(map[string]any)
	if got["id"] != "alpha" || got["source"] != "cwd" {
		t.Fatalf("current = %v", got)
	}
	byAlias := toolCall(t, backend, "brain_projects", `{"action":"current","project":"Alpha-alias"}`)
	if byAlias["isError"] != false || structured(t, byAlias)["current"].(map[string]any)["source"] != "alias" {
		t.Fatalf("current by alias = %v", byAlias)
	}
	register := toolCall(t, backend, "brain_projects", `{"action":"register","from":"gamma","path":"C:/work/gamma","alias":["g"]}`)
	if register["isError"] != false || len(backend.registerCalls) != 1 || backend.registerCalls[0].ID != "gamma" {
		t.Fatalf("register = %v, calls = %v", register, backend.registerCalls)
	}
}

func TestSurfacePromptActionsRoute(t *testing.T) {
	backend := newSurfaceFake()
	save := toolCall(t, backend, "brain_prompt", `{"action":"save","content":"remember this","session":"s1"}`)
	if save["isError"] != false || len(backend.promptSaves) != 1 || backend.promptSaves[0].Text != "remember this" {
		t.Fatalf("save = %v, saves = %v", save, backend.promptSaves)
	}
	search := toolCall(t, backend, "brain_prompt", `{"action":"search","query":"handoff"}`)
	if search["isError"] != false || len(structured(t, search)["hits"].([]any)) != 1 {
		t.Fatalf("search = %v", search)
	}
}

func TestSurfacePromptDisabledRefusesEveryAction(t *testing.T) {
	backend := newSurfaceFake()
	backend.promptsOn = false
	for _, args := range []string{
		`{"action":"save","content":"x"}`,
		`{"action":"search"}`,
		`{"action":"discard"}`,
	} {
		result := toolCall(t, backend, "brain_prompt", args)
		if result["isError"] != true {
			t.Fatalf("%s accepted with the prompt zone disabled", args)
		}
		refusal := structured(t, result)
		if refusal["code"] != RefusalUnsupported {
			t.Fatalf("%s code = %v", args, refusal["code"])
		}
		if tryInstead, _ := refusal["try_instead"].(string); !strings.Contains(tryInstead, "prompts.enabled") {
			t.Fatalf("%s try_instead = %q", args, tryInstead)
		}
	}
	if len(backend.promptSaves) != 0 {
		t.Fatalf("a refused prompt was stored: %v", backend.promptSaves)
	}
}

func TestSurfaceUnsupportedActionsAreTyped(t *testing.T) {
	backend := newSurfaceFake()
	cases := []struct {
		tool string
		args string
	}{
		{"brain_judge", `{"action":"consolidate"}`},
		{"brain_judge", `{"from":"a","to":"b","verdict":"relates_to","reason":"x","expected_revision":"1"}`},
		{"brain_prompt", `{"action":"discard"}`},
		{"brain_projects", `{"action":"merge","from":"alpha","to":"beta"}`},
		{"brain_record", `{"action":"list","due_review":true}`},
		{"brain_search", `{"query":"x","include_prompts":true}`},
	}
	for _, test := range cases {
		result := toolCall(t, backend, test.tool, test.args)
		if result["isError"] != true {
			t.Fatalf("%s %s accepted", test.tool, test.args)
		}
		refusal := structured(t, result)
		if refusal["code"] != RefusalUnsupported {
			t.Fatalf("%s %s code = %v", test.tool, test.args, refusal["code"])
		}
		if tryInstead, _ := refusal["try_instead"].(string); tryInstead == "" {
			t.Fatalf("%s %s has no try_instead", test.tool, test.args)
		}
	}
}

func TestSurfaceHistoryWithoutBackendIsTyped(t *testing.T) {
	result := toolCall(t, &fullFake{}, "brain_record", `{"action":"history","id":"decision-alpha-1"}`)
	refusal := structured(t, result)
	if refusal["code"] != RefusalUnsupported || !strings.Contains(refusal["try_instead"].(string), "--history") {
		t.Fatalf("refusal = %v", refusal)
	}
}

func TestSurfaceReadOnlyListsReadActionsAndRefusesWrites(t *testing.T) {
	backend := newSurfaceFake()
	names := policyTools(t, backend, Policy{ReadOnly: true})
	if len(names) != 10 {
		t.Fatalf("read-only tools = %d, want 10: %v", len(names), names)
	}
	for _, name := range []string{
		"brain_open", "brain_context", "brain_search", "brain_record",
		"brain_catchup", "brain_review", "brain_projects", "brain_code",
		"brain_graph", "brain_capabilities",
	} {
		if !names[name] {
			t.Fatalf("read-only server dropped %s", name)
		}
	}
	for _, hidden := range []string{"brain_note", "brain_decision", "brain_attempt", "brain_handoff", "brain_state", "brain_judge", "brain_prompt"} {
		if names[hidden] {
			t.Fatalf("read-only server lists %s", hidden)
		}
	}
	for _, call := range []struct{ name, args string }{
		{"brain_state", `{"action":"set","goal":"x"}`},
		{"brain_judge", `{"action":"accept","id":"decision-alpha-1"}`},
		{"brain_prompt", `{"action":"save","content":"x"}`},
		{"brain_code", `{"action":"index","project":"alpha"}`},
		{"brain_code", `{"action":"sync","project":"alpha"}`},
	} {
		result := policyCall(t, backend, Policy{ReadOnly: true}, call.name, call.args)
		if result["isError"] != true || structured(t, result)["code"] != RefusalUnsupported {
			t.Fatalf("%s was not refused: %v", call.name, result)
		}
	}
	if len(backend.statusChanges) != 0 || len(backend.promptSaves) != 0 {
		t.Fatalf("a hidden write reached the backend: %v %v", backend.statusChanges, backend.promptSaves)
	}
}

func TestSurfaceOpenIncludesSections(t *testing.T) {
	backend := newSurfaceFake()
	backend.state = State{Goal: "ship 0.17"}
	backend.records = []RecordSummary{
		{Kind: "handoff", ID: "handoff-alpha-1", ProjectID: "alpha", Title: "0.16 done"},
		{Kind: "decision", ID: "decision-alpha-1", ProjectID: "alpha", Title: "Use fsstore"},
	}
	result := toolCall(t, backend, "brain_open", `{"include":["state","handoff","recent","counts"]}`)
	if result["isError"] != false {
		t.Fatalf("brain_open include = %v", result)
	}
	obj := structured(t, result)
	if obj["state"].(map[string]any)["goal"] != "ship 0.17" {
		t.Fatalf("state = %v", obj["state"])
	}
	if obj["handoff"].(map[string]any)["id"] != "handoff-alpha-1" {
		t.Fatalf("handoff = %v", obj["handoff"])
	}
	if recent := obj["recent"].([]any); len(recent) != 2 {
		t.Fatalf("recent = %v", recent)
	}
	counts := obj["counts"].(map[string]any)
	if counts["records"] != float64(2) || counts["projects"] != float64(1) {
		t.Fatalf("counts = %v", counts)
	}
	if got := toolCall(t, backend, "brain_open", `{"include":["bogus"]}`); got["isError"] != true {
		t.Fatal("unknown include accepted")
	}
}

func TestSurfaceSearchAllProjectsIsRefusedUnderAnAllowlist(t *testing.T) {
	backend := newSurfaceFake()
	result := policyCall(t, backend, Policy{Projects: []string{"alpha"}}, "brain_search",
		`{"query":"cache","all_projects":true}`)
	if result["isError"] != true {
		t.Fatalf("all_projects under an allowlist accepted: %v", result)
	}
	if code := structured(t, result)["code"]; code != RefusalInvalidArguments {
		t.Fatalf("code = %v", code)
	}
}

func TestSurfaceReadOnlyRefusesRegistryWrite(t *testing.T) {
	backend := newSurfaceFake()
	result := policyCall(t, backend, Policy{ReadOnly: true}, "brain_projects",
		`{"action":"register","from":"gamma"}`)
	if result["isError"] != true || structured(t, result)["code"] != RefusalUnsupported {
		t.Fatalf("register under --read-only = %v", result)
	}
	if len(backend.registerCalls) != 0 {
		t.Fatalf("a read-only registry write reached the backend: %v", backend.registerCalls)
	}
}

func TestSurfaceCapabilitiesPayload(t *testing.T) {
	backend := newSurfaceFake()
	result := toolCall(t, backend, "brain_capabilities", `{}`)
	if result["isError"] != false {
		t.Fatalf("capabilities = %v", result)
	}
	payload := structured(t, result)
	if payload["profile"] != ProfileLV24 || payload["profile_version"] != ProfileVersion || payload["engine"] != "test-version" {
		t.Fatalf("identity = %v", payload)
	}
	tools := payload["tools"].(map[string]any)
	if tools["total"] != float64(17) || tools["read_only"] != float64(9) ||
		tools["append_only"] != float64(5) || tools["destructive"] != float64(3) {
		t.Fatalf("tools = %v", tools)
	}
	codes, _ := payload["refusal_codes"].([]any)
	if len(codes) != 7 || !slices.Contains(codes, any(RefusalUnsupported)) {
		t.Fatalf("refusal_codes = %v", codes)
	}
	gaps, _ := payload["gaps"].([]any)
	if len(gaps) == 0 {
		t.Fatal("capabilities has no gaps")
	}
	endpoints := payload["endpoints"].(map[string]any)
	if endpoints["stdio"] != true || endpoints["http"] != false || endpoints["resources"] != true {
		t.Fatalf("endpoints = %v", endpoints)
	}
	features := payload["features"].(map[string]any)
	if features["code_graph"] != true || features["knowledge_graph"] != "partial" ||
		features["semantic_search"] != false || features["resources"] != true {
		t.Fatalf("features = %v", features)
	}
	if _, ok := payload["tools_detail"]; ok {
		t.Fatalf("non-verbose capabilities carries tools_detail: %v", payload["tools_detail"])
	}

	verbose := toolCall(t, backend, "brain_capabilities", `{"verbose":true}`)
	details, _ := structured(t, verbose)["tools_detail"].([]any)
	if len(details) != 17 {
		t.Fatalf("tools_detail = %d, want 17", len(details))
	}

	// No side effects: nothing was written or read from the knowledge store.
	if len(backend.notes)+len(backend.decisions)+len(backend.attempts)+len(backend.handoffs) != 0 ||
		len(backend.statusChanges) != 0 || len(backend.promptSaves) != 0 || backend.doctorCalls != 0 {
		t.Fatal("brain_capabilities had a side effect")
	}
}

func TestSurfaceCapabilitiesMatchTheCatalogue(t *testing.T) {
	capabilities := CapabilityReport("test-version", DefaultProfile(), Policy{ReadOnly: true})
	if capabilities.Tools.Total != 10 || capabilities.Tools.ReadOnly != 9 {
		t.Fatalf("read-only capabilities = %+v", capabilities.Tools)
	}
	visible, omitted := CatalogForPolicy("test-version", DefaultProfile(), Policy{ReadOnly: true})
	if len(visible) != capabilities.Tools.Total || !slices.Equal(omitted, []string{
		"brain_note", "brain_decision", "brain_attempt", "brain_handoff", "brain_state", "brain_judge", "brain_prompt",
	}) {
		t.Fatalf("visible = %d, omitted = %v", len(visible), omitted)
	}
}
