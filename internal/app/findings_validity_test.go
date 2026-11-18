package app

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// supersessionGapCount counts the supersession-gap findings of one review input.
func supersessionGapCount(in reviewInputs) int {
	count := 0
	for _, finding := range supersessionGapFindings(in) {
		if finding.Kind == FindingSupersessionGap {
			count++
		}
	}
	return count
}

func TestFindingSupersessionGapNeedsTitleLineage(t *testing.T) {
	tests := []struct {
		name string
		in   reviewInputs
		want int
	}{
		{
			name: "release notes for different versions are a series",
			in: reviewInputs{notes: []domain.Note{
				{ID: "note-060", Title: "Release notes 0.6.0", CreatedAt: dupAt(0)},
				{ID: "note-061", Title: "Release notes 0.6.1", CreatedAt: dupAt(1)},
				{ID: "note-062", Title: "Release notes 0.6.2", CreatedAt: dupAt(2)},
			}},
			want: 0,
		},
		{
			name: "the same release worded differently is one topic",
			in: reviewInputs{notes: []domain.Note{
				{ID: "note-plan", Title: "Release notes 0.6.0", CreatedAt: dupAt(0)},
				{ID: "note-shipped", Title: "0.6.0 shipped", CreatedAt: dupAt(1)},
			}},
			want: 1,
		},
		{
			name: "unrelated decisions sharing generic words",
			in: reviewInputs{decisions: []domain.Decision{
				{ID: "decision-wire", Title: "Keep the wire format stable for desktop", CreatedAt: dupAt(0)},
				{ID: "decision-journal", Title: "Keep the journal format stable for git", CreatedAt: dupAt(1)},
				{ID: "decision-installer", Title: "Keep the desktop installer format stable for Windows", CreatedAt: dupAt(2)},
			}},
			want: 0,
		},
		{
			name: "a true superseded decision",
			in: reviewInputs{decisions: []domain.Decision{
				{ID: "decision-old", Title: "Index storage engine: SQLite full text", CreatedAt: dupAt(0)},
				{ID: "decision-new", Title: "Index storage engine: SQLite full text search", CreatedAt: dupAt(1)},
			}},
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := supersessionGapCount(tt.in); got != tt.want {
				t.Fatalf("supersession-gap findings = %d, want %d: %+v", got, tt.want, supersessionGapFindings(tt.in))
			}
		})
	}
}

func TestDecisionContradictedNeedsExplicitFailure(t *testing.T) {
	decision := domain.Decision{
		ID: "decision-union", Title: "Sync journals with git union merge",
		Status: domain.DecisionAccepted, CreatedAt: dupAt(0),
	}
	tests := []struct {
		name   string
		result string
		reason string
		want   int
	}{
		{name: "a neutral result is not a failure", result: "the journal had duplicated lines", want: 0},
		{name: "an explicit failure is reported", result: "failed: duplicated lines in the journal", want: 1},
		{name: "a Russian failure is reported", result: "не сработал: дублирует строки", want: 1},
		{name: "an abandoned reason counts when the result is empty", reason: "abandoned, it broke the merge", want: 1},
		{name: "a success is not a failure", result: "worked: both sides were kept", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := reviewInputs{
				decisions: []domain.Decision{decision},
				attempts: []domain.Attempt{{
					ID: "attempt-union", Title: "Union merge for journals",
					Result: tt.result, Reason: tt.reason, CreatedAt: dupAt(1),
				}},
			}
			got := 0
			for _, finding := range decisionFindings(in) {
				if finding.Kind == FindingDecisionContradicted {
					got++
				}
			}
			if got != tt.want {
				t.Fatalf("decision-contradicted findings = %d, want %d: %+v", got, tt.want, decisionFindings(in))
			}
		})
	}
}

// A stale superseded record with no replacement must stay reported: the lineage
// gate makes supersession-gap quieter, not stale-superseded.
func TestFindingStaleSupersededKeptByLineageGate(t *testing.T) {
	ended := domain.Validity{ValidFrom: dupAt(0), ValidTo: dupAt(5)}
	in := reviewInputs{notes: []domain.Note{{
		ID: "note-old", Title: "Release notes 0.6.0", Validity: ended, CreatedAt: dupAt(0),
	}}}
	findings := staleSupersededFindings(in, dupAt(10))
	if len(findings) != 1 || findings[0].Kind != FindingStaleSuperseded {
		t.Fatalf("stale-superseded findings = %+v", findings)
	}
}
