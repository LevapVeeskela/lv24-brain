package domain

import (
	"errors"
	"testing"
	"time"
)

func TestParseReviewAfterAcceptsDurationsAndDates(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Time
	}{
		{"thirty days", "30d", now.Add(30 * 24 * time.Hour)},
		{"two weeks", "2w", now.Add(14 * 24 * time.Hour)},
		{"hours", "12h", now.Add(12 * time.Hour)},
		{"date", "2027-02-05", time.Date(2027, 2, 5, 0, 0, 0, 0, time.UTC)},
		{"rfc3339", "2027-02-05T10:30:00Z", time.Date(2027, 2, 5, 10, 30, 0, 0, time.UTC)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseReviewAfter(test.value, now)
			if err != nil {
				t.Fatalf("ParseReviewAfter(%q) error = %v", test.value, err)
			}
			if !got.Equal(test.want) {
				t.Fatalf("ParseReviewAfter(%q) = %s, want %s", test.value, got, test.want)
			}
		})
	}
}

func TestParseReviewAfterRejectsBadValues(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	for _, value := range []string{"0d", "-3d", "soon", "90", "999999999999d"} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseReviewAfter(value, now); !errors.Is(err, ErrValidation) {
				t.Fatalf("ParseReviewAfter(%q) error = %v, want validation", value, err)
			}
		})
	}
	empty, err := ParseReviewAfter("  ", now)
	if err != nil || !empty.IsZero() {
		t.Fatalf("ParseReviewAfter(empty) = %s, %v, want the zero time", empty, err)
	}
}

func TestParseReviewWindowBounds(t *testing.T) {
	window, err := ParseReviewWindow("90d")
	if err != nil || window != 90*24*time.Hour {
		t.Fatalf("ParseReviewWindow(90d) = %s, %v", window, err)
	}
	if _, err := ParseReviewWindow("101y"); err == nil {
		t.Fatal("ParseReviewWindow(101y) accepted")
	}
}

func TestReviewScheduleDueAndOverdue(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	future := ReviewSchedule{ReviewAfter: now.Add(time.Hour)}
	if future.Due(now) || future.Overdue(now) != 0 {
		t.Fatalf("future schedule reported due: %+v", future)
	}
	past := ReviewSchedule{ReviewAfter: now.Add(-48 * time.Hour)}
	if !past.Due(now) || past.Overdue(now) != 48*time.Hour {
		t.Fatalf("past schedule = due %v overdue %s", past.Due(now), past.Overdue(now))
	}
	unscheduled := ReviewSchedule{}
	if unscheduled.Due(now) || unscheduled.Overdue(now) != 0 {
		t.Fatal("unscheduled record reported due")
	}
	// The boundary is inclusive: at the review instant the record is due.
	boundary := ReviewSchedule{ReviewAfter: now}
	if !boundary.Due(now) {
		t.Fatal("review instant must be due")
	}
}

func TestFormatReviewWindowKeepsWholeDays(t *testing.T) {
	if got := FormatReviewWindow(90 * 24 * time.Hour); got != "90d" {
		t.Fatalf("FormatReviewWindow(90d) = %q", got)
	}
	if got := FormatReviewWindow(36 * time.Hour); got != "36h0m0s" {
		t.Fatalf("FormatReviewWindow(36h) = %q", got)
	}
}
