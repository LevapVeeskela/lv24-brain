package domain

import (
	"fmt"
	"strings"
	"time"
)

// Review cadence defaults, per record kind. They are prompts to re-check a
// claim, never expiries: an overdue record stays current until a person
// confirms it, supersedes it or moves its review date.
const (
	// DefaultReviewDecision is the review window of a decision.
	DefaultReviewDecision = 90 * 24 * time.Hour
	// DefaultReviewAttempt is the review window of an attempt.
	DefaultReviewAttempt = 60 * 24 * time.Hour
	// DefaultReviewNote is the review window of a note.
	DefaultReviewNote = 180 * 24 * time.Hour
	// DefaultReviewHandoff is the review window of a handoff.
	DefaultReviewHandoff = 90 * 24 * time.Hour
	// DefaultReviewState is the age at which a project state looks due for a
	// re-read when its freshness window is open.
	DefaultReviewState = 14 * 24 * time.Hour
	// DefaultReviewProposal is the age at which a pending proposal is due.
	DefaultReviewProposal = 14 * 24 * time.Hour
)

// maxReviewWindow bounds a configured review window so a typo cannot push a
// review date past the engine's useful horizon.
const maxReviewWindow = 100 * 365 * 24 * time.Hour

// ReviewSchedule is the review cadence metadata of a curated record. It is
// embedded in Note, Decision, Attempt and Handoff, so the fields serialize next
// to the record fields and round-trip through frontmatter.
//
// The zero value keeps the pre-review behavior: the record has no review date
// and review derives one from its creation time and the kind's default.
type ReviewSchedule struct {
	// ReviewAfter is when the record should be re-checked. Zero means the
	// kind's default applies to CreatedAt.
	ReviewAfter time.Time `json:"reviewAfter,omitzero"`
	// ReviewedAt is when a person or agent last reviewed the record. Zero
	// means never reviewed.
	ReviewedAt time.Time `json:"reviewedAt,omitzero"`
}

// Due reports whether the review date has passed at now.
func (s ReviewSchedule) Due(now time.Time) bool {
	return !s.ReviewAfter.IsZero() && !s.ReviewAfter.After(now)
}

// Overdue is how long ago the review date passed; zero while it is ahead.
func (s ReviewSchedule) Overdue(now time.Time) time.Duration {
	if !s.Due(now) {
		return 0
	}
	return now.Sub(s.ReviewAfter)
}

// ParseReviewWindow parses a positive review window. It accepts a whole number
// of days (90d), weeks (2w) and a Go duration (2160h). The value is a length of
// time, not an instant; see ParseReviewAfter for the instant form.
func ParseReviewWindow(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, NewValidationError("review-after", "is required")
	}
	window, err := parseSinceWindow(value)
	if err != nil {
		return 0, NewValidationError("review-after",
			fmt.Sprintf("%q: want a positive duration (90d, 2w, 2160h)", value))
	}
	if window > maxReviewWindow {
		return 0, NewValidationError("review-after",
			fmt.Sprintf("%q is longer than %d years", value, int(maxReviewWindow.Hours()/24/365)))
	}
	return window, nil
}

// ParseReviewAfter turns a review-after value into the instant a record becomes
// due. It accepts a duration relative to now (90d, 30d, 12h) and an absolute
// RFC3339 time or calendar date (2027-02-05, midnight UTC). An empty value
// returns the zero time, which means "no review date".
func ParseReviewAfter(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return at.UTC(), nil
	}
	if at, err := time.Parse(sinceDateLayout, value); err == nil {
		return at.UTC(), nil
	}
	window, err := ParseReviewWindow(value)
	if err != nil {
		return time.Time{}, NewValidationError("review-after",
			fmt.Sprintf("%q: want a positive duration (90d, 30d) or a date (2027-02-05)", value))
	}
	return now.UTC().Add(window), nil
}

// FormatReviewWindow renders a duration as whole days when it is one, so a
// configuration round-trip keeps "90d" instead of "2160h".
func FormatReviewWindow(window time.Duration) string {
	if window > 0 && window%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(window.Hours()/24))
	}
	return window.String()
}
