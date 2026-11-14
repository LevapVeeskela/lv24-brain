package domain

import (
	"strings"
	"testing"
	"time"
)

func mustTestULID(t *testing.T, at time.Time) string {
	t.Helper()
	ulid, err := NewULID(at)
	if err != nil {
		t.Fatalf("NewULID: %v", err)
	}
	return ulid
}

func TestNewULIDIsUniqueAndWellFormed(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		ulid := mustTestULID(t, at)
		if len(ulid) != RecordIDULIDLength {
			t.Fatalf("ulid %q has length %d, want %d", ulid, len(ulid), RecordIDULIDLength)
		}
		if ulid != strings.ToLower(ulid) {
			t.Fatalf("ulid %q is not lower case", ulid)
		}
		if !IsULID(ulid) {
			t.Fatalf("ulid %q is not a valid Crockford base32 id", ulid)
		}
		if _, duplicate := seen[ulid]; duplicate {
			t.Fatalf("duplicate ulid %q after %d generations", ulid, i)
		}
		seen[ulid] = struct{}{}
	}
}

func TestNewULIDSortsByTime(t *testing.T) {
	early := mustTestULID(t, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	late := mustTestULID(t, time.Date(2026, 11, 15, 12, 0, 1, 0, time.UTC))
	if early >= late {
		t.Fatalf("ulid order %q >= %q; the timestamp prefix must sort first", early, late)
	}
}

func TestIsULIDRejectsInvalidValues(t *testing.T) {
	valid := mustTestULID(t, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	for name, value := range map[string]string{
		"empty":      "",
		"short":      valid[:25],
		"long":       valid + "0",
		"upper":      strings.ToUpper(valid),
		"excluded":   "i" + valid[1:],
		"whitespace": valid[:25] + " ",
	} {
		if IsULID(value) {
			t.Fatalf("%s: IsULID(%q) = true, want false", name, value)
		}
	}
}

func TestNewRecordIDShape(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	ulid := mustTestULID(t, at)
	id := NewRecordID("decision", at, "pick-storage", ulid)
	want := "decision-2026-11-15-pick-storage-" + ulid
	if id != want {
		t.Fatalf("NewRecordID = %q, want %q", id, want)
	}
	if !IsRecordID("decision", id) {
		t.Fatalf("IsRecordID(%q) = false, want true", id)
	}
	if IsRecordID("attempt", id) {
		t.Fatalf("IsRecordID read a decision id as an attempt")
	}
	handoff := NewRecordID("handoff", at, at.Format("150405"), ulid)
	if !IsRecordID("handoff", handoff) {
		t.Fatalf("handoff id %q is not recognized", handoff)
	}
}

func TestIsRecordIDRejectsLegacyAndUnsafeIDs(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	ulid := mustTestULID(t, at)
	legacy := []string{
		"decision-lv24-brain-20261114-150405",
		"decision-2026-11-15",
		"decision-2026-11-15-pick-storage",
		"dec-" + ulid,
		"decision-2026-13-40-pick-storage-" + ulid,
		"decision-2026-11-15-" + ulid,
		"decision-2026-11-15-pick/storage-" + ulid,
		"decision-2026-11-15-pick storage-" + ulid,
		"decision-2026-11-15-Pick-" + ulid,
		"decision-2026-11-15-pick-storage-" + strings.ToUpper(ulid),
		"note-2026-11-15-pick-storage-" + ulid,
	}
	for _, id := range legacy {
		if IsRecordID("decision", id) {
			t.Fatalf("IsRecordID(%q) = true, want false", id)
		}
	}
}

func TestSlugifyIsPathSafe(t *testing.T) {
	cases := map[string]string{
		"Pick storage":         "pick-storage",
		"Пересборка индекса":   "peresborka-indeksa",
		"  --Weird__title?!  ": "weird-title",
		"":                     "item",
		"日本語":                  "item",
		"a/b\\c":               "a-b-c",
	}
	for input, want := range cases {
		if got := Slugify(input); got != want {
			t.Fatalf("Slugify(%q) = %q, want %q", input, got, want)
		}
	}
	long := strings.Repeat("abcdefghij", 20)
	slug := Slugify(long)
	if len(slug) > MaxSlugBytes {
		t.Fatalf("Slugify produced %d bytes, want <= %d", len(slug), MaxSlugBytes)
	}
}
