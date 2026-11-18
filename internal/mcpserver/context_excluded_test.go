package mcpserver

import (
	"context"
	"fmt"
	"testing"
)

// excludedBackend returns a context with more exclusions than the transport
// cap, so the structuredContent rule can be checked end to end.
type excludedBackend struct {
	fakeBackend
}

func (b *excludedBackend) Context(_ context.Context, in ContextInput) (TaskContext, error) {
	excluded := make([]ContextExclusion, 0, 30)
	for i := 0; i < 30; i++ {
		excluded = append(excluded, ContextExclusion{
			ID: fmt.Sprintf("note-alpha-%02d", i), Kind: "note",
			Reason: ContextExcludeOverBudgetSample, Score: 0.5,
		})
	}
	return TaskContext{
		ProjectID: in.ProjectID, Query: in.Query, Budget: in.Budget,
		Excluded: excluded, ExcludedTotal: len(excluded), NearMisses: excluded[:3],
		Summary: "# alpha — context",
	}, nil
}

// ContextExcludeOverBudgetSample is the reason the sample backend reports. It
// is spelled here because the transport does not import the app package.
const ContextExcludeOverBudgetSample = "over-budget"

// TestContextExcludedListIsCappedAndTotaled checks the shared JSON rule: at
// most MaxContextExclusions entries travel and excluded_total keeps the count.
func TestContextExcludedListIsCappedAndTotaled(t *testing.T) {
	// Arrange
	excluded := make([]ContextExclusion, 0, 60)
	for i := 0; i < 60; i++ {
		excluded = append(excluded, ContextExclusion{
			ID: fmt.Sprintf("decision-alpha-%02d", i), Reason: ContextExcludeOverBudgetSample,
		})
	}

	// Act
	result := TaskContext{Excluded: excluded}.WithEmptyLists()
	again := result.WithEmptyLists()

	// Assert
	if len(result.Excluded) != MaxContextExclusions {
		t.Fatalf("excluded = %d entries, want the cap of %d", len(result.Excluded), MaxContextExclusions)
	}
	if result.ExcludedTotal != len(excluded) {
		t.Fatalf("excluded_total = %d, want %d", result.ExcludedTotal, len(excluded))
	}
	if len(again.Excluded) != MaxContextExclusions || again.ExcludedTotal != len(excluded) {
		t.Fatalf("second pass = %d entries, total %d", len(again.Excluded), again.ExcludedTotal)
	}
}

// TestContextExcludedDerivedFromTruncated checks the fallback: a backend that
// only carries truncated[] still yields exclusions with a code and kind.
func TestContextExcludedDerivedFromTruncated(t *testing.T) {
	// Arrange
	truncated := make([]ContextTruncation, 0, 25)
	for i := 0; i < 24; i++ {
		truncated = append(truncated, ContextTruncation{
			ID: fmt.Sprintf("decision-alpha-%02d", i), Section: "query",
			Reason: "over-budget: does not fit the query layer",
		})
	}
	truncated = append(truncated, ContextTruncation{
		ID: "decision-alpha-old", Section: "decisions", Reason: "superseded: replaced by decision-alpha-new",
	})

	// Act
	result := TaskContext{Truncated: truncated}.WithEmptyLists()

	// Assert
	if len(result.Excluded) != len(truncated) {
		t.Fatalf("derived excluded = %d, want %d", len(result.Excluded), len(truncated))
	}
	if result.Excluded[0].Reason != ContextExcludeOverBudgetSample || result.Excluded[0].Kind != "decision" {
		t.Fatalf("first exclusion = %+v", result.Excluded[0])
	}
	if got := result.Excluded[len(result.Excluded)-1].Reason; got != "superseded" {
		t.Fatalf("superseded reason = %q", got)
	}
	if len(result.NearMisses) != MaxContextNearMisses {
		t.Fatalf("derived near misses = %d, want %d", len(result.NearMisses), MaxContextNearMisses)
	}
}

// TestMCPContextStructuredExcludedCaps checks the tool itself: structuredContent
// carries a sample of excluded records with the honest total and three near
// misses, inside the existing transport cap.
func TestMCPContextStructuredExcludedCaps(t *testing.T) {
	// Act
	result := toolCall(t, &excludedBackend{}, "brain_context", `{"query":"cache"}`)

	// Assert
	obj := structured(t, result)
	excluded, ok := obj["excluded"].([]any)
	if !ok {
		t.Fatalf("structuredContent lacks excluded: %v", obj)
	}
	if len(excluded) != MaxContextTruncations {
		t.Fatalf("excluded = %d entries, want the MCP cap of %d", len(excluded), MaxContextTruncations)
	}
	total, ok := obj["excluded_total"].(float64)
	if !ok || int(total) != 30 {
		t.Fatalf("excluded_total = %v, want 30", obj["excluded_total"])
	}
	near, ok := obj["near_misses"].([]any)
	if !ok || len(near) != MaxContextNearMisses {
		t.Fatalf("near_misses = %v, want %d entries", obj["near_misses"], MaxContextNearMisses)
	}
	first, ok := excluded[0].(map[string]any)
	if !ok || first["reason"] != ContextExcludeOverBudgetSample || first["id"] == "" {
		t.Fatalf("first excluded entry = %v", excluded[0])
	}
}

// TestMCPContextStructuredDerivesExclusions checks that the tool reports the
// derived exclusions when the backend predates the exclusion field.
func TestMCPContextStructuredDerivesExclusions(t *testing.T) {
	// Act
	result := toolCall(t, &truncatedBackend{}, "brain_context", `{"query":"cache"}`)

	// Assert
	obj := structured(t, result)
	excluded, ok := obj["excluded"].([]any)
	if !ok || len(excluded) != MaxContextTruncations {
		t.Fatalf("derived excluded = %v, want %d entries", obj["excluded"], MaxContextTruncations)
	}
	if total, _ := obj["excluded_total"].(float64); int(total) != 25 {
		t.Fatalf("excluded_total = %v, want 25", obj["excluded_total"])
	}
	if near, _ := obj["near_misses"].([]any); len(near) != MaxContextNearMisses {
		t.Fatalf("near_misses = %v, want %d entries", obj["near_misses"], MaxContextNearMisses)
	}
}

// truncatedBackend returns only the truncation bookkeeping, like a backend
// written before the exclusion view existed.
type truncatedBackend struct {
	fakeBackend
}

func (b *truncatedBackend) Context(_ context.Context, in ContextInput) (TaskContext, error) {
	truncated := make([]ContextTruncation, 0, 25)
	for i := 0; i < 25; i++ {
		truncated = append(truncated, ContextTruncation{
			ID: fmt.Sprintf("note-alpha-%02d", i), Section: "query",
			Reason: "over-budget: does not fit the query layer",
		})
	}
	return TaskContext{
		ProjectID: in.ProjectID, Query: in.Query, Budget: in.Budget,
		Truncated: truncated, Summary: "# alpha — context",
	}, nil
}
