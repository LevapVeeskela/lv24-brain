package app

import (
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// validityFilter mirrors the temporal options of a use case.
func validityFilter(asOf time.Time, includeSuperseded bool) domain.ValidityFilter {
	return domain.ValidityFilter{AsOf: asOf, IncludeSuperseded: includeSuperseded}
}

// filterNotes keeps the notes the filter allows at now.
func filterNotes(notes []domain.Note, filter domain.ValidityFilter, now time.Time) []domain.Note {
	out := make([]domain.Note, 0, len(notes))
	for _, note := range notes {
		if filter.Allows(note.Validity, note.CreatedAt, now) {
			out = append(out, note)
		}
	}
	return out
}

// filterDecisions keeps the decisions the filter allows at now.
func filterDecisions(decisions []domain.Decision, filter domain.ValidityFilter, now time.Time) []domain.Decision {
	out := make([]domain.Decision, 0, len(decisions))
	for _, decision := range decisions {
		if filter.Allows(decision.Validity, decision.CreatedAt, now) {
			out = append(out, decision)
		}
	}
	return out
}

// filterAttempts keeps the attempts the filter allows at now.
func filterAttempts(attempts []domain.Attempt, filter domain.ValidityFilter, now time.Time) []domain.Attempt {
	out := make([]domain.Attempt, 0, len(attempts))
	for _, attempt := range attempts {
		if filter.Allows(attempt.Validity, attempt.CreatedAt, now) {
			out = append(out, attempt)
		}
	}
	return out
}

// filterHandoffs keeps the handoffs the filter allows at now.
func filterHandoffs(handoffs []domain.Handoff, filter domain.ValidityFilter, now time.Time) []domain.Handoff {
	out := make([]domain.Handoff, 0, len(handoffs))
	for _, handoff := range handoffs {
		if filter.Allows(handoff.Validity, handoff.CreatedAt, now) {
			out = append(out, handoff)
		}
	}
	return out
}
