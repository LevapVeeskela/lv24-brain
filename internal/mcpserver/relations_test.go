package mcpserver

import (
	"context"
	"fmt"
	"testing"
)

// relationFake is the full backend with a relation listing it can record and
// page over.
type relationFake struct {
	fullFake
	query RelationQuery
}

func (f *relationFake) ListRelations(_ context.Context, query RelationQuery) ([]RelationRow, error) {
	f.query = query
	out := f.relations
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

// threeRelations are one pending and two accepted rows, so ordering and paging
// are observable.
func threeRelations() []RelationRow {
	return []RelationRow{
		{SourceKind: "note", SourceID: "note-1", SourceTitle: "One", TargetID: "note-2",
			TargetState: "found", Relation: "relates_to", Status: "pending", AgeDays: 3},
		{SourceKind: "decision", SourceID: "decision-1", SourceTitle: "Two", TargetID: "decision-2",
			TargetState: "found", Relation: "supersedes", Status: "accepted", VerdictBy: "human"},
		{SourceKind: "attempt", SourceID: "attempt-1", SourceTitle: "Three", TargetID: "attempt-2",
			TargetState: "tombstone", Relation: "evidences", Status: "accepted", VerdictBy: "agent"},
	}
}

func TestRelationsListPagesWithTheCursorContract(t *testing.T) {
	backend := &relationFake{fullFake: fullFake{relations: threeRelations()}}
	first := toolCall(t, backend, "brain_relations", `{"project":"alpha","limit":2}`)
	if first["isError"] != false {
		t.Fatalf("first page = %v", first)
	}
	page := structured(t, first)
	if page["project_id"] != "alpha" {
		t.Fatalf("project_id = %v", page["project_id"])
	}
	if rows := page["relations"].([]any); len(rows) != 2 {
		t.Fatalf("page rows = %d, want 2", len(rows))
	}
	if page["has_more"] != true {
		t.Fatalf("has_more = %v", page["has_more"])
	}
	cursor, _ := page["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("next_cursor missing: %v", page)
	}
	second := structured(t, toolCall(t, backend, "brain_relations",
		fmt.Sprintf(`{"project":"alpha","limit":2,"cursor":%q}`, cursor)))
	if rows := second["relations"].([]any); len(rows) != 1 {
		t.Fatalf("second page rows = %d, want 1", len(rows))
	}
	if second["has_more"] != false {
		t.Fatalf("second has_more = %v", second["has_more"])
	}
	if _, ok := second["next_cursor"]; ok {
		t.Fatalf("second page has a next_cursor: %v", second["next_cursor"])
	}
}

func TestRelationsListForwardsTheFilters(t *testing.T) {
	backend := &relationFake{fullFake: fullFake{relations: threeRelations()}}
	toolCall(t, backend, "brain_relations",
		`{"project":"alpha","id":"note-1","status":"pending","relation":"relates_to","limit":5}`)
	query := backend.query
	if query.ProjectID != "alpha" || query.ID != "note-1" || query.Target != "" {
		t.Fatalf("query = %+v", query)
	}
	if len(query.Statuses) != 1 || query.Statuses[0] != "pending" {
		t.Fatalf("statuses = %+v", query.Statuses)
	}
	if len(query.Relations) != 1 || query.Relations[0] != "relates_to" {
		t.Fatalf("relations = %+v", query.Relations)
	}
	// The tool asks for the page plus one look-ahead row.
	if query.Limit != 6 {
		t.Fatalf("backend limit = %d, want 6", query.Limit)
	}
}

func TestRelationsListRefusals(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"both ends", `{"project":"alpha","id":"note-1","target":"note-2"}`},
		{"unknown status", `{"project":"alpha","status":"someday"}`},
		{"unknown relation", `{"project":"alpha","relation":"likes"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			backend := &relationFake{fullFake: fullFake{relations: threeRelations()}}
			result := toolCall(t, backend, "brain_relations", test.args)
			if result["isError"] != true {
				t.Fatalf("accepted: %v", result)
			}
			if code := structured(t, result)["code"]; code != RefusalInvalidArguments {
				t.Fatalf("code = %v, want %s", code, RefusalInvalidArguments)
			}
			if backend.query.ProjectID != "" {
				t.Fatalf("a refused listing reached the backend: %+v", backend.query)
			}
		})
	}
}

func TestJudgePassesTheAgentVerdict(t *testing.T) {
	backend := &relationFake{}
	result := toolCall(t, backend, "brain_judge",
		`{"project":"alpha","from":"decision-1","to":"decision-2","verdict":"conflicts_with","reason":"opposite storage choices","confidence":0.7}`)
	if result["isError"] != false {
		t.Fatalf("judge = %v", result)
	}
	if len(backend.judged) != 1 {
		t.Fatalf("judged = %+v", backend.judged)
	}
	got := backend.judged[0]
	if got.ProjectID != "alpha" || got.From != "decision-1" || got.To != "decision-2" {
		t.Fatalf("judged = %+v", got)
	}
	if got.Verdict != "conflicts_with" || got.Confidence != 0.7 || got.Reason == "" {
		t.Fatalf("judged = %+v", got)
	}
	if got.By != "agent" {
		t.Fatalf("judge = %q, want agent", got.By)
	}
	out := structured(t, result)
	if out["changed"] != true {
		t.Fatalf("changed = %v", out["changed"])
	}
	relation := out["relation"].(map[string]any)
	if relation["status"] != "accepted" || relation["verdict_by"] != "agent" {
		t.Fatalf("relation = %v", relation)
	}
}

func TestJudgeRefusals(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"unknown verdict", `{"from":"a","to":"b","verdict":"likes","reason":"x"}`},
		{"missing reason", `{"from":"a","to":"b","verdict":"relates_to"}`},
		{"bad confidence", `{"from":"a","to":"b","verdict":"relates_to","reason":"x","confidence":2}`},
		{"missing from", `{"to":"b","verdict":"relates_to","reason":"x"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			backend := &relationFake{}
			result := toolCall(t, backend, "brain_judge", test.args)
			if result["isError"] != true {
				t.Fatalf("accepted: %v", result)
			}
			if code := structured(t, result)["code"]; code != RefusalInvalidArguments {
				t.Fatalf("code = %v, want %s", code, RefusalInvalidArguments)
			}
			if len(backend.judged) != 0 {
				t.Fatalf("a refused verdict reached the backend: %+v", backend.judged)
			}
		})
	}
}

func TestReadOnlyPolicyDropsTheJudgeAndKeepsTheListing(t *testing.T) {
	names := policyTools(t, &relationFake{}, Policy{ReadOnly: true})
	if names["brain_judge"] {
		t.Fatal("read-only server lists brain_judge")
	}
	if !names["brain_relations"] {
		t.Fatal("read-only server dropped brain_relations")
	}
	backend := &relationFake{}
	result := policyCall(t, backend, Policy{ReadOnly: true}, "brain_judge",
		`{"from":"a","to":"b","verdict":"relates_to","reason":"x"}`)
	if result["isError"] != true {
		t.Fatalf("read-only judge accepted: %v", result)
	}
	if code := structured(t, result)["code"]; code != RefusalUnsupported {
		t.Fatalf("code = %v, want %s", code, RefusalUnsupported)
	}
	if len(backend.judged) != 0 {
		t.Fatalf("the refused verdict reached the backend: %+v", backend.judged)
	}
}

func TestJudgeHonoursTheWriteScope(t *testing.T) {
	backend := &relationFake{}
	policy := Policy{Projects: []string{"alpha", "beta"}, RestrictWrites: true, DefaultProject: "beta"}
	result := policyCall(t, backend, policy, "brain_judge",
		`{"from":"decision-1","to":"decision-2","verdict":"relates_to","reason":"x"}`)
	if result["isError"] != true {
		t.Fatalf("out-of-scope judge accepted: %v", result)
	}
	if code := structured(t, result)["code"]; code != RefusalUnsupported {
		t.Fatalf("code = %v, want %s", code, RefusalUnsupported)
	}
	if len(backend.judged) != 0 {
		t.Fatalf("the out-of-scope verdict reached the backend: %+v", backend.judged)
	}
	// The default project is inside the write scope.
	policyCall(t, backend, policy, "brain_judge",
		`{"project":"beta","from":"decision-1","to":"decision-2","verdict":"relates_to","reason":"x"}`)
	if len(backend.judged) != 1 || backend.judged[0].ProjectID != "beta" {
		t.Fatalf("judged = %+v", backend.judged)
	}
}

func TestJudgeRefusesAProjectOutsideTheAllowlist(t *testing.T) {
	backend := &relationFake{}
	policy := Policy{Projects: []string{"beta"}, RestrictWrites: true, AllowWrite: true}
	result := policyCall(t, backend, policy, "brain_judge",
		`{"project":"alpha","from":"decision-1","to":"decision-2","verdict":"relates_to","reason":"x"}`)
	if result["isError"] != true {
		t.Fatalf("out-of-allowlist judge accepted: %v", result)
	}
	if code := structured(t, result)["code"]; code != RefusalInvalidArguments {
		t.Fatalf("code = %v, want %s", code, RefusalInvalidArguments)
	}
	if len(backend.judged) != 0 {
		t.Fatalf("the out-of-allowlist verdict reached the backend: %+v", backend.judged)
	}
}
