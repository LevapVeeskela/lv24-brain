package app_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
)

// excludeReasons is the RET-4 vocabulary: every dropped record names one of
// these codes, whatever stage dropped it.
var excludeReasons = []string{
	app.ContextExcludeOverBudget, app.ContextExcludeLowerRank, app.ContextExcludeSuperseded,
	app.ContextExcludeStale, app.ContextExcludeQuarantined, app.ContextExcludeOtherOwner,
	app.ContextExcludeOther,
}

func knownExcludeReason(reason string) bool {
	for _, known := range excludeReasons {
		if reason == known {
			return true
		}
	}
	return false
}

func packedIDs(pack app.ContextPack) map[string]bool {
	ids := make(map[string]bool, 32)
	for _, section := range pack.Sections {
		for _, record := range section.Records {
			ids[record.ID] = true
		}
	}
	return ids
}

// TestContextBudgetExcludedReasonsAndNearMisses is the RET-4 budget contract:
// at 200/500/4000 tokens every drop carries a known reason, the summary prints
// the per-layer dropped line and the near-miss line, and it stays in budget.
func TestContextBudgetExcludedReasonsAndNearMisses(t *testing.T) {
	for _, budget := range []int{200, 500, 4000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			// Arrange
			ctx := context.Background()
			f := newPackFixture(t)

			// Act
			result, err := f.app.Context(ctx, app.ContextOptions{
				ProjectID: "p", Query: "packtoken", TokenBudget: budget,
			})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if got := textbudget.Tokens(result.Summary); got > budget {
				t.Fatalf("summary costs %d tokens, budget %d:\n%s", got, budget, result.Summary)
			}
			if result.ExcludedTotal != len(result.Excluded) {
				t.Fatalf("excluded_total = %d, excluded = %d", result.ExcludedTotal, len(result.Excluded))
			}
			if budget == 200 && result.ExcludedTotal == 0 {
				t.Fatal("a 200-token budget excluded nothing; the fixture cannot prove the reasons")
			}
			packed := packedIDs(result.Pack)
			for _, exclusion := range result.Excluded {
				if !knownExcludeReason(exclusion.Reason) {
					t.Fatalf("%s has unknown reason %q", exclusion.ID, exclusion.Reason)
				}
				if exclusion.Section == "" {
					t.Fatalf("%s has no layer", exclusion.ID)
				}
				if packed[exclusion.ID] {
					t.Fatalf("%s is both packed and excluded", exclusion.ID)
				}
			}
			for _, layer := range []string{
				app.ContextLayerQuery, app.ContextLayerDecisions,
				app.ContextLayerAttempts, app.ContextLayerLinked,
			} {
				count := 0
				for _, exclusion := range result.Excluded {
					if exclusion.Section == layer {
						count++
					}
				}
				line := fmt.Sprintf("- … %d more dropped (", count)
				if count > 0 && !strings.Contains(result.Summary, line) {
					t.Fatalf("%s dropped %d records but the summary lacks %q:\n%s", layer, count, line, result.Summary)
				}
			}
			if len(result.NearMisses) > 3 {
				t.Fatalf("near misses = %d, want at most 3", len(result.NearMisses))
			}
			if len(result.NearMisses) > 0 {
				line := fmt.Sprintf("- … %d lower-ranked omitted", len(result.NearMisses))
				if !strings.Contains(result.Summary, line) {
					t.Fatalf("summary lacks %q:\n%s", line, result.Summary)
				}
				for _, miss := range result.NearMisses {
					if miss.Section != app.ContextLayerQuery {
						t.Fatalf("near miss %s is from %s, want the query layer", miss.ID, miss.Section)
					}
					if miss.Score <= 0 {
						t.Fatalf("near miss %s has no score", miss.ID)
					}
					if miss.Reason != app.ContextExcludeOverBudget && miss.Reason != app.ContextExcludeLowerRank {
						t.Fatalf("near miss %s has reason %q", miss.ID, miss.Reason)
					}
				}
			}
		})
	}
}

// TestContextExcludedSupersededRecord checks the filter reason: a superseded
// record that still matches the query is named as superseded, not hidden.
func TestContextExcludedSupersededRecord(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	old := f.decision(t, "Superseded decision supersededmarker", "supersededmarker old body")
	f.advance(time.Hour)
	current := f.decision(t, "Replacement decision supersededmarker", "supersededmarker new body")
	if _, err := f.app.Supersede(ctx, "p", old, current); err != nil {
		t.Fatal(err)
	}

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "supersededmarker", TokenBudget: 4000,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	reason := ""
	for _, exclusion := range result.Excluded {
		if exclusion.ID == old {
			reason = exclusion.Reason
		}
	}
	if reason != app.ContextExcludeSuperseded {
		t.Fatalf("%s excluded as %q, want %q (excluded: %+v)", old, reason, app.ContextExcludeSuperseded, result.Excluded)
	}
	if strings.Contains(result.Summary, "supersededmarker old body") {
		t.Fatalf("summary carried the superseded body:\n%s", result.Summary)
	}
}

// TestContextExcludedStaleRecord checks the since window reason: a record
// outside the freshness window is named stale.
func TestContextExcludedStaleRecord(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := newContextFixture(t, true)
	old := f.decision(t, "Stale decision stalemarker", "stalemarker old body")
	f.advance(10 * 24 * time.Hour)
	f.decision(t, "Recent decision stalemarker", "stalemarker recent body")

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "stalemarker", Since: "7d", TokenBudget: 4000,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	reason := ""
	for _, exclusion := range result.Excluded {
		if exclusion.ID == old {
			reason = exclusion.Reason
		}
	}
	if reason != app.ContextExcludeStale {
		t.Fatalf("%s excluded as %q, want %q (excluded: %+v)", old, reason, app.ContextExcludeStale, result.Excluded)
	}
}

// TestContextNearMissesCappedAtThree checks the bucket size: a tight budget
// keeps the top three below-the-cut matches, not the whole dropped tail.
func TestContextNearMissesCappedAtThree(t *testing.T) {
	// Arrange
	ctx := context.Background()
	f := smallNoteFixture(t, 20)

	// Act
	result, err := f.app.Context(ctx, app.ContextOptions{
		ProjectID: "p", Query: "retmarker", TokenBudget: 200,
	})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Excluded) < 4 {
		t.Fatalf("excluded = %d, want a dropped tail", len(result.Excluded))
	}
	if len(result.NearMisses) != 3 {
		t.Fatalf("near misses = %d, want 3", len(result.NearMisses))
	}
	seen := map[string]bool{}
	for _, miss := range result.NearMisses {
		if seen[miss.ID] {
			t.Fatalf("near miss %s listed twice", miss.ID)
		}
		seen[miss.ID] = true
	}
}
