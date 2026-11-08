package domain

import (
	"errors"
	"testing"
	"time"
)

func TestParseSince(t *testing.T) {
	now := time.Date(2026, time.November, 8, 15, 30, 0, 0, time.UTC)
	tests := []struct {
		value string
		want  time.Time
	}{
		{"", time.Time{}},
		{"  ", time.Time{}},
		{"7d", now.Add(-7 * 24 * time.Hour)},
		{"24h", now.Add(-24 * time.Hour)},
		{"90m", now.Add(-90 * time.Minute)},
		{"2026-11-01", time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-11-01T10:00:00+03:00", time.Date(2026, time.November, 1, 7, 0, 0, 0, time.UTC)},
	}
	for _, test := range tests {
		got, err := ParseSince(test.value, now)
		if err != nil || !got.Equal(test.want) {
			t.Errorf("ParseSince(%q) = %v, %v; want %v", test.value, got, err, test.want)
		}
	}
}

func TestParseSinceRejectsInvalidValues(t *testing.T) {
	now := time.Date(2026, time.November, 8, 0, 0, 0, 0, time.UTC)
	for _, value := range []string{"0d", "-1d", "xd", "-24h", "0s", "soon", "2026-13-01", "7"} {
		if _, err := ParseSince(value, now); !errors.Is(err, ErrValidation) {
			t.Errorf("ParseSince(%q) error = %v, want a validation error", value, err)
		}
	}
}
