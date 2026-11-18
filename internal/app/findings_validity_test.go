package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
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

// TestFindingsIgnoreStubRecordsButKeepRealSupersession pins IDX-4 at the review
// layer: generated label+id notes and "nothing new" handoffs produce no
// supersession gap and no duplicate findings, while a real unlinked replacement
// still does. Before the stub filter this fixture reported 4 note gaps, 1
// decision gap and 4 near-duplicate handoffs (plus one exact handoff cluster);
// after it, the decision gap is the only finding.
func TestFindingsIgnoreStubRecordsButKeepRealSupersession(t *testing.T) {
	// Arrange
	var notes []domain.Note
	var handoffs []domain.Handoff
	for i := 0; i < 5; i++ {
		notes = append(notes, domain.Note{
			ID:        fmt.Sprintf("note-%02d", i),
			Title:     fmt.Sprintf("note-2026-11-18-deploy-index-01j2x3aaaaaaaaaaaaaaaa%02d", i),
			Body:      fmt.Sprintf("deploy attempt %d finished", i),
			CreatedAt: dupAt(i),
		})
		handoffs = append(handoffs, domain.Handoff{
			ID: fmt.Sprintf("handoff-%02d", i), Summary: "nothing new", CreatedAt: dupAt(i),
		})
	}
	in := reviewInputs{
		notes:    notes,
		handoffs: handoffs,
		decisions: []domain.Decision{
			{ID: "decision-old", Title: "Index storage engine: SQLite full text", CreatedAt: dupAt(10)},
			{ID: "decision-new", Title: "Index storage engine: SQLite full text search", CreatedAt: dupAt(11)},
		},
	}
	// The stub note titles alone line up as replacements: without the filter
	// the notes would produce 4 gaps. "nothing new" handoffs never line up as
	// replacements, but they repeat exactly, which is duplicate noise.
	if !titleLineage(notes[0].Title, notes[1].Title) {
		t.Fatalf("fixture is not noisy: %q and %q do not line up", notes[0].Title, notes[1].Title)
	}
	rawNotes := make([]supersedeCheckRecord, 0, len(notes))
	for _, note := range notes {
		rawNotes = append(rawNotes, supersedeCheckRecord{id: note.ID, title: note.Title, created: note.CreatedAt})
	}
	if got := len(supersessionGaps(ports.RecordNote, rawNotes)); got != 4 {
		t.Fatalf("stub notes would produce %d gaps, want 4", got)
	}
	rawHandoffs := make([]supersedeCheckRecord, 0, len(handoffs))
	for _, handoff := range handoffs {
		rawHandoffs = append(rawHandoffs, supersedeCheckRecord{id: handoff.ID, title: SummarizeLine(handoff.Summary, 120), created: handoff.CreatedAt})
	}
	if got := len(supersessionGaps(ports.RecordHandoff, rawHandoffs)); got != 0 {
		t.Fatalf("unexpected handoff gaps before the filter: %d", got)
	}

	// Act
	findings := supersessionGapFindings(in)
	duplicates := nearDuplicateFindings(in)
	clusters := duplicatesOfProject("p", in)

	// Assert
	if len(findings) != 1 {
		t.Fatalf("supersession-gap findings = %d %+v, want the real decision gap only", len(findings), findings)
	}
	if findings[0].Kind != FindingSupersessionGap || !strings.Contains(findings[0].Evidence, "decision-old -> decision-new") {
		t.Fatalf("finding = %+v, want the real superseded decision", findings[0])
	}
	if len(duplicates) != 0 || len(clusters) != 0 {
		t.Fatalf("stub duplicates = %d %+v, clusters = %d %+v; want none", len(duplicates), duplicates, len(clusters), clusters)
	}
}

// TestDecisionContradictedIgnoresStubRecords proves a generated label+id
// attempt cannot contradict a decision even when its id repeats the decision's
// words, and neither can a stub decision be contradicted by a real attempt.
func TestDecisionContradictedIgnoresStubRecords(t *testing.T) {
	// Arrange: the stub attempt matches the decision and records a failure.
	decision := domain.Decision{
		ID: "decision-union", Title: "Sync journals with git union merge",
		Status: domain.DecisionAccepted, CreatedAt: dupAt(0),
	}
	stubAttempt := domain.Attempt{
		ID:     "attempt-2026-11-08-sync-journals-git-union-merge-01j2x3y4z5a6b7c8d9e0f1g2h3",
		Title:  "attempt-2026-11-08-sync-journals-git-union-merge-01j2x3y4z5a6b7c8d9e0f1g2h3",
		Result: "failed: the merge duplicated lines", CreatedAt: dupAt(1),
	}
	if !similar(decision.Title, stubAttempt.Title+" "+stubAttempt.Hypothesis) || !attemptRecordsFailure(stubAttempt) {
		t.Fatal("fixture stub attempt would not contradict the decision without the filter")
	}

	// Act / Assert: no contradiction is reported for the stub attempt.
	if findings := decisionFindings(reviewInputs{
		decisions: []domain.Decision{decision},
		attempts:  []domain.Attempt{stubAttempt},
	}); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none for a stub attempt", findings)
	}

	// A stub decision is skipped even against a real, matching attempt.
	stubDecision := domain.Decision{
		ID:     "decision-2026-11-08-sync-journals-git-union-merge-01j2x3y4z5a6b7c8d9e0f1g2h3",
		Title:  "decision-2026-11-08-sync-journals-git-union-merge-01j2x3y4z5a6b7c8d9e0f1g2h3",
		Status: domain.DecisionAccepted, CreatedAt: dupAt(0),
	}
	realAttempt := domain.Attempt{
		ID: "attempt-union", Title: "Sync journals with git union merge",
		Result: "failed: the merge duplicated lines", CreatedAt: dupAt(1),
	}
	if findings := decisionFindings(reviewInputs{
		decisions: []domain.Decision{stubDecision},
		attempts:  []domain.Attempt{realAttempt},
	}); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none for a stub decision", findings)
	}
}
