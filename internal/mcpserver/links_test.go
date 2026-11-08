package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecordDetailLinksJSONAndText(t *testing.T) {
	// Arrange
	detail := RecordDetail{
		Kind: "note", ID: "n1", ProjectID: "alpha", Title: "Why",
		Links: []RecordLink{
			{Field: "body", Ref: "d1", Text: "the decision", ProjectID: "alpha", ID: "d1", State: "found", Kind: "decision", Title: "Use fsstore", Status: "accepted"},
			{Field: "body", Ref: "beta/x", ProjectID: "beta", ID: "x", State: "missing", Detail: "project beta not found"},
		},
		Backlinks: []RecordBacklink{{Field: "reason", Kind: "attempt", ID: "a1", ProjectID: "alpha", Title: "Try"}},
	}

	// Act
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	text := detailText(detail)
	empty, _ := json.Marshal(RecordDetail{Kind: "note", Links: []RecordLink{}, Backlinks: []RecordBacklink{}})

	// Assert
	for _, want := range []string{`"links":[{"field":"body","ref":"d1"`, `"state":"missing"`, `"backlinks":[{"field":"reason","kind":"attempt","id":"a1"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("json missing %s: %s", want, raw)
		}
	}
	for _, want := range []string{
		"link: d1 (the decision) -> decision [accepted] Use fsstore",
		"link: beta/x -> missing (project beta not found)",
		"backlink: attempt a1 Try (in reason)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(string(empty), `"links":[],"backlinks":[]`) {
		t.Fatalf("empty lists = %s", empty)
	}
}
