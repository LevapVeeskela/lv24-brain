package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"
)

// quarantineReviewFake adds the quarantine review capability to fullFake, so tools/list
// reports the temporary brain_review_record tool.
type quarantineReviewFake struct {
	fullFake
	reviews []ReviewRecordInput
}

func (f *quarantineReviewFake) ReviewRecord(_ context.Context, in ReviewRecordInput) (ReviewRecordResult, error) {
	f.reviews = append(f.reviews, in)
	id := in.ID
	if id == "" {
		id = "decision-alpha-1"
	}
	return ReviewRecordResult{
		ProjectID: in.ProjectID,
		Reviewed:  []RecordSummary{{Kind: "decision", ID: id, ProjectID: in.ProjectID, Title: "Use fsstore"}},
	}, nil
}

func TestReviewRecordToolIsListedOnlyForReviewBackends(t *testing.T) {
	plain := listToolsWith(t, &fullFake{})
	if toolNameSet(plain)["brain_review_record"] {
		t.Fatal("brain_review_record is listed for a backend that cannot review records")
	}
	review := listToolsWith(t, &quarantineReviewFake{})
	if !toolNameSet(review)["brain_review_record"] {
		t.Fatal("brain_review_record is missing for a review backend")
	}
}

func toolNameSet(result map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, raw := range result["tools"].([]any) {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	return names
}

func TestReviewRecordToolCallsTheBackend(t *testing.T) {
	backend := &quarantineReviewFake{}
	result := toolCall(t, backend, "brain_review_record", `{"id":"decision-alpha-1","trust":"stated"}`)
	if result["isError"] != false {
		t.Fatalf("brain_review_record error: %v", result)
	}
	if len(backend.reviews) != 1 {
		t.Fatalf("reviews = %+v, want 1", backend.reviews)
	}
	got := backend.reviews[0]
	if got.ProjectID != "alpha" || got.ID != "decision-alpha-1" || got.All || got.Trust != "stated" {
		t.Fatalf("review input = %+v", got)
	}
	if text := textOf(t, result); !strings.Contains(text, "reviewed") {
		t.Fatalf("text = %q", text)
	}
	if obj := structured(t, result); obj["project_id"] != "alpha" {
		t.Fatalf("structured = %v", obj)
	}

	all := toolCall(t, backend, "brain_review_record", `{"all":true}`)
	if all["isError"] != false || backend.reviews[1].All != true {
		t.Fatalf("review all = %v, input %+v", all, backend.reviews)
	}
}

func TestReviewRecordToolRejectsAmbiguousArguments(t *testing.T) {
	for name, args := range map[string]string{
		"neither": `{}`,
		"both":    `{"id":"decision-alpha-1","all":true}`,
		"trust":   `{"all":true,"trust":"guessed"}`,
	} {
		result := toolCall(t, &quarantineReviewFake{}, "brain_review_record", args)
		if result["isError"] != true {
			t.Fatalf("%s: accepted %s", name, args)
		}
		if code := structured(t, result)["code"]; code != RefusalInvalidArguments {
			t.Fatalf("%s: code = %v, want %s", name, code, RefusalInvalidArguments)
		}
	}
}

func TestContextToolPassesIncludeUnreviewed(t *testing.T) {
	backend := &quarantineReviewFake{}
	result := toolCall(t, backend, "brain_context", `{"query":"cache","include_unreviewed":true}`)
	if result["isError"] != false {
		t.Fatalf("brain_context error: %v", result)
	}
	if !backend.contextIn.IncludeUnreviewed {
		t.Fatalf("include_unreviewed was not passed: %+v", backend.contextIn)
	}
}

func TestCatchupTextReportsQuarantinedIDs(t *testing.T) {
	since := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	empty := catchupText(Catchup{
		ProjectID: "alpha", Since: since, SinceSource: "handoff",
		Quarantined: []string{"note-alpha-1", "decision-alpha-2"},
	})
	if !strings.Contains(empty, "quarantined: 2 (note-alpha-1, decision-alpha-2)") {
		t.Fatalf("empty catchup text = %q", empty)
	}
	withItems := catchupText(Catchup{
		ProjectID: "alpha", Since: since, SinceSource: "handoff",
		Quarantined: []string{"note-alpha-1"},
		Notes:       []CatchupItem{{Kind: "note", ID: "note-alpha-0", Title: "trusted note", CreatedAt: since.Add(time.Minute)}},
	})
	if !strings.Contains(withItems, "quarantined: 1 (note-alpha-1)") || !strings.Contains(withItems, "trusted note") {
		t.Fatalf("catchup text = %q", withItems)
	}
}

func TestReviewRecordToolIsMutatingAndAnnotated(t *testing.T) {
	result := listToolsWith(t, &quarantineReviewFake{})
	for _, raw := range result["tools"].([]any) {
		tool := raw.(map[string]any)
		if tool["name"] != "brain_review_record" {
			continue
		}
		annotations := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != false {
			t.Fatalf("review tool is read-only: %v", annotations)
		}
		if annotations["destructiveHint"] != true {
			t.Fatalf("review tool is not marked mutating: %v", annotations)
		}
		if !strings.Contains(tool["description"].(string), "brain_judge") {
			t.Fatalf("temporary status is not documented: %v", tool["description"])
		}
		return
	}
	t.Fatal("brain_review_record missing")
}
