package mcpserver

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// listToolsWith returns the result of one tools/list for a backend.
func listToolsWith(t *testing.T, backend Backend) map[string]any {
	t.Helper()
	return resultOf(t, session(t, backend, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)[0])
}

func TestContractEveryToolIsAnnotated(t *testing.T) {
	result := listToolsWith(t, &fullFake{})
	tools := result["tools"].([]any)
	if len(tools) != 18 {
		t.Fatalf("tools = %d, want 18", len(tools))
	}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if title, _ := tool["title"].(string); title == "" {
			t.Fatalf("%s has no title", name)
		}
		annotations, ok := tool["annotations"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no annotations", name)
		}
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := annotations[hint].(bool); !ok {
				t.Fatalf("%s annotations.%s = %v, want a boolean", name, hint, annotations[hint])
			}
		}
		if annotations["readOnlyHint"] == true && annotations["destructiveHint"] == true {
			t.Fatalf("%s is both read-only and destructive", name)
		}
		if schema, ok := tool["outputSchema"].(map[string]any); !ok || schema["type"] != "object" {
			t.Fatalf("%s outputSchema = %v", name, tool["outputSchema"])
		}
	}
}

func TestContractReadOnlyHintsMatchTheTools(t *testing.T) {
	result := listToolsWith(t, &fullFake{})
	readOnly := map[string]bool{
		"brain_open": true, "brain_context": true, "brain_search": true, "brain_record": true,
		"brain_catchup": true, "brain_review": true, "brain_projects": true, "brain_capabilities": true,
		"brain_graph": true,
	}
	seen := 0
	for _, raw := range result["tools"].([]any) {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		got := tool["annotations"].(map[string]any)["readOnlyHint"]
		if got != readOnly[name] {
			t.Fatalf("%s readOnlyHint = %v, want %v", name, got, readOnly[name])
		}
		if got == true {
			seen++
		}
	}
	if seen != 9 {
		t.Fatalf("read-only tools = %d, want 9", seen)
	}
}

func TestContractCatalogueCoversEveryTool(t *testing.T) {
	specs := Catalog("test-version", DefaultProfile())
	if len(specs) != 18 {
		t.Fatalf("catalogue = %d tools, want 18", len(specs))
	}
	seen := map[string]bool{}
	for _, spec := range specs {
		if spec.Name == "" || spec.Title == "" || spec.Description == "" {
			t.Fatalf("incomplete spec: %+v", spec)
		}
		if seen[spec.Name] {
			t.Fatalf("duplicate tool %s", spec.Name)
		}
		seen[spec.Name] = true
		if spec.InputSchema["type"] != "object" || spec.OutputSchema["type"] != "object" {
			t.Fatalf("%s schemas = %v / %v", spec.Name, spec.InputSchema["type"], spec.OutputSchema["type"])
		}
	}
	for _, name := range []string{"brain_search", "brain_record", "brain_catchup", "brain_judge"} {
		spec := findSpec(t, specs, name)
		for _, key := range []string{"default_page_size", "max_page_size"} {
			if spec.Limits[key] <= 0 {
				t.Fatalf("%s limits.%s = %v", name, key, spec.Limits)
			}
		}
	}
}

func findSpec(t *testing.T, specs []ToolSpec, name string) ToolSpec {
	t.Helper()
	for _, spec := range specs {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("tool %s missing from the catalogue", name)
	return ToolSpec{}
}

func TestContractPaginationWalksThreePagesWithoutDuplicates(t *testing.T) {
	backend := &fullFake{}
	for i := range 7 {
		backend.records = append(backend.records, RecordSummary{
			Kind: "decision", ID: fmt.Sprintf("decision-%d", i), ProjectID: "alpha", Title: fmt.Sprintf("decision %d", i),
		})
	}
	var seen []string
	cursor := ""
	for page := 1; page <= 3; page++ {
		args := `{"action":"list","project":"alpha","limit":3`
		if cursor != "" {
			args += fmt.Sprintf(`,"cursor":%q`, cursor)
		}
		args += "}"
		result := toolCall(t, backend, "brain_record", args)
		if result["isError"] != false {
			t.Fatalf("page %d error: %v", page, result)
		}
		obj := structured(t, result)
		if got := obj["page"]; got != float64(page) {
			t.Fatalf("page = %v, want %d", got, page)
		}
		if got := obj["page_size"]; got != float64(3) {
			t.Fatalf("page_size = %v, want 3", got)
		}
		// The backend must be asked for the page plus one look-ahead row.
		if want := page*3 + 1; backend.recordsQuery.Limit != want {
			t.Fatalf("backend limit = %d, want %d", backend.recordsQuery.Limit, want)
		}
		records := obj["records"].([]any)
		for _, raw := range records {
			seen = append(seen, raw.(map[string]any)["id"].(string))
		}
		switch page {
		case 1, 2:
			if obj["has_more"] != true {
				t.Fatalf("page %d has_more = %v", page, obj["has_more"])
			}
			cursor, _ = obj["next_cursor"].(string)
			if cursor == "" {
				t.Fatalf("page %d has no next_cursor", page)
			}
		case 3:
			if obj["has_more"] != false {
				t.Fatalf("page 3 has_more = %v", obj["has_more"])
			}
			if _, ok := obj["next_cursor"]; ok {
				t.Fatalf("page 3 has a next_cursor: %v", obj["next_cursor"])
			}
		}
	}
	if len(seen) != 7 {
		t.Fatalf("records read = %d, want 7: %v", len(seen), seen)
	}
	unique := map[string]bool{}
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("duplicate record %s across pages", id)
		}
		unique[id] = true
	}
	for i := range 7 {
		if !unique[fmt.Sprintf("decision-%d", i)] {
			t.Fatalf("record decision-%d missing from the pages: %v", i, seen)
		}
	}
}

func TestContractPaginationRejectsForeignCursor(t *testing.T) {
	backend := &fullFake{}
	backend.records = []RecordSummary{
		{Kind: "decision", ID: "decision-1", ProjectID: "alpha", Title: "one"},
		{Kind: "decision", ID: "decision-2", ProjectID: "alpha", Title: "two"},
	}
	first := structured(t, toolCall(t, backend, "brain_record", `{"action":"list","project":"alpha","limit":1}`))
	cursor, _ := first["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("no cursor to reuse: %v", first)
	}
	// The same cursor for another project (a different query) must be refused.
	result := toolCall(t, backend, "brain_record", fmt.Sprintf(`{"action":"list","project":"beta","limit":1,"cursor":%q}`, cursor))
	if result["isError"] != true {
		t.Fatalf("foreign cursor accepted: %v", result)
	}
	if code := structured(t, result)["code"]; code != RefusalInvalidArguments {
		t.Fatalf("code = %v, want %s", code, RefusalInvalidArguments)
	}
	if text := textOf(t, result); !strings.Contains(text, "cursor") {
		t.Fatalf("text = %q", text)
	}
}

func TestContractSearchPageFooter(t *testing.T) {
	backend := &fullFake{hits: []Hit{
		{ID: "note-1", ProjectID: "alpha", Kind: "note", Title: "one", Snippet: "cache"},
		{ID: "note-2", ProjectID: "alpha", Kind: "note", Title: "two", Snippet: "cache"},
		{ID: "note-3", ProjectID: "alpha", Kind: "note", Title: "three", Snippet: "cache"},
	}}
	result := toolCall(t, backend, "brain_search", `{"query":"cache","limit":1}`)
	obj := structured(t, result)
	if obj["has_more"] != true {
		t.Fatalf("has_more = %v", obj["has_more"])
	}
	text := textOf(t, result)
	if !strings.Contains(text, "cursor=") {
		t.Fatalf("text has no cursor hint: %q", text)
	}
	if !strings.Contains(text, obj["next_cursor"].(string)) {
		t.Fatalf("text does not carry the cursor: %q", text)
	}
}

func TestContractRefusalShape(t *testing.T) {
	result := toolCall(t, &fullFake{}, "brain_note", `{"text":"x","bogus":1}`)
	if result["isError"] != true {
		t.Fatalf("unknown argument accepted: %v", result)
	}
	refusal := structured(t, result)
	if len(refusal) != 5 {
		t.Fatalf("refusal keys = %v, want exactly code/message/try_instead/current_revision/retryable", refusal)
	}
	if refusal["code"] != RefusalInvalidArguments {
		t.Fatalf("code = %v", refusal["code"])
	}
	for _, key := range []string{"message", "try_instead", "current_revision"} {
		if _, ok := refusal[key].(string); !ok {
			t.Fatalf("%s = %v, want a string", key, refusal[key])
		}
	}
	if refusal["retryable"] != false {
		t.Fatalf("retryable = %v", refusal["retryable"])
	}
	if text := textOf(t, result); !strings.Contains(text, RefusalInvalidArguments) {
		t.Fatalf("text = %q", text)
	}
}

func TestContractRefusalCodesFromTheBackend(t *testing.T) {
	missing := &fullFake{notFound: true}
	result := toolCall(t, missing, "brain_record", `{"id":"decision-404"}`)
	if code := structured(t, result)["code"]; code != RefusalNotFound {
		t.Fatalf("not-found code = %v", code)
	}
	conflict := &fullFake{conflicts: true}
	result = toolCall(t, conflict, "brain_judge", `{"action":"accept","id":"decision-1"}`)
	refusal := structured(t, result)
	if refusal["code"] != RefusalConflict {
		t.Fatalf("conflict code = %v", refusal["code"])
	}
	if refusal["retryable"] != true {
		t.Fatalf("conflict retryable = %v", refusal["retryable"])
	}
}

func TestContractSupersedeRequiresTarget(t *testing.T) {
	result := toolCall(t, &fullFake{}, "brain_judge", `{"action":"supersede","id":"decision-1"}`)
	refusal := structured(t, result)
	if refusal["code"] != RefusalInvalidArguments {
		t.Fatalf("code = %v, want %s", refusal["code"], RefusalInvalidArguments)
	}
	if refusal["try_instead"] == "" {
		t.Fatalf("no recovery hint: %v", refusal)
	}
}

func TestContractPageLimitsAreCapped(t *testing.T) {
	backend := &fullFake{}
	backend.records = []RecordSummary{{Kind: "decision", ID: "decision-1", ProjectID: "alpha", Title: "one"}}
	result := toolCall(t, backend, "brain_record", `{"action":"list","project":"alpha","limit":10000}`)
	obj := structured(t, result)
	if obj["page_size"] != float64(recordsMaxPageSize) {
		t.Fatalf("page_size = %v, want %d", obj["page_size"], recordsMaxPageSize)
	}
	if backend.recordsQuery.Limit != recordsMaxPageSize+1 {
		t.Fatalf("backend limit = %d", backend.recordsQuery.Limit)
	}
}

func TestContractJournalPagesPastTheWindowAreRefused(t *testing.T) {
	limits := pageLimits{def: 20, max: 500, window: 10}
	cursor := encodeCursor("brain_journal", 10, "")
	_, _, err := pageRequest("brain_journal", limits, 5, cursor, "")
	if err == nil {
		t.Fatal("cursor past the window accepted")
	}
	var refused *RefusalError
	if !errors.As(err, &refused) || refused.Code != RefusalUnsupported {
		t.Fatalf("err = %v, want an UNSUPPORTED refusal", err)
	}
	if refused.TryInstead == "" {
		t.Fatalf("no recovery hint: %+v", refused.Refusal)
	}
}
