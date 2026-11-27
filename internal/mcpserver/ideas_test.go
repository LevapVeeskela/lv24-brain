package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// ideaFake is a Backend plus IdeaBackend that records calls. It embeds the
// plain fakeBackend so the unrelated tools keep their baseline behaviour.
type ideaFake struct {
	fakeBackend
	adds     []IdeaAddInput
	lists    []IdeaListQuery
	briefs   []IdeaBriefQuery
	updates  []IdeaUpdateInput
	trashed  []ideaTrashCall
	restored []ideaRestoreCall
	brief    IdeaBrief
	ideas    map[string]domain.Idea
}

type ideaRestoreCall struct {
	ProjectID string
	ID        string
}

type ideaTrashCall struct {
	ProjectID string
	ID        string
}

func newIdeaFake() *ideaFake {
	return &ideaFake{
		ideas: map[string]domain.Idea{},
		brief: IdeaBrief{
			Text:  "## Ideas — 2 open · 1 due today\n- [ ] today 18:00 — Ship it (alpha)",
			Ideas: []domain.Idea{{ID: "idea-alpha-2", ProjectID: "alpha", Title: "Ship it", Status: domain.IdeaOpen}},
			Open:  2, DueToday: 1,
		},
	}
}

func (f *ideaFake) AddIdea(_ context.Context, in IdeaAddInput) (IdeaWrite, error) {
	f.adds = append(f.adds, in)
	return IdeaWrite{Idea: domain.Idea{
		ID: "idea-alpha-1", ProjectID: "alpha", Title: in.Title, Body: in.Body,
		Status: domain.IdeaOpen, Priority: domain.IdeaPriorityNormal,
		DueAt: in.DueAt, DueDate: in.DueDate, Tags: in.Tags, Source: in.Source,
	}, ProjectSource: app.IdeaProjectExplicit}, nil
}

func (f *ideaFake) ListIdeas(_ context.Context, in IdeaListQuery) (IdeaList, error) {
	f.lists = append(f.lists, in)
	ideas := []domain.Idea{
		{ID: "idea-alpha-1", ProjectID: "alpha", Title: "First", Status: domain.IdeaOpen, Body: "hidden"},
		{ID: "idea-alpha-2", ProjectID: "alpha", Title: "Second", Status: domain.IdeaDoing, Body: "hidden too"},
	}
	return IdeaList{
		Ideas: ideas, Total: len(ideas),
		Problems: []IdeaProblem{{Path: "projects/alpha/ideas/bad.md", Kind: "idea", Reason: "conflict markers"}},
	}, nil
}

func (f *ideaFake) GetIdea(_ context.Context, id string) (domain.Idea, error) {
	switch id {
	case "idea-alpha-1":
		return domain.Idea{ID: id, ProjectID: "alpha", Title: "First", Body: "the body", Status: domain.IdeaOpen}, nil
	case "idea-beta-1":
		return domain.Idea{ID: id, ProjectID: "beta", Title: "Elsewhere", Status: domain.IdeaOpen}, nil
	default:
		return domain.Idea{}, fmt.Errorf("idea %q: %w", id, domain.ErrNotFound)
	}
}

func (f *ideaFake) UpdateIdea(_ context.Context, in IdeaUpdateInput) (IdeaWrite, error) {
	f.updates = append(f.updates, in)
	status := domain.IdeaOpen
	if in.Status != nil {
		status = domain.IdeaStatus(*in.Status)
	}
	return IdeaWrite{Idea: domain.Idea{
		ID: in.ID, ProjectID: "alpha", Title: "First", Status: status, CompletionNote: in.Note,
	}}, nil
}

func (f *ideaFake) TrashIdea(_ context.Context, projectID, id string) (domain.Idea, error) {
	f.trashed = append(f.trashed, ideaTrashCall{ProjectID: projectID, ID: id})
	return domain.Idea{ID: id, ProjectID: "alpha", Title: "First"}, nil
}

func (f *ideaFake) RestoreIdea(_ context.Context, projectID, id string) (domain.Idea, error) {
	f.restored = append(f.restored, ideaRestoreCall{ProjectID: projectID, ID: id})
	project := "alpha"
	if id == "idea-beta-1" {
		project = "beta"
	}
	if projectID != "" && projectID != project {
		return domain.Idea{}, fmt.Errorf("trashed idea %q: %w", id, domain.ErrNotFound)
	}
	return domain.Idea{ID: id, ProjectID: project, Title: "First"}, nil
}

func (f *ideaFake) IdeaBrief(_ context.Context, in IdeaBriefQuery) (IdeaBrief, error) {
	f.briefs = append(f.briefs, in)
	return f.brief, nil
}

// ListRecords and GetRecord let the record tool read ideas without writing
// them; they are the read-only half of the contract.
func (f *ideaFake) ListRecords(_ context.Context, query RecordQuery) ([]RecordSummary, error) {
	return []RecordSummary{{
		Kind: "idea", ID: "idea-alpha-1", ProjectID: "alpha", Title: "First", Status: "open",
		CreatedAt: time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC),
	}}, nil
}

func (f *ideaFake) GetRecord(_ context.Context, projectID, id string) (RecordDetail, error) {
	return RecordDetail{
		Kind: "idea", ID: id, ProjectID: projectID, Title: "First", Status: "open", Body: "the body",
	}, nil
}

func (f *ideaFake) SetDecisionStatus(context.Context, string, string, string) (RecordSummary, error) {
	return RecordSummary{}, fmt.Errorf("decisions are out of scope: %w", domain.ErrUnsupported)
}

func (f *ideaFake) Supersede(context.Context, string, string, string) (RecordSummary, error) {
	return RecordSummary{}, fmt.Errorf("decisions are out of scope: %w", domain.ErrUnsupported)
}

func TestBrainIdeaCatalogueAndSchema(t *testing.T) {
	spec := findSpec(t, Catalog("test-version", DefaultProfile()), "brain_idea")
	if spec.Title == "" || spec.Description == "" {
		t.Fatalf("brain_idea spec = %+v", spec)
	}
	if spec.Annotations.ReadOnlyHint {
		t.Fatal("brain_idea is marked read-only")
	}
	if spec.Annotations.DestructiveHint {
		t.Fatal("brain_idea is marked destructive: rm is reversible trash")
	}
	for _, want := range []string{"explicit intent", "never invent", "done"} {
		if !strings.Contains(strings.ToLower(spec.Description), strings.ToLower(want)) {
			t.Fatalf("description does not teach %q: %s", want, spec.Description)
		}
	}
	required, _ := spec.InputSchema["required"].([]string)
	if len(required) != 1 || required[0] != "action" {
		t.Fatalf("required = %v, want [action]", required)
	}
	props := spec.InputSchema["properties"].(map[string]any)
	for _, name := range []string{
		"action", "id", "title", "body", "project", "all", "status", "priority",
		"due_at", "due_date", "remind_before_minutes", "tags", "source", "note",
		"reason", "statuses", "due_before", "due_after", "query", "limit", "max",
		"scope", "clear",
	} {
		if _, ok := props[name]; !ok {
			t.Fatalf("input schema is missing %q", name)
		}
	}
	actions := props["action"].(map[string]any)["enum"].([]string)
	if len(actions) != 10 {
		t.Fatalf("actions = %v, want 10", actions)
	}
	for _, want := range []string{"add", "list", "get", "update", "done", "reopen", "drop", "rm", "restore", "brief"} {
		if !slices.Contains(actions, want) {
			t.Fatalf("actions are missing %q: %v", want, actions)
		}
	}
	capabilities := CapabilityReport("test-version", DefaultProfile(), Policy{})
	if capabilities.Tools.Total != 18 || capabilities.Features["ideas"] != true {
		t.Fatalf("capabilities = %+v", capabilities.Tools)
	}
	if capabilities.Limits["idea_page"] != ideaMaxListLimit {
		t.Fatalf("idea_page = %d", capabilities.Limits["idea_page"])
	}
}

func TestBrainIdeaAddPassesTheContractArguments(t *testing.T) {
	backend := newIdeaFake()
	result := toolCall(t, backend, "brain_idea", `{"action":"add","project":"alpha","title":"  Dark theme  ",`+
		`"body":"markdown","priority":"high","due_date":"2026-12-03","remind_before_minutes":120,`+
		`"tags":["ui","dark"],"source":{"type":"session","id":"ses_1","harness":"opencode"}}`)
	if result["isError"] != false {
		t.Fatalf("add = %v", result)
	}
	if len(backend.adds) != 1 {
		t.Fatalf("adds = %d", len(backend.adds))
	}
	add := backend.adds[0]
	if add.Title != "Dark theme" || add.Body != "markdown" || add.Priority != "high" ||
		add.DueDate != "2026-12-03" || add.RemindBeforeMinutes == nil || *add.RemindBeforeMinutes != 120 ||
		len(add.Tags) != 2 || add.Source == nil || add.Source.Harness != "opencode" {
		t.Fatalf("add input = %+v", add)
	}
	obj := structured(t, result)
	row := obj["idea"].(map[string]any)
	if row["id"] != "idea-alpha-1" || row["body"] != "markdown" || row["due_date"] != "2026-12-03" {
		t.Fatalf("row = %v", row)
	}
	if obj["project_source"] != "explicit" {
		t.Fatalf("project_source = %v", obj["project_source"])
	}
}

func TestBrainIdeaListFiltersAndTrims(t *testing.T) {
	backend := newIdeaFake()
	result := toolCall(t, backend, "brain_idea", `{"action":"list","project":"alpha","statuses":["open","doing"],`+
		`"tags":["ui"],"query":"first","due_before":"2026-12-01T00:00:00+03:00","limit":1}`)
	if result["isError"] != false {
		t.Fatalf("list = %v", result)
	}
	if len(backend.lists) != 1 {
		t.Fatalf("lists = %d", len(backend.lists))
	}
	list := backend.lists[0]
	if list.ProjectID != "alpha" || len(list.Statuses) != 2 || len(list.Tags) != 1 ||
		list.DueBefore == "" || list.Query != "first" {
		t.Fatalf("list query = %+v", list)
	}
	obj := structured(t, result)
	ideas := obj["ideas"].([]any)
	if len(ideas) != 1 || obj["total"] != float64(2) {
		t.Fatalf("ideas = %v, total = %v", ideas, obj["total"])
	}
	if _, ok := ideas[0].(map[string]any)["body"]; ok {
		t.Fatal("list row carries a body")
	}
	problems := obj["problems"].([]any)
	if len(problems) != 1 {
		t.Fatalf("problems = %v", problems)
	}
}

func TestBrainIdeaStatusAndTrashActions(t *testing.T) {
	backend := newIdeaFake()
	if got := toolCall(t, backend, "brain_idea", `{"action":"done","id":"idea-alpha-1","note":"shipped"}`); got["isError"] != false {
		t.Fatalf("done = %v", got)
	}
	if len(backend.updates) != 1 || backend.updates[0].Status == nil || *backend.updates[0].Status != "done" ||
		backend.updates[0].Note != "shipped" {
		t.Fatalf("done update = %+v", backend.updates)
	}
	if got := toolCall(t, backend, "brain_idea", `{"action":"drop","id":"idea-alpha-1","reason":"not now"}`); got["isError"] != false {
		t.Fatalf("drop = %v", got)
	}
	if len(backend.updates) != 2 || *backend.updates[1].Status != "dropped" || backend.updates[1].Note != "not now" {
		t.Fatalf("drop update = %+v", backend.updates)
	}
	rm := toolCall(t, backend, "brain_idea", `{"action":"rm","id":"idea-alpha-1"}`)
	if rm["isError"] != false {
		t.Fatalf("rm = %v", rm)
	}
	if len(backend.trashed) != 1 || backend.trashed[0].ProjectID != "alpha" || backend.trashed[0].ID != "idea-alpha-1" {
		t.Fatalf("trashed = %+v", backend.trashed)
	}
	if structured(t, rm)["trashed"] != true {
		t.Fatalf("trash result = %v", structured(t, rm))
	}
	if got := toolCall(t, backend, "brain_idea", `{"action":"restore","id":"idea-alpha-1"}`); got["isError"] != false {
		t.Fatalf("restore = %v", got)
	}
	if len(backend.restored) != 1 {
		t.Fatalf("restored = %v", backend.restored)
	}
}

func TestBrainIdeaUpdateClearsAndReopens(t *testing.T) {
	backend := newIdeaFake()
	result := toolCall(t, backend, "brain_idea", `{"action":"update","id":"idea-alpha-1","project":"beta",`+
		`"title":"Renamed","priority":"low","due_at":"2026-12-01T10:00:00+03:00","clear":["reminder"],`+
		`"tags":[],"status":"doing"}`)
	if result["isError"] != false {
		t.Fatalf("update = %v", result)
	}
	if len(backend.updates) != 1 {
		t.Fatalf("updates = %d", len(backend.updates))
	}
	in := backend.updates[0]
	if in.ID != "idea-alpha-1" || in.ProjectID != "beta" || in.Title == nil || *in.Title != "Renamed" ||
		in.Priority == nil || *in.Priority != "low" || in.DueAt == nil || in.Status == nil || *in.Status != "doing" ||
		!in.ClearReminder || in.ClearTags || in.ClearDue {
		t.Fatalf("update input = %+v", in)
	}
	if in.Tags == nil || len(*in.Tags) != 0 {
		t.Fatalf("explicit empty tags = %v", in.Tags)
	}
	reopen := toolCall(t, backend, "brain_idea", `{"action":"reopen","id":"idea-alpha-1"}`)
	if reopen["isError"] != false {
		t.Fatalf("reopen = %v", reopen)
	}
	last := backend.updates[len(backend.updates)-1]
	if last.Status == nil || *last.Status != "open" {
		t.Fatalf("reopen update = %+v", last)
	}
}

func TestBrainIdeaBriefAndGet(t *testing.T) {
	backend := newIdeaFake()
	brief := toolCall(t, backend, "brain_idea", `{"action":"brief","project":"alpha","max":2}`)
	if brief["isError"] != false {
		t.Fatalf("brief = %v", brief)
	}
	obj := structured(t, brief)
	if obj["text"] == "" || obj["open"] != float64(2) || obj["due_today"] != float64(1) ||
		len(obj["ideas"].([]any)) != 1 {
		t.Fatalf("brief = %v", obj)
	}
	get := toolCall(t, backend, "brain_idea", `{"action":"get","id":"idea-alpha-1"}`)
	if get["isError"] != false {
		t.Fatalf("get = %v", get)
	}
	row := structured(t, get)["idea"].(map[string]any)
	if row["body"] != "the body" {
		t.Fatalf("get row = %v", row)
	}
}

func TestBrainIdeaRefusals(t *testing.T) {
	backend := newIdeaFake()
	cases := []struct {
		name string
		args string
		code string
	}{
		{"missing title", `{"action":"add"}`, RefusalInvalidArguments},
		{"unknown action", `{"action":"archive"}`, RefusalInvalidArguments},
		{"bad due", `{"action":"add","title":"x","due_at":"tomorrow"}`, RefusalInvalidArguments},
		{"both dues", `{"action":"add","title":"x","due_at":"2026-12-01T10:00:00Z","due_date":"2026-12-01"}`, RefusalInvalidArguments},
		{"bad status", `{"action":"list","statuses":["urgent"]}`, RefusalInvalidArguments},
		{"bad limit", `{"action":"list","limit":201}`, RefusalInvalidArguments},
		{"bad scope", `{"action":"brief","scope":"everywhere"}`, RefusalInvalidArguments},
		{"bad max", `{"action":"brief","max":11}`, RefusalInvalidArguments},
		{"missing id", `{"action":"get"}`, RefusalInvalidArguments},
		{"unknown id", `{"action":"get","id":"idea-alpha-404"}`, RefusalNotFound},
		{"bad clear", `{"action":"update","id":"idea-alpha-1","clear":["everything"]}`, RefusalInvalidArguments},
	}
	for _, test := range cases {
		result := toolCall(t, backend, "brain_idea", test.args)
		if result["isError"] != true {
			t.Fatalf("%s accepted: %v", test.name, result)
		}
		refusal := structured(t, result)
		if refusal["code"] != test.code {
			t.Fatalf("%s code = %v, want %s (%v)", test.name, refusal["code"], test.code, refusal)
		}
	}
	// An unknown id points at the idea tool, not at brain_records.
	if got := structured(t, toolCall(t, backend, "brain_idea", `{"action":"get","id":"idea-alpha-404"}`)); got["code"] == RefusalNotFound {
		if try, _ := got["try_instead"].(string); !strings.Contains(strings.ToLower(try), "brain_idea") {
			t.Fatalf("not-found try_instead = %q", try)
		}
	}
}

func TestBrainIdeaReadOnlyPolicy(t *testing.T) {
	backend := newIdeaFake()
	names := policyTools(t, backend, Policy{ReadOnly: true})
	if !names["brain_idea"] {
		t.Fatal("a read-only server dropped brain_idea")
	}
	for _, action := range []string{
		`{"action":"add","title":"x"}`,
		`{"action":"update","id":"idea-alpha-1","title":"y"}`,
		`{"action":"done","id":"idea-alpha-1"}`,
		`{"action":"rm","id":"idea-alpha-1"}`,
	} {
		result := policyCall(t, backend, Policy{ReadOnly: true}, "brain_idea", action)
		if result["isError"] != true || structured(t, result)["code"] != RefusalUnsupported {
			t.Fatalf("%s was not refused: %v", action, result)
		}
	}
	if len(backend.adds)+len(backend.updates)+len(backend.trashed) != 0 {
		t.Fatal("a read-only refusal reached the backend")
	}
	if result := policyCall(t, backend, Policy{ReadOnly: true}, "brain_idea", `{"action":"list","project":"alpha"}`); result["isError"] != false {
		t.Fatalf("read-only list = %v", result)
	}
}

func TestBrainIdeaWriteScope(t *testing.T) {
	backend := newIdeaFake()
	policy := Policy{RestrictWrites: true, DefaultProject: "alpha"}
	if got := policyCall(t, backend, policy, "brain_idea", `{"action":"add","title":"own project"}`); got["isError"] != false {
		t.Fatalf("add in the default project = %v", got)
	}
	refused := policyCall(t, backend, policy, "brain_idea", `{"action":"add","project":"beta","title":"elsewhere"}`)
	if refused["isError"] != true || structured(t, refused)["code"] != RefusalUnsupported {
		t.Fatalf("add outside the default project = %v", refused)
	}
	if len(backend.adds) != 1 {
		t.Fatalf("refused add reached the backend: %+v", backend.adds)
	}
	// An idea stored in another project is refused before every mutation, by
	// its global id.
	for _, action := range []string{"done", "reopen", "drop", "rm"} {
		args := fmt.Sprintf(`{"action":%q,"id":"idea-beta-1"}`, action)
		refused := policyCall(t, backend, policy, "brain_idea", args)
		if refused["isError"] != true || structured(t, refused)["code"] != RefusalUnsupported {
			t.Fatalf("%s outside the default project = %v", action, refused)
		}
	}
	if len(backend.updates) != 0 || len(backend.trashed) != 0 {
		t.Fatalf("a refused id write reached the backend: updates=%+v trashed=%v", backend.updates, backend.trashed)
	}
	move := policyCall(t, backend, policy, "brain_idea", `{"action":"update","id":"idea-alpha-1","project":"beta","title":"moved"}`)
	if move["isError"] != true || structured(t, move)["code"] != RefusalUnsupported {
		t.Fatalf("move outside the default project = %v", move)
	}
	if len(backend.updates) != 0 {
		t.Fatalf("refused move reached the backend: %+v", backend.updates)
	}
	// Restore is narrowed to the default project, never a global write.
	restore := policyCall(t, backend, policy, "brain_idea", `{"action":"restore","id":"idea-alpha-1"}`)
	if restore["isError"] != false {
		t.Fatalf("restore in the default project = %v", restore)
	}
	if len(backend.restored) != 1 || backend.restored[0].ProjectID != "alpha" || backend.restored[0].ID != "idea-alpha-1" {
		t.Fatalf("restored = %+v, want project alpha", backend.restored)
	}
	// A trashed idea of another project is not readable through the narrowed
	// restore: it is NOT_FOUND, not a write outside the scope.
	other := policyCall(t, backend, policy, "brain_idea", `{"action":"restore","id":"idea-beta-1"}`)
	if other["isError"] != true || structured(t, other)["code"] != RefusalNotFound {
		t.Fatalf("restore outside the default project = %v", other)
	}
	if len(backend.restored) != 2 || backend.restored[1].ProjectID != "alpha" {
		t.Fatalf("restore calls = %+v", backend.restored)
	}
}

func TestBrainIdeaWritesRespectTheAllowlist(t *testing.T) {
	// A beta checkout with an alpha-only allowlist: the default project is
	// outside the allowlist, so no implicit write may reach it.
	restricted := Policy{Projects: []string{"alpha"}, DefaultProject: "beta", RestrictWrites: true}

	t.Run("restricted", func(t *testing.T) {
		backend := newIdeaFake()
		if got := policyCall(t, backend, restricted, "brain_idea", `{"action":"add","title":"omitted"}`); got["isError"] != true {
			t.Fatalf("omitted add from a beta checkout = %v", got)
		}
		if got := policyCall(t, backend, restricted, "brain_idea", `{"action":"add","project":"alpha","title":"alpha"}`); got["isError"] != true {
			t.Fatalf("add to alpha without a writable default = %v", got)
		}
		if len(backend.adds) != 0 {
			t.Fatalf("refused adds reached the backend: %+v", backend.adds)
		}
		if got := policyCall(t, backend, restricted, "brain_idea", `{"action":"done","id":"idea-beta-1"}`); got["isError"] != true {
			t.Fatalf("done on a beta id = %v", got)
		}
		if got := policyCall(t, backend, restricted, "brain_idea", `{"action":"restore","id":"idea-beta-1"}`); got["isError"] != true {
			t.Fatalf("restore without a writable project = %v", got)
		}
		if len(backend.restored) != 0 {
			t.Fatalf("refused restore reached the backend: %+v", backend.restored)
		}
	})

	t.Run("allow-write", func(t *testing.T) {
		policy := restricted
		policy.AllowWrite = true
		backend := newIdeaFake()

		// The default project is still outside the allowlist: an omitted add
		// must not silently write it even though AllowWrite is set.
		if got := policyCall(t, backend, policy, "brain_idea", `{"action":"add","title":"omitted"}`); got["isError"] != true {
			t.Fatalf("omitted add from a beta checkout with AllowWrite = %v", got)
		}
		if len(backend.adds) != 0 {
			t.Fatalf("refused omitted add reached the backend: %+v", backend.adds)
		}
		// An explicit allowed project is written, and the backend receives the
		// resolved id, not the raw selector.
		if got := policyCall(t, backend, policy, "brain_idea", `{"action":"add","project":"alpha","title":"Alpha add"}`); got["isError"] != false {
			t.Fatalf("add to the allowed project = %v", got)
		}
		if len(backend.adds) != 1 || backend.adds[0].ProjectID != "alpha" {
			t.Fatalf("add target = %+v", backend.adds)
		}
		if got := policyCall(t, backend, policy, "brain_idea", `{"action":"add","project":"beta","title":"Beta add"}`); got["isError"] != true {
			t.Fatalf("add outside the allowlist = %v", got)
		}
		if len(backend.adds) != 1 {
			t.Fatalf("refused beta add reached the backend: %+v", backend.adds)
		}
		// Every id-addressed mutation checks the stored project.
		for _, action := range []string{"done", "reopen", "drop", "rm"} {
			if got := policyCall(t, backend, policy, "brain_idea", fmt.Sprintf(`{"action":%q,"id":"idea-beta-1"}`, action)); got["isError"] != true {
				t.Fatalf("%s on a beta id = %v", action, got)
			}
		}
		if len(backend.updates) != 0 || len(backend.trashed) != 0 {
			t.Fatalf("a refused beta mutation reached the backend: updates=%+v trashed=%+v", backend.updates, backend.trashed)
		}
		if got := policyCall(t, backend, policy, "brain_idea", `{"action":"done","id":"idea-alpha-1"}`); got["isError"] != false {
			t.Fatalf("done on an allowed id = %v", got)
		}
		if len(backend.updates) != 1 {
			t.Fatalf("allowed done did not reach the backend: %+v", backend.updates)
		}
		if got := policyCall(t, backend, policy, "brain_idea", `{"action":"update","id":"idea-alpha-1","project":"beta","title":"moved"}`); got["isError"] != true {
			t.Fatalf("move outside the allowlist = %v", got)
		}
		// Restore never performs a global lookup under an allowlist: the single
		// allowed project is used, so a beta id is NOT_FOUND.
		if got := policyCall(t, backend, policy, "brain_idea", `{"action":"restore","id":"idea-beta-1"}`); got["isError"] != true || structured(t, got)["code"] != RefusalNotFound {
			t.Fatalf("restore of a beta id without project = %v", got)
		}
		if len(backend.restored) != 1 || backend.restored[0].ProjectID != "alpha" {
			t.Fatalf("restore calls = %+v", backend.restored)
		}
		if got := policyCall(t, backend, policy, "brain_idea", `{"action":"restore","id":"idea-beta-1","project":"beta"}`); got["isError"] != true {
			t.Fatalf("restore restricted to the allowlist = %v", got)
		}
		if got := policyCall(t, backend, policy, "brain_idea", `{"action":"restore","id":"idea-alpha-1","project":"alpha"}`); got["isError"] != false {
			t.Fatalf("restore inside the allowlist = %v", got)
		}
	})

	// Several allowed projects: a restore without a project is ambiguous and
	// must be refused instead of silently going global.
	multiple := Policy{Projects: []string{"alpha", "beta"}, DefaultProject: "beta", RestrictWrites: true, AllowWrite: true}
	backend := newIdeaFake()
	if got := policyCall(t, backend, multiple, "brain_idea", `{"action":"restore","id":"idea-beta-1"}`); got["isError"] != true {
		t.Fatalf("ambiguous restore under several allowed projects = %v", got)
	}
	if got := policyCall(t, backend, multiple, "brain_idea", `{"action":"restore","id":"idea-beta-1","project":"beta"}`); got["isError"] != false {
		t.Fatalf("named restore inside the allowlist = %v", got)
	}
}

func TestBrainIdeaReadScope(t *testing.T) {
	backend := newIdeaFake()
	policy := Policy{Projects: []string{"alpha"}}
	// A named project outside the allowlist is refused before the read.
	outside := policyCall(t, backend, policy, "brain_idea", `{"action":"list","project":"beta"}`)
	if outside["isError"] != true || structured(t, outside)["code"] != RefusalInvalidArguments {
		t.Fatalf("list outside the allowlist = %v", outside)
	}
	if len(backend.lists) != 0 {
		t.Fatalf("refused list reached the backend: %+v", backend.lists)
	}
	// all=true is a global read and cannot be served under an allowlist.
	all := policyCall(t, backend, policy, "brain_idea", `{"action":"list","all":true}`)
	if all["isError"] != true {
		t.Fatalf("all=true under an allowlist = %v", all)
	}
	// The allowed project, and an omitted selector (narrowed to the single
	// allowed project), work.
	for _, args := range []string{`{"action":"list","project":"alpha"}`, `{"action":"list"}`} {
		if got := policyCall(t, backend, policy, "brain_idea", args); got["isError"] != false {
			t.Fatalf("%s = %v", args, got)
		}
	}
	// The global brief is refused; a current-scope brief is allowed and bound
	// to the allowed project.
	global := policyCall(t, backend, policy, "brain_idea", `{"action":"brief"}`)
	if global["isError"] != true {
		t.Fatalf("global brief under an allowlist = %v", global)
	}
	current := policyCall(t, backend, policy, "brain_idea", `{"action":"brief","scope":"current"}`)
	if current["isError"] != false {
		t.Fatalf("current brief under an allowlist = %v", current)
	}
	if len(backend.briefs) != 1 || backend.briefs[0].ProjectID != "alpha" || backend.briefs[0].Scope != app.IdeaBriefScopeCurrent {
		t.Fatalf("brief query = %+v", backend.briefs)
	}
	if brief := policyCall(t, backend, policy, "brain_idea", `{"action":"brief","project":"beta","scope":"current"}`); brief["isError"] != true {
		t.Fatalf("brief outside the allowlist = %v", brief)
	}
	// get is by global id, so its stored project is checked.
	if got := policyCall(t, backend, policy, "brain_idea", `{"action":"get","id":"idea-beta-1"}`); got["isError"] != true {
		t.Fatalf("get outside the allowlist = %v", got)
	}
	if got := policyCall(t, backend, policy, "brain_idea", `{"action":"get","id":"idea-alpha-1"}`); got["isError"] != false {
		t.Fatalf("get inside the allowlist = %v", got)
	}
}

func TestBrainRecordReadsIdeasWithoutWriting(t *testing.T) {
	backend := newIdeaFake()
	list := toolCall(t, backend, "brain_record", `{"action":"list","project":"alpha","kinds":["idea"]}`)
	if list["isError"] != false {
		t.Fatalf("record list = %v", list)
	}
	records := structured(t, list)["records"].([]any)
	if len(records) != 1 || records[0].(map[string]any)["kind"] != "idea" {
		t.Fatalf("records = %v", records)
	}
	get := toolCall(t, backend, "brain_record", `{"action":"get","project":"alpha","id":"idea-alpha-1"}`)
	if get["isError"] != false {
		t.Fatalf("record get = %v", get)
	}
	if structured(t, get)["body"] != "the body" {
		t.Fatalf("record get = %v", structured(t, get))
	}
	// brain_record has no mutation action: an idea write belongs to brain_idea.
	mutate := toolCall(t, backend, "brain_record", `{"action":"update","project":"alpha","id":"idea-alpha-1"}`)
	if mutate["isError"] != true || structured(t, mutate)["code"] != RefusalInvalidArguments {
		t.Fatalf("record mutation = %v", mutate)
	}
}
