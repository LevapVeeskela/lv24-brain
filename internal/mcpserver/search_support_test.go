package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// reportingSearchFake is a backend that can report the support verdict and the
// withheld records, the way the local app backend will.
type reportingSearchFake struct {
	fullFake
	report SearchReport
}

func (f *reportingSearchFake) SearchReport(_ context.Context, in SearchInput) (SearchReport, error) {
	f.search = in
	return f.report, nil
}

// resultText is the text half of a tool result.
func resultText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v, want one text item", result["content"])
	}
	item, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] = %T, want object", content[0])
	}
	return fmt.Sprint(item["text"])
}

// RET-2: the search response carries the support block, the per-hit tier and
// the excluded records, in JSON and in text.
func TestSearchSupportMirrorsTierAndExcluded(t *testing.T) {
	backend := &reportingSearchFake{report: SearchReport{
		Hits: []Hit{{
			ID: "decision-1", ProjectID: "alpha", Kind: "decision",
			Title: "Release signing", Snippet: "signing keys", Score: 0.8, Tier: SupportSupported,
		}},
		Support: Support{Tier: SupportSupported, Best: 1, Threshold: 0.5},
		Excluded: []ExcludedHit{{
			ID: "note-1", ProjectID: "alpha", Kind: "note",
			Title: "Duplicate probe", Score: 0.4, Reason: "duplicate",
		}},
	}}

	result := toolCall(t, backend, "brain_search", `{"query":"release signing"}`)
	obj := structured(t, result)
	support, ok := obj["support"].(map[string]any)
	if !ok || support["tier"] != SupportSupported {
		t.Fatalf("support = %v, want tier %q", obj["support"], SupportSupported)
	}
	if support["threshold"] != 0.5 {
		t.Fatalf("support.threshold = %v, want 0.5", support["threshold"])
	}
	excluded, ok := obj["excluded"].([]any)
	if !ok || len(excluded) != 1 {
		t.Fatalf("excluded = %v, want one record", obj["excluded"])
	}
	if entry := excluded[0].(map[string]any); entry["reason"] != "duplicate" || entry["id"] != "note-1" {
		t.Fatalf("excluded[0] = %v, want note-1 as duplicate", entry)
	}
	hits, ok := obj["hits"].([]any)
	if !ok || len(hits) != 1 || hits[0].(map[string]any)["tier"] != SupportSupported {
		t.Fatalf("hits = %v, want one supported hit", obj["hits"])
	}
	text := resultText(t, result)
	if !strings.Contains(text, "[supported]") || !strings.Contains(text, "duplicate 1") {
		t.Fatalf("text = %q, want the tier and the duplicate count", text)
	}
}

// RET-2: none is explicit in text, not an empty "no matches".
func TestSearchSupportNoneCarriesReason(t *testing.T) {
	backend := &reportingSearchFake{report: SearchReport{
		Hits:    []Hit{},
		Support: Support{Tier: SupportNone, Best: 0.1, Threshold: 0.5, Reason: "no record contains every rare query term"},
		Excluded: []ExcludedHit{{
			ID: "note-2", ProjectID: "alpha", Kind: "note",
			Title: "Nearby", Score: 0.2, Reason: "lower-rank",
		}},
	}}

	result := toolCall(t, backend, "brain_search", `{"query":"zephyrion"}`)
	obj := structured(t, result)
	support := obj["support"].(map[string]any)
	if support["tier"] != SupportNone || support["reason"] == "" {
		t.Fatalf("support = %v, want none with a reason", support)
	}
	if hits := obj["hits"].([]any); len(hits) != 0 {
		t.Fatalf("hits = %v, want none", hits)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "no supported matches:") || !strings.Contains(text, "no record contains every rare query term") {
		t.Fatalf("text = %q, want the explicit reason", text)
	}
	if !strings.Contains(text, "lower-rank 1") {
		t.Fatalf("text = %q, want the excluded reason", text)
	}
}

// A backend that does not report support keeps the old response shape: the new
// keys are present and empty, never a guessed tier.
func TestSearchWithoutReporterKeepsAdditiveShape(t *testing.T) {
	backend := &fullFake{hits: []Hit{{
		ID: "note-1", ProjectID: "alpha", Kind: "note",
		Title: "Use fsstore", Snippet: "preview", Score: 0.5,
	}}}
	result := toolCall(t, backend, "brain_search", `{"query":"fsstore"}`)
	obj := structured(t, result)
	support, ok := obj["support"].(map[string]any)
	if !ok || support["tier"] != "" {
		t.Fatalf("support = %v, want an empty tier", obj["support"])
	}
	excluded, ok := obj["excluded"].([]any)
	if !ok || len(excluded) != 0 {
		t.Fatalf("excluded = %v, want an empty array", obj["excluded"])
	}
	hits := obj["hits"].([]any)
	if tier, present := hits[0].(map[string]any)["tier"]; present && tier != "" {
		t.Fatalf("hit tier = %v, want absent or empty", tier)
	}
}
