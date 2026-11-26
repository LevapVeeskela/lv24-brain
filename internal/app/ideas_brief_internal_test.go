package app

import (
	"testing"
	"time"
	_ "time/tzdata"
)

// TestIdeaDateOffsetAcrossDST pins the calendar-day arithmetic. Europe/Berlin
// springs forward on 2026-03-29, so 2026-03-28 -> 2026-03-30 is two calendar
// days but only 47 hours; dividing local midnights by 24 would label it one.
func TestIdeaDateOffsetAcrossDST(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatalf("load Europe/Berlin: %v", err)
	}
	cases := []struct {
		from, to string
		want     int
	}{
		{"2026-03-28", "2026-03-30", 2},
		{"2026-03-30", "2026-03-28", -2},
		{"2026-03-29", "2026-03-30", 1},
		{"2026-03-30", "2026-03-30", 0},
		{"2026-10-24", "2026-10-26", 2},
		{"2026-10-26", "2026-10-24", -2},
	}
	for _, tc := range cases {
		if got := ideaDateOffsetIn(tc.from, tc.to, berlin); got != tc.want {
			t.Errorf("ideaDateOffsetIn(%q, %q, Europe/Berlin) = %d, want %d", tc.from, tc.to, got, tc.want)
		}
	}
}
