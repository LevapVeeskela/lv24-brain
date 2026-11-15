package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"
)

// quarantineReviewFake adds the quarantine review capability to fullFake.
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

func TestReviewActionIsAlwaysListed(t *testing.T) {
	plain := listToolsWith(t, &fullFake{})
	if !toolNameSet(plain)["brain_judge"] {
		t.Fatal("brain_judge is missing from the 15-tool surface")
	}
	if toolNameSet(plain)["brain_review_record"] {
		t.Fatal("retired brain_review_record is still listed")
	}
}

func toolNameSet(result map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, raw := range result["tools"].([]any) {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	return names
}

func TestReviewActionCallsTheBackend(t *testing.T) {
	backend := &quarantineReviewFake{}
	result := toolCall(t, backend, "brain_judge", `{"action":"review","id":"decision-alpha-1","trust":"stated"}`)
	if result["isError"] != false {
		t.Fatalf("brain_judge review error: %v", result)
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

	all := toolCall(t, backend, "brain_judge", `{"action":"review","ids":["*"]}`)
	if all["isError"] != false || backend.reviews[1].All != true {
		t.Fatalf("review all = %v, input %+v", all, backend.reviews)
	}
}

func TestReviewActionRejectsAmbiguousArguments(t *testing.T) {
	for name, args := range map[string]string{
		"neither": `{"action":"review"}`,
		"both":    `{"action":"review","id":"decision-alpha-1","ids":["decision-alpha-2"]}`,
		"trust":   `{"action":"review","ids":["*"],"trust":"guessed"}`,
		"wrong":   `{"action":"accept","id":"decision-alpha-1","trust":"stated"}`,
	} {
		result := toolCall(t, &quarantineReviewFake{}, "brain_judge", args)
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

func TestJudgeIsDestructiveAndAnnotated(t *testing.T) {
	result := listToolsWith(t, &quarantineReviewFake{})
	for _, raw := range result["tools"].([]any) {
		tool := raw.(map[string]any)
		if tool["name"] != "brain_judge" {
			continue
		}
		annotations := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != false {
			t.Fatalf("judge tool is read-only: %v", annotations)
		}
		if annotations["destructiveHint"] != true {
			t.Fatalf("judge tool is not marked destructive: %v", annotations)
		}
		if !strings.Contains(tool["description"].(string), "action=judge") {
			t.Fatalf("actions are not documented: %v", tool["description"])
		}
		return
	}
	t.Fatal("brain_judge missing")
}
