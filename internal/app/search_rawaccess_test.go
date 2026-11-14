package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// rawSearchGrant returns a grant store that authorizes connection "tracker"
// only for projectID (empty projectID means no grants at all).
func rawSearchGrant(projectID string) *fakeContextGrants {
	grants := domain.ContextGrants{SchemaVersion: domain.ContextAllowSchemaVersion}
	if projectID != "" {
		grants.Grants = []domain.ContextGrant{{Project: projectID, Connection: "tracker"}}
	}
	return &fakeContextGrants{grants: grants}
}

// TestRawSearchHonorsGrants is the WB-0.9 matrix: grant/no-grant x
// include_dumps/kinds:item x project match/mismatch. No raw path may bypass
// context-allow.json, and a denied request returns no hits plus a hint.
func TestRawSearchHonorsGrants(t *testing.T) {
	ctx := context.Background()
	requestStyles := []struct {
		name string
		opts app.SearchOptions
	}{
		{"include_dumps", app.SearchOptions{IncludeDumps: true}},
		{"kinds_item", app.SearchOptions{Kinds: []string{app.KindItem}}},
	}
	cases := []struct {
		name     string
		grant    string
		project  string
		wantHits int
		wantHint bool
	}{
		{"granted include_dumps", "p", "p", 1, false},
		{"granted kinds_item", "p", "p", 1, false},
		{"ungranted include_dumps", "", "p", 0, true},
		{"ungranted kinds_item", "", "p", 0, true},
		{"other-project grant include_dumps", "other", "p", 0, true},
		{"other-project grant kinds_item", "other", "p", 0, true},
		{"grant but other project include_dumps", "p", "other", 0, false},
		{"grant but other project kinds_item", "p", "other", 0, false},
	}
	for _, tc := range cases {
		for _, style := range requestStyles {
			t.Run(tc.name+"/"+style.name, func(t *testing.T) {
				// Arrange: a fresh fixture so grants never leak between cases.
				f := newRawContextFixtureWithGrants(t, rawSearchGrant(tc.grant))
				f.addItem(t, f.item("tracker", "ISSUE-1", "Wombat outage", "wombat body"))
				opts := style.opts
				opts.Text = "wombat"
				opts.ProjectID = tc.project

				// Act.
				hits, notice, err := f.app.SearchWithNotice(ctx, opts)

				// Assert.
				if err != nil {
					t.Fatalf("SearchWithNotice: %v", err)
				}
				if len(hits) != tc.wantHits {
					t.Fatalf("hits = %+v, want %d", hits, tc.wantHits)
				}
				if tc.wantHint && notice == "" {
					t.Fatalf("no actionable hint for a denied raw search")
				}
				if !tc.wantHint && notice != "" {
					t.Fatalf("unexpected notice: %q", notice)
				}
				if notice != "" && !strings.Contains(notice, "context-allow.json") {
					t.Fatalf("hint is not actionable: %q", notice)
				}
			})
		}
	}
}

// TestCuratedSearchNeedsNoGrant keeps the policy narrow: notes and decisions
// are never gated, even with no grant file at all.
func TestCuratedSearchNeedsNoGrant(t *testing.T) {
	f := newRawContextFixtureWithGrants(t, rawSearchGrant(""))
	f.note(t, "wombat note")

	hits, notice, err := f.app.SearchWithNotice(context.Background(), app.SearchOptions{Text: "wombat", ProjectID: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Kind != app.ContextKindNote || notice != "" {
		t.Fatalf("hits = %+v notice = %q", hits, notice)
	}
}

// TestRawSearchKindRestrictedGrant proves a kind filter is resolved against
// the stored item, so a grant for issues never returns a message item.
func TestRawSearchKindRestrictedGrant(t *testing.T) {
	ctx := context.Background()
	grants := rawSearchGrant("p")
	grants.grants.Grants[0].Kinds = []domain.ItemKind{domain.ItemCommit}
	f := newRawContextFixtureWithGrants(t, grants)
	f.addItem(t, f.item("tracker", "ISSUE-1", "Wombat outage", "wombat body"))

	hits, notice, err := f.app.SearchWithNotice(ctx, app.SearchOptions{
		Text: "wombat", ProjectID: "p", Kinds: []string{app.KindItem},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || notice == "" {
		t.Fatalf("kind filter bypassed: hits = %+v notice = %q", hits, notice)
	}
}

// TestRawSearchHonorsGrantAge proves the grant's age window also gates search,
// not only excerpts: an expired item is not returned by a raw search.
func TestRawSearchHonorsGrantAge(t *testing.T) {
	grants := rawSearchGrant("p")
	grants.grants.Grants[0].MaxAgeDays = 1
	f := newRawContextFixtureWithGrants(t, grants)
	item := f.item("tracker", "ISSUE-1", "Wombat outage", "wombat body")
	item.UpdatedAt = f.clock.Add(-48 * time.Hour)
	item.ObservedAt = item.UpdatedAt
	f.addItem(t, item)

	hits, notice, err := f.app.SearchWithNotice(context.Background(), app.SearchOptions{
		Text: "wombat", ProjectID: "p", Kinds: []string{app.KindItem},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || notice == "" {
		t.Fatalf("age filter bypassed: hits = %+v notice = %q", hits, notice)
	}
}

// TestRawSearchLegacyDumpsAreDefaultDeny documents the legacy path: a dump
// file has no connection identity, so no connection grant can authorize it.
func TestRawSearchLegacyDumpsAreDefaultDeny(t *testing.T) {
	f := newRawContextFixture(t)
	if err := f.index.Upsert(context.Background(), ports.Document{
		ID: "p/dump:mail/1.md", ProjectID: "p", Kind: app.KindDump,
		Title: "wombat mail", Body: "wombat body",
	}); err != nil {
		t.Fatal(err)
	}

	hits, notice, err := f.app.SearchWithNotice(context.Background(), app.SearchOptions{
		Text: "wombat", ProjectID: "p", IncludeDumps: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || notice == "" {
		t.Fatalf("legacy dump leaked: hits = %+v notice = %q", hits, notice)
	}
}

// TestServerModeRawSearchIsUnavailable covers the server client: no grant
// store exists, so raw hits are dropped with an explanatory notice while
// curated hits survive.
func TestServerModeRawSearchIsUnavailable(t *testing.T) {
	searcher := &fakeSearcher{hits: []ports.Hit{
		{ID: "note-1", ProjectID: "demo", Kind: "note", Title: "Curated"},
		{ID: "item:tracker/" + strings.Repeat("a", 40), ProjectID: "demo", Kind: app.KindItem, Title: "Raw"},
	}}
	uc := serverModeApp(searcher)

	hits, notice, err := uc.SearchWithNotice(context.Background(), app.SearchOptions{
		Text: "release", ProjectID: "demo", Kinds: []string{app.KindItem, "note"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "note-1" {
		t.Fatalf("hits = %+v", hits)
	}
	if !strings.Contains(notice, "server mode") {
		t.Fatalf("notice = %q", notice)
	}
}
