package domain

import (
	"fmt"
	"strings"
	"time"
)

// ParseSnoozeUntil turns a snooze value into the instant a proposal returns to
// the review queue. It accepts a positive Go duration (90m, 24h), a whole
// number of days or weeks (7d, 2w), a calendar date (2026-12-01, midnight in
// now's location) or an RFC3339 time. An empty value returns the zero time,
// which means "clear the snooze". Errors are validation errors that name the
// accepted forms.
func ParseSnoozeUntil(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return at.UTC(), nil
	}
	if at, err := time.ParseInLocation(sinceDateLayout, value, now.Location()); err == nil {
		return at.UTC(), nil
	}
	window, err := parseSinceWindow(value)
	if err != nil {
		return time.Time{}, NewValidationError("snooze.until",
			fmt.Sprintf("%q: want a positive duration (7d, 24h), a date (2026-12-01) or an RFC3339 time", value))
	}
	return now.Add(window).UTC(), nil
}
