package mcpserver

import (
	"context"
	"strings"
	"testing"
)

type reviewFake struct {
	fakeBackend
	gotProject string
	review     Review
}

func (f *reviewFake) Review(_ context.Context, projectID string) (Review, error) {
	f.gotProject = projectID
	review := f.review
	review.ProjectID = projectID
	return review, nil
}

func TestMCPServerReviewTool(t *testing.T) {
	backend := &reviewFake{review: Review{
		Summary: "# Review — Alpha\nnext_action: old step",
		Findings: []ReviewFinding{{
			Kind:     "next-behind-handoff",
			Severity: "warn",
			Message:  "next action is behind the latest handoff",
			Why:      "a later handoff names a different next action",
			Next:     "run brain review --fix",
			Confirm:  "the handoff's next action is the work that continues",
			Evidence: "handoff-1",
			Fixable:  true,
			Fix:      "set next action to the handoff's next step",
		}},
	}}

	got := callTool(t, backend, "brain_review", `{"project":"alpha"}`)
	if got["isError"] == true {
		t.Fatalf("brain_review error: %+v", got)
	}
	if backend.gotProject != "alpha" {
		t.Fatalf("review called with project %q", backend.gotProject)
	}
	text := got["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{
		"# Review", "## Findings",
		"[warn] next-behind-handoff: a later handoff names a different next action",
		"-> run brain review --fix",
		"evidence: handoff-1", "confirm: the handoff's next action",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q:\n%s", want, text)
		}
	}
	structured := got["structuredContent"].(map[string]any)
	if structured["project_id"] != "alpha" || len(structured["findings"].([]any)) != 1 {
		t.Fatalf("structured = %+v", structured)
	}
	finding := structured["findings"].([]any)[0].(map[string]any)
	if finding["severity"] != "warn" || finding["why"] == "" || finding["next"] == "" || finding["confirm"] == "" {
		t.Fatalf("finding = %+v, want severity/why/next/confirm", finding)
	}
}

func TestMCPServerReviewToolNoFindingsIsEmptyList(t *testing.T) {
	backend := &reviewFake{review: Review{Summary: "# Review — Alpha"}}

	got := callTool(t, backend, "brain_review", `{}`)
	text := got["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasSuffix(text, "## Findings\n- none") {
		t.Fatalf("text = %q", text)
	}
	if findings := got["structuredContent"].(map[string]any)["findings"]; findings == nil {
		t.Fatal("findings must be an empty list, not null")
	}
}

func TestMCPServerReviewToolNeverFixes(t *testing.T) {
	backend := &reviewFake{}

	if got := callTool(t, backend, "brain_review", `{"fix":true}`); got["isError"] != true {
		t.Fatalf("fix argument accepted: %+v", got)
	}
}

func TestMCPServerReviewToolOnlyWithReviewBackend(t *testing.T) {
	names := func(backend Backend) string {
		var list []string
		for _, tool := range New(backend, "test").toolList() {
			list = append(list, tool.Name)
		}
		return strings.Join(list, ",")
	}
	if strings.Contains(names(&fakeBackend{}), "brain_review") {
		t.Fatal("review tool listed for a backend without review support")
	}
	if !strings.Contains(names(&reviewFake{}), "brain_review") {
		t.Fatal("review tool missing for a review backend")
	}
}
