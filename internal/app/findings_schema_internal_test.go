package app

import (
	"strings"
	"testing"
)

// TestEveryFindingKindHasReviewMetadata is the table that keeps the review
// contract: no producer can emit a finding without a severity, a why and a next
// action, whichever kind it is.
func TestEveryFindingKindHasReviewMetadata(t *testing.T) {
	for _, kind := range findingKinds {
		t.Run(string(kind), func(t *testing.T) {
			spec, ok := findingSpecs[kind]
			if !ok {
				t.Fatalf("finding kind %q has no spec", kind)
			}
			if !spec.Severity.Valid() {
				t.Fatalf("severity %q is not error/warn/info", spec.Severity)
			}
			if strings.TrimSpace(spec.Why) == "" {
				t.Fatal("why is empty")
			}
			if strings.TrimSpace(spec.Next) == "" {
				t.Fatal("next is empty")
			}
			if strings.TrimSpace(spec.Confirm) == "" {
				t.Fatal("confirm is empty")
			}
			bare := finalizeFinding(Finding{Kind: kind, Message: "x"})
			if !bare.Severity.Valid() || bare.Why == "" || bare.Next == "" || bare.Confirm == "" {
				t.Fatalf("finalizeFinding left a gap: %+v", bare)
			}
		})
	}
	if len(findingSpecs) != len(findingKinds) {
		t.Fatalf("findingSpecs has %d entries, findingKinds %d", len(findingSpecs), len(findingKinds))
	}
}

// TestFinalizeFindingKeepsProducerOverrides proves a producer may escalate a
// finding without losing the shared metadata.
func TestFinalizeFindingKeepsProducerOverrides(t *testing.T) {
	finding := finalizeFinding(Finding{Kind: FindingReviewOverdue, Severity: SeverityError, Why: "custom"})
	if finding.Severity != SeverityError || finding.Why != "custom" {
		t.Fatalf("overrides lost: %+v", finding)
	}
	if finding.Next == "" || finding.Confirm == "" {
		t.Fatalf("metadata not filled: %+v", finding)
	}
	unknown := finalizeFinding(Finding{Kind: "not-a-kind", Message: "x"})
	if !unknown.Severity.Valid() {
		t.Fatalf("unknown kind severity = %q", unknown.Severity)
	}
}
