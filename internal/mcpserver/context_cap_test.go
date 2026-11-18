package mcpserver

import (
	"fmt"
	"testing"
)

// TestContextTruncatedListIsCappedAndTotaled checks the transport rule: at most
// MaxContextTruncations entries travel, and total_truncated keeps the full
// count so the caller can see the list is a sample.
func TestContextTruncatedListIsCappedAndTotaled(t *testing.T) {
	// Arrange
	truncations := make([]ContextTruncation, 0, 25)
	for i := 0; i < 25; i++ {
		truncations = append(truncations, ContextTruncation{
			ID: fmt.Sprintf("record-%02d", i), Section: "query", Reason: "does not fit",
		})
	}

	// Act
	context := TaskContext{Truncated: truncations}.WithEmptyLists()
	again := context.WithEmptyLists()

	// Assert
	if len(context.Truncated) != MaxContextTruncations {
		t.Fatalf("truncated = %d entries, want the cap of %d", len(context.Truncated), MaxContextTruncations)
	}
	if context.TotalTruncated != len(truncations) {
		t.Fatalf("total_truncated = %d, want %d", context.TotalTruncated, len(truncations))
	}
	if context.StateAsOf != StateAsOfCurrent {
		t.Fatalf("state_as_of = %q, want %q", context.StateAsOf, StateAsOfCurrent)
	}
	// Applying the normalizer twice must not shrink the total.
	if len(again.Truncated) != MaxContextTruncations || again.TotalTruncated != len(truncations) {
		t.Fatalf("second pass = %d entries, total %d", len(again.Truncated), again.TotalTruncated)
	}
}

// TestMCPServerContextReportsTruncationTotal checks the cap and the total in the
// structured content of brain_context itself.
func TestMCPServerContextReportsTruncationTotal(t *testing.T) {
	// Arrange
	backend := &fakeBackend{}

	// Act
	result := toolCall(t, backend, "brain_context", `{"query":"cache"}`)

	// Assert
	obj := structured(t, result)
	if obj["state_as_of"] != StateAsOfCurrent {
		t.Fatalf("state_as_of = %v, want %q", obj["state_as_of"], StateAsOfCurrent)
	}
	if _, ok := obj["total_truncated"]; !ok {
		t.Fatalf("structured result lacks total_truncated: %v", obj)
	}
}
