package domain

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// sinceDateLayout is the calendar-date form of a since value.
const sinceDateLayout = "2006-01-02"

// ParseSince turns a since filter into the earliest time to keep. It accepts
// a positive Go duration (90m, 24h), a whole number of days or weeks (7d, 2w), a calendar
// date (2026-11-01, midnight in now's location) or an RFC3339 time. An empty
// value returns the zero time, which means "no filter". Errors are validation
// errors that name the accepted forms.
func ParseSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return at, nil
	}
	if at, err := time.ParseInLocation(sinceDateLayout, value, now.Location()); err == nil {
		return at, nil
	}
	window, err := parseSinceWindow(value)
	if err != nil {
		return time.Time{}, err
	}
	return now.Add(-window), nil
}

func parseSinceWindow(value string) (time.Duration, error) {
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if count, ok := strings.CutSuffix(value, suffix); ok {
			n, err := strconv.Atoi(count)
			if err != nil || n <= 0 {
				return 0, sinceError(value)
			}
			return time.Duration(n) * unit, nil
		}
	}
	window, err := time.ParseDuration(value)
	if err != nil || window <= 0 {
		return 0, sinceError(value)
	}
	return window, nil
}

func sinceError(value string) error {
	return NewValidationError("since",
		fmt.Sprintf("%q: want a positive duration (24h, 7d, 2w), a date (2026-11-01) or an RFC3339 time", value))
}
