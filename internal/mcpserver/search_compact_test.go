package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// expandingFake returns a decision with a long body, so an expansion is
// visible in both the structured and the text half of the result.
type expandingFake struct {
	fullFake
	body string
}

func (f *expandingFake) GetRecord(_ context.Context, projectID, id string) (RecordDetail, error) {
	return RecordDetail{
		Kind: "decision", ID: id, ProjectID: projectID, Title: "Use fsstore",
		Status: "accepted", Context: f.body,
		Links: []RecordLink{}, Backlinks: []RecordBacklink{},
	}, nil
}

// newExpandBackend returns a backend whose search returns the given record ids.
func newExpandBackend(body string, ids ...string) *expandingFake {
	backend := &expandingFake{body: body}
	for _, id := range ids {
		backend.hits = append(backend.hits, Hit{
			ID: id, ProjectID: "alpha", Kind: "decision", Title: "Use fsstore",
			Snippet: "decision preview", Score: 0.5, MatchReasons: []string{"bm25", "importance"},
		})
	}
	return backend
}

// expandedOf decodes the expanded array of a successful search result.
func expandedOf(t *testing.T, result map[string]any) []map[string]any {
	t.Helper()
	obj := structured(t, result)
	raw, ok := obj["expanded"].([]any)
	if !ok {
		t.Fatalf("expanded is %T, want array", obj["expanded"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(map[string]any))
	}
	return out
}

// A search returns previews and ids only; bodies never ride along by default.
func TestSearchCompactByDefault(t *testing.T) {
	backend := newExpandBackend("the whole decision body", "decision-1")
	result := toolCall(t, backend, "brain_search", `{"query":"fsstore"}`)
	if result["isError"] != false {
		t.Fatalf("search error: %v", result)
	}
	obj := structured(t, result)
	if obj["compact"] != true {
		t.Fatalf("compact = %v, want true", obj["compact"])
	}
	if expanded := expandedOf(t, result); len(expanded) != 0 {
		t.Fatalf("expanded = %v, want none by default", expanded)
	}
	if text, _ := result["content"].([]any); len(text) > 0 {
		if content, ok := text[0].(map[string]any); ok {
			if strings.Contains(fmt.Sprint(content["text"]), "the whole decision body") {
				t.Fatal("default search text contains a body")
			}
		}
	}
}

// expand_ids returns the requested bodies in the same call.
func TestSearchExpandIDsReturnsBody(t *testing.T) {
	backend := newExpandBackend("the whole decision body", "decision-1", "decision-2")
	result := toolCall(t, backend, "brain_search", `{"query":"fsstore","expand_ids":["decision-2"],"max_tokens":100}`)
	if result["isError"] != false {
		t.Fatalf("search error: %v", result)
	}
	obj := structured(t, result)
	if obj["compact"] != true {
		t.Fatalf("compact = %v, want true: expanding one id must not drop the compact default", obj["compact"])
	}
	expanded := expandedOf(t, result)
	if len(expanded) != 1 || expanded[0]["id"] != "decision-2" {
		t.Fatalf("expanded = %v, want only decision-2", expanded)
	}
	if !strings.Contains(fmt.Sprint(expanded[0]["context"]), "the whole decision body") {
		t.Fatalf("expanded context = %v, want the body", expanded[0]["context"])
	}
	if expanded[0]["truncated"] != false {
		t.Fatalf("truncated = %v, want false", expanded[0]["truncated"])
	}
}

// compact=false expands every hit on the page.
func TestSearchCompactFalseExpandsPage(t *testing.T) {
	backend := newExpandBackend("the whole decision body", "decision-1", "decision-2")
	result := toolCall(t, backend, "brain_search", `{"query":"fsstore","compact":false}`)
	if result["isError"] != false {
		t.Fatalf("search error: %v", result)
	}
	obj := structured(t, result)
	if obj["compact"] != false {
		t.Fatalf("compact = %v, want false", obj["compact"])
	}
	if expanded := expandedOf(t, result); len(expanded) != 2 {
		t.Fatalf("expanded = %v, want both hits", expanded)
	}
}

// expand=true is the short form of expanding the page.
func TestSearchExpandFlag(t *testing.T) {
	backend := newExpandBackend("the whole decision body", "decision-1")
	result := toolCall(t, backend, "brain_search", `{"query":"fsstore","expand":true}`)
	if result["isError"] != false {
		t.Fatalf("search error: %v", result)
	}
	if expanded := expandedOf(t, result); len(expanded) != 1 {
		t.Fatalf("expanded = %v, want one hit", expanded)
	}
}

// A record expanded under a small budget carries the marker and the flag.
func TestSearchExpandTruncatesToBudget(t *testing.T) {
	backend := newExpandBackend(strings.Repeat("cache keeps entries ", 40), "decision-1")
	result := toolCall(t, backend, "brain_search", `{"query":"fsstore","expand_ids":["decision-1"],"max_tokens":8}`)
	if result["isError"] != false {
		t.Fatalf("search error: %v", result)
	}
	expanded := expandedOf(t, result)
	if len(expanded) != 1 || expanded[0]["truncated"] != true {
		t.Fatalf("expanded = %v, want one truncated record", expanded)
	}
	if !strings.Contains(fmt.Sprint(expanded[0]["context"]), "truncated") {
		t.Fatalf("context = %v, want a truncation marker", expanded[0]["context"])
	}
}

// An id from another page is refused instead of returning a surprise record.
func TestSearchExpandIDsRejectsUnknownID(t *testing.T) {
	backend := newExpandBackend("body", "decision-1")
	result := toolCall(t, backend, "brain_search", `{"query":"fsstore","expand_ids":["decision-9"]}`)
	if result["isError"] != true {
		t.Fatalf("result = %v, want a refusal", result)
	}
	if !strings.Contains(fmt.Sprint(result["content"]), "not on this page") {
		t.Fatalf("refusal text = %v", result["content"])
	}
}

// Too many ids are refused before any record is read.
func TestSearchExpandIDsBounded(t *testing.T) {
	backend := newExpandBackend("body", "decision-1")
	ids := make([]string, 0, ExpandMaxRecords+1)
	for index := 0; index <= ExpandMaxRecords; index++ {
		ids = append(ids, fmt.Sprintf("%q", fmt.Sprintf("decision-%d", index)))
	}
	result := toolCall(t, backend, "brain_search", `{"query":"fsstore","expand_ids":[`+strings.Join(ids, ",")+`]}`)
	if result["isError"] != true {
		t.Fatalf("result = %v, want a refusal", result)
	}
	if !strings.Contains(fmt.Sprint(result["content"]), "limit is") {
		t.Fatalf("refusal text = %v", result["content"])
	}
}
