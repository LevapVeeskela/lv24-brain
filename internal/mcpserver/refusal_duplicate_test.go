package mcpserver

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestRefusalFromDuplicateNamesTheExistingRecord: a duplicate write must reach
// the agent as DUPLICATE with the existing id, never as a retryable conflict
// that loops.
func TestRefusalFromDuplicateNamesTheExistingRecord(t *testing.T) {
	err := fmt.Errorf("create note: %w", &domain.DuplicateError{Kind: "note", ID: "note-2026-11-15-release-01abc", ProjectID: "p"})
	refusal := refusalFrom(err)
	if refusal.Code != RefusalDuplicate {
		t.Fatalf("code = %q, want %q", refusal.Code, RefusalDuplicate)
	}
	if !strings.Contains(refusal.TryInstead, "note-2026-11-15-release-01abc") {
		t.Fatalf("try_instead = %q, want the existing id", refusal.TryInstead)
	}
	if refusal.Retryable {
		t.Fatal("a duplicate is not retryable unchanged")
	}
	if !errors.Is(err, domain.ErrDuplicate) {
		t.Fatal("the wrapped error no longer matches domain.ErrDuplicate")
	}
}

// TestRefusalDuplicateEnvelopeRendersHints keeps the text content as useful as
// the structured envelope for hosts that only show text.
func TestRefusalDuplicateEnvelopeRendersHints(t *testing.T) {
	raw, rpcErr := refusalResult(refusalFrom(&domain.DuplicateError{Kind: "decision", ID: "decision-x", ProjectID: "p"}))
	if rpcErr != nil {
		t.Fatalf("rpc error = %+v", rpcErr)
	}
	text := string(raw)
	for _, want := range []string{RefusalDuplicate, "decision-x", "try instead:", "read the existing decision"} {
		if !strings.Contains(text, want) {
			t.Fatalf("refusal result missing %q:\n%s", want, text)
		}
	}
}
