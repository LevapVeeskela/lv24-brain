package app

import (
	"fmt"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// quarantineIDLimit bounds how many quarantined ids one context or bootstrap
// line names. The full list stays available in `brain review` and
// `brain record review --all`.
const quarantineIDLimit = 10

// quarantinedIDs returns the sorted ids of records that must stay out of
// trusted context: unreviewed agent/connector/import writes and records with
// an origin this engine does not know. Only ids are returned; bodies never
// leave the store through this path.
func quarantinedIDs(notes []domain.Note, decisions []domain.Decision, attempts []domain.Attempt, handoffs []domain.Handoff) []string {
	ids := make([]string, 0)
	for _, note := range notes {
		if note.Quarantined() {
			ids = append(ids, note.ID)
		}
	}
	for _, decision := range decisions {
		if decision.Quarantined() {
			ids = append(ids, decision.ID)
		}
	}
	for _, attempt := range attempts {
		if attempt.Quarantined() {
			ids = append(ids, attempt.ID)
		}
	}
	for _, handoff := range handoffs {
		if handoff.Quarantined() {
			ids = append(ids, handoff.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// QuarantineLine renders the `quarantined: N (ids)` header line used by
// context, bootstrap and catchup, or "" when nothing is quarantined. It never
// carries a record body.
func QuarantineLine(ids []string, included bool) string {
	return quarantineLine(ids, included)
}

// quarantineLine renders the `quarantined: N (ids)` header line, or "" when
// nothing is quarantined. The line never carries a record body.
func quarantineLine(ids []string, included bool) string {
	if len(ids) == 0 {
		return ""
	}
	shown := ids
	suffix := ""
	if len(shown) > quarantineIDLimit {
		suffix = fmt.Sprintf(", … %d more", len(shown)-quarantineIDLimit)
		shown = shown[:quarantineIDLimit]
	}
	line := fmt.Sprintf("quarantined: %d (%s%s)", len(ids), strings.Join(shown, ", "), suffix)
	if included {
		line += " (included)"
	}
	return line
}

// trustedNotes drops records that must not enter trusted context.
func trustedNotes(values []domain.Note) []domain.Note {
	out := make([]domain.Note, 0, len(values))
	for _, value := range values {
		if !value.Quarantined() {
			out = append(out, value)
		}
	}
	return out
}

// trustedDecisions drops records that must not enter trusted context.
func trustedDecisions(values []domain.Decision) []domain.Decision {
	out := make([]domain.Decision, 0, len(values))
	for _, value := range values {
		if !value.Quarantined() {
			out = append(out, value)
		}
	}
	return out
}

// trustedAttempts drops records that must not enter trusted context.
func trustedAttempts(values []domain.Attempt) []domain.Attempt {
	out := make([]domain.Attempt, 0, len(values))
	for _, value := range values {
		if !value.Quarantined() {
			out = append(out, value)
		}
	}
	return out
}

// trustedHandoffs drops records that must not enter trusted context.
func trustedHandoffs(values []domain.Handoff) []domain.Handoff {
	out := make([]domain.Handoff, 0, len(values))
	for _, value := range values {
		if !value.Quarantined() {
			out = append(out, value)
		}
	}
	return out
}
