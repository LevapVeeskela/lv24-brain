package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

// supportFixture is a small corpus with three distinct notes: one answers a
// two-term query, one only shares its common term, and one is about another
// topic.
func supportFixture(t *testing.T) *searchFixture {
	t.Helper()
	fixture := newSearchFixture(t)
	fixture.note(t, "note-2026-11-01-full", "Quartz ledger reconciliation",
		"quartz ledger", searchClock.Add(-time.Hour))
	fixture.note(t, "note-2026-11-01-quartz", "Quartz note",
		"quartz entry", searchClock.Add(-2*time.Hour))
	fixture.note(t, "note-2026-11-01-harness", "Harness note",
		"harness rig", searchClock.Add(-3*time.Hour))
	fixture.reindex(t)
	return fixture
}

// A query the corpus answers is supported; a query with an absent rare term is
// related, not supported, and an absent-topic query is none with a reason and
// an empty hit list instead of relaxed noise.
func TestSearchSupportTiers(t *testing.T) {
	fixture := supportFixture(t)
	cases := []struct {
		name     string
		query    string
		wantTier string
		wantHits int
	}{
		{"full match is supported", "quartz ledger", app.SupportSupported, 2},
		{"partial match is related", "quartz ledger harness", app.SupportRelated, 3},
		{"absent topic is none", "quartz zephyrion", app.SupportNone, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{
				Text: tc.query, ProjectID: "demo",
			})
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if report.Support.Tier != tc.wantTier {
				t.Fatalf("tier = %q, want %q (reason %q)", report.Support.Tier, tc.wantTier, report.Support.Reason)
			}
			if len(report.Hits) != tc.wantHits {
				t.Fatalf("hits = %d, want %d: %+v", len(report.Hits), tc.wantHits, report.Hits)
			}
			for _, hit := range report.Hits {
				if hit.Tier == "" {
					t.Fatalf("hit %s has no support tier", hit.ID)
				}
			}
			switch tc.wantTier {
			case app.SupportNone:
				if report.Support.Reason == "" {
					t.Fatal("none without an explicit reason")
				}
				if len(report.Excluded) == 0 {
					t.Fatal("none dropped the relaxed hits without listing them")
				}
				for _, excluded := range report.Excluded {
					if excluded.Reason != app.ExcludedLowerRank {
						t.Fatalf("excluded %s reason = %q, want %q", excluded.ID, excluded.Reason, app.ExcludedLowerRank)
					}
				}
			case app.SupportRelated:
				for _, hit := range report.Hits {
					if hit.Tier == app.SupportSupported {
						t.Fatalf("partial hit %s is reported as supported", hit.ID)
					}
				}
			}
		})
	}
}

// IDX-4: a stub stays in the file and the index, but search hides it until
// IncludeStubs asks for it.
func TestSearchHidesStubsUnlessRequested(t *testing.T) {
	fixture := newSearchFixture(t)
	fixture.note(t, "note-2026-11-01-real", "Release checklist",
		"nothing new is only a stub phrase; release checklist here", searchClock.Add(-time.Hour))
	fixture.note(t, "note-2026-11-01-stub", "Session wrap",
		"nothing new", searchClock.Add(-time.Hour))
	fixture.reindex(t)

	report, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{
		Text: "nothing", ProjectID: "demo",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(report.Hits) != 1 || report.Hits[0].ID != "note-2026-11-01-real" {
		t.Fatalf("default hits = %+v, want only the real note", report.Hits)
	}
	with, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{
		Text: "nothing", ProjectID: "demo", IncludeStubs: true,
	})
	if err != nil {
		t.Fatalf("search --include-stubs: %v", err)
	}
	if len(with.Hits) != 2 {
		t.Fatalf("IncludeStubs hits = %+v, want the stub as well", with.Hits)
	}
}

// The same record written twice collapses to the oldest row; the copy is
// reported once with the duplicate reason instead of occupying a second slot.
func TestSearchCollapsesDuplicateContent(t *testing.T) {
	fixture := newSearchFixture(t)
	fixture.note(t, "note-2026-11-01-older", "Duplicate probe", "same body text",
		searchClock.Add(-48*time.Hour))
	fixture.note(t, "note-2026-11-01-newer", "Duplicate probe", "same body text",
		searchClock.Add(-time.Hour))
	fixture.reindex(t)

	report, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{
		Text: "duplicate probe", ProjectID: "demo",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(report.Hits) != 1 {
		t.Fatalf("hits = %+v, want the duplicate collapsed to one row", report.Hits)
	}
	if report.Hits[0].ID != "note-2026-11-01-older" {
		t.Fatalf("kept %s, want the oldest record", report.Hits[0].ID)
	}
	if len(report.Excluded) != 1 || report.Excluded[0].Reason != app.ExcludedDuplicate {
		t.Fatalf("excluded = %+v, want one duplicate entry", report.Excluded)
	}
	if report.Excluded[0].ID != "note-2026-11-01-newer" {
		t.Fatalf("excluded %s, want the newer copy", report.Excluded[0].ID)
	}
}
