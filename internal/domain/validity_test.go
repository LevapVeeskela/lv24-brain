package domain

import (
	"errors"
	"testing"
	"time"
)

func TestValidityValidate(t *testing.T) {
	created := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	ok := Validity{ValidFrom: created, ValidTo: created.Add(24 * time.Hour), Supersedes: "decision-2026-10-01-old"}
	if err := ok.Validate("validity", created, "decision-2026-11-01-new"); err != nil {
		t.Fatalf("valid validity rejected: %v", err)
	}
	cases := map[string]Validity{
		"end equal to start":      {ValidFrom: created, ValidTo: created},
		"end before start":        {ValidFrom: created, ValidTo: created.Add(-time.Hour)},
		"self superseded_by":      {SupersededBy: "note-2026-11-01-self"},
		"self supersedes":         {Supersedes: "note-2026-11-01-self"},
		"superseded_by not an id": {SupersededBy: "readme"},
		"supersedes path":         {Supersedes: "../decision-2026-01-01-x"},
		"negative confidence":     {Confidence: -0.1},
		"confidence above one":    {Confidence: 1.5},
	}
	for name, validity := range cases {
		if err := validity.Validate("validity", created, "note-2026-11-01-self"); err == nil {
			t.Errorf("%s: invalid validity accepted", name)
		} else if !errors.Is(err, ErrValidation) {
			t.Errorf("%s: error = %v, want ErrValidation", name, err)
		}
	}
}

func TestValidityAllowsAtBoundaries(t *testing.T) {
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	valid := Validity{ValidFrom: created.Add(time.Hour), ValidTo: created.Add(3 * time.Hour)}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"before valid_from", valid.ValidFrom.Add(-time.Nanosecond), false},
		{"at valid_from", valid.ValidFrom, true},
		{"inside", valid.ValidFrom.Add(time.Hour), true},
		{"just before valid_to", valid.ValidTo.Add(-time.Nanosecond), true},
		{"at valid_to", valid.ValidTo, false},
		{"after valid_to", valid.ValidTo.Add(time.Hour), false},
	}
	for _, tc := range cases {
		if got := valid.AllowsAt(tc.at, created); got != tc.want {
			t.Errorf("%s: AllowsAt = %v, want %v", tc.name, got, tc.want)
		}
	}
	open := Validity{ValidFrom: created}
	if !open.AllowsAt(created.Add(1000*time.Hour), created) || open.AllowsAt(created.Add(-time.Hour), created) {
		t.Error("a record without valid_to must stay current after valid_from")
	}
	// A legacy record without valid_from is valid from its creation time.
	if !open.AllowsAt(created, created) || open.AllowsAt(created.Add(-time.Nanosecond), created) {
		t.Error("legacy valid_from did not default to created_at")
	}
}

func TestValidityFilter(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	created := now.Add(-30 * 24 * time.Hour)
	current := Validity{ValidFrom: created}
	superseded := Validity{ValidFrom: created, ValidTo: now.Add(-24 * time.Hour), SupersededBy: "decision-2026-11-14-new"}
	expired := Validity{ValidFrom: created, ValidTo: now.Add(time.Hour)}

	defaultFilter := ValidityFilter{}
	if !defaultFilter.Allows(current, created, now) {
		t.Error("current record excluded by default")
	}
	if defaultFilter.Allows(superseded, created, now) {
		t.Error("superseded record included by default")
	}
	if !defaultFilter.Allows(expired, created, now) {
		t.Error("record whose valid_to is in the future must stay current")
	}

	all := ValidityFilter{IncludeSuperseded: true}
	if !all.Allows(superseded, created, now) {
		t.Error("--include-superseded did not keep the superseded record")
	}

	before := ValidityFilter{AsOf: created.Add(-time.Hour)}
	if before.Allows(superseded, created, now) || before.Allows(current, created, now) {
		t.Error("as-of before valid_from must select neither generation")
	}
	inside := ValidityFilter{AsOf: superseded.ValidTo.Add(-time.Hour)}
	if !inside.Allows(superseded, created, now) {
		t.Error("as-of inside the window must select the record")
	}
	asOfEnd := ValidityFilter{AsOf: superseded.ValidTo}
	if asOfEnd.Allows(superseded, created, now) {
		t.Error("as-of at valid_to must exclude the record")
	}
}

func TestParseAsOf(t *testing.T) {
	at, err := ParseAsOf("2026-11-01T12:30:00Z")
	if err != nil || !at.Equal(time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)) {
		t.Fatalf("RFC3339 = %v, %v", at, err)
	}
	at, err = ParseAsOf("2026-11-01")
	if err != nil || !at.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("date = %v, %v", at, err)
	}
	if _, err := ParseAsOf("last tuesday"); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad value error = %v, want ErrValidation", err)
	}
	if at, err := ParseAsOf("  "); err != nil || !at.IsZero() {
		t.Fatalf("empty value = %v, %v; want zero time", at, err)
	}
}

func TestIsRecordReference(t *testing.T) {
	for _, value := range []string{
		"decision-2026-11-01-use-sqlite",
		"note-2026-11-01-first-note-01jc9w5m6r8s9t0v1w2x3y4z5a",
		"handoff-2026-11-01-120000",
		"attempt-2026-11-01-cache-01jc9w5m6r8s9t0v1w2x3y4z5a",
	} {
		if !IsRecordReference(value) {
			t.Errorf("%q rejected", value)
		}
	}
	for _, value := range []string{
		"", "state", "README.md", "notes-2026-11-01-x", "decision-../x",
		"decision-2026-11-01-../x", " decision-2026-11-01-x", "decision-2026-11-01-a/b",
	} {
		if IsRecordReference(value) {
			t.Errorf("%q accepted", value)
		}
	}
}

func TestRecordValidateRejectsSelfSupersede(t *testing.T) {
	created := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	decision := Decision{
		ID: "decision-2026-11-01-self", ProjectID: "p", Title: "Self", CreatedAt: created, UpdatedAt: created,
		Validity: Validity{Supersedes: "decision-2026-11-01-self"},
	}
	if err := decision.Validate(); err == nil {
		t.Fatal("decision superseding itself accepted")
	}
	attempt := Attempt{
		ID: "attempt-2026-11-01-window", ProjectID: "p", Title: "Window", CreatedAt: created, UpdatedAt: created,
		Validity: Validity{ValidTo: created},
	}
	if err := attempt.Validate(); err == nil {
		t.Fatal("attempt with valid_to equal to valid_from accepted")
	}
}
