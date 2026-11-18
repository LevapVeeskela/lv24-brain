package app_test

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
)

// searchClock is the fixture instant, so recency never depends on the test
// machine's clock.
var searchClock = time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)

// searchFixture is a local brain with a real Markdown store and SQLite index
// whose records carry explicit creation times.
type searchFixture struct {
	app   *app.App
	store *fsstore.Store
}

// newSearchFixture opens the store and index under one temporary data
// directory and registers the demo project.
func newSearchFixture(t *testing.T) *searchFixture {
	t.Helper()
	dataDir := t.TempDir()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	if err := store.PutProject(context.Background(), domain.Project{
		ID: "demo", Name: "Demo", Kind: domain.ProjectKindOther, CreatedAt: searchClock.Add(-90 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	return &searchFixture{
		app: app.New(cfg, app.Dependencies{
			Store: store, Index: index,
			Now: func() time.Time { return searchClock },
		}),
		store: store,
	}
}

// note appends one note with an explicit id and creation time.
func (f *searchFixture) note(t *testing.T, id, title, body string, created time.Time) {
	t.Helper()
	if _, err := f.store.AppendNote(context.Background(), domain.Note{
		ID: id, ProjectID: "demo", Title: title, Body: body, CreatedAt: created, UpdatedAt: created,
	}); err != nil {
		t.Fatalf("note %s: %v", id, err)
	}
}

// decision appends one decision with an explicit id and creation time.
func (f *searchFixture) decision(t *testing.T, id, title, contextText string, created time.Time, status domain.DecisionStatus) {
	t.Helper()
	if _, err := f.store.AppendDecision(context.Background(), domain.Decision{
		ID: id, ProjectID: "demo", Title: title, Context: contextText, Status: status,
		CreatedAt: created, UpdatedAt: created,
	}); err != nil {
		t.Fatalf("decision %s: %v", id, err)
	}
}

// reindex refreshes the search projection after records were appended.
func (f *searchFixture) reindex(t *testing.T) {
	t.Helper()
	if _, err := f.app.Reindex(context.Background(), app.IndexOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A search never returns more than the preview budget, and --full adds the
// body instead of growing the preview.
func TestSearchPreviewIsCappedAndFullAddsBody(t *testing.T) {
	fixture := newSearchFixture(t)
	longBody := strings.TrimSpace(strings.Repeat("needle-identifier ", 30))
	fixture.note(t, "note-2026-11-01-long", "Long needle note", longBody, searchClock.Add(-24*time.Hour))
	fixture.reindex(t)

	report, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{Text: "needle", ProjectID: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(report.Hits))
	}
	if got := utf8.RuneCountInString(report.Hits[0].Snippet); got > app.PreviewRunes {
		t.Fatalf("preview length = %d, want <= %d: %q", got, app.PreviewRunes, report.Hits[0].Snippet)
	}
	if report.Hits[0].Body != "" {
		t.Fatalf("preview search returned a body: %q", report.Hits[0].Body)
	}

	full, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{Text: "needle", ProjectID: "demo", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Hits) != 1 || !strings.Contains(full.Hits[0].Body, "needle-identifier") {
		t.Fatalf("full hit body = %q", full.Hits[0].Body)
	}
}

// The strict pass reports its fallback instead of silently widening the query.
func TestSearchRelaxedIsReported(t *testing.T) {
	fixture := newSearchFixture(t)
	fixture.note(t, "note-2026-11-01-both", "Alpha beta", "alpha beta", searchClock.Add(-time.Hour))
	fixture.note(t, "note-2026-11-01-alpha", "Alpha only", "alpha", searchClock.Add(-time.Hour))
	fixture.reindex(t)

	cases := []struct {
		name        string
		opts        app.SearchOptions
		wantHits    int
		wantRelaxed bool
	}{
		{"default keeps the fallback", app.SearchOptions{Text: "alpha gamma"}, 2, true},
		{"explicit all is strict", app.SearchOptions{Text: "alpha gamma", MatchMode: "all"}, 0, false},
		{"all with relaxed falls back", app.SearchOptions{Text: "alpha gamma", MatchMode: "all", Relaxed: true}, 2, true},
		{"any starts wide", app.SearchOptions{Text: "alpha gamma", MatchMode: "any"}, 2, false},
		// A strict pass that finds fewer than SearchFallbackThreshold hits is
		// widened too: one strict hit is not enough to call the query answered.
		{"strict hit below the threshold relaxes", app.SearchOptions{Text: "alpha beta"}, 2, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.ProjectID = "demo"
			report, err := fixture.app.SearchWithReport(context.Background(), tc.opts)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if len(report.Hits) != tc.wantHits || report.Relaxed != tc.wantRelaxed {
				t.Fatalf("hits = %d relaxed = %v, want %d/%v", len(report.Hits), report.Relaxed, tc.wantHits, tc.wantRelaxed)
			}
		})
	}
}

// Explain fills the contribution block; a threshold filters on the same score.
func TestSearchExplainAndMinScore(t *testing.T) {
	fixture := newSearchFixture(t)
	fixture.note(t, "note-2026-11-01-storage", "Storage limit",
		"storage limit for the cache", searchClock.Add(-time.Hour))
	fixture.reindex(t)

	report, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{Text: "storage", ProjectID: "demo", Explain: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Hits) != 1 || report.Hits[0].Explain == nil {
		t.Fatalf("hits = %+v, want one explained hit", report.Hits)
	}
	if len(report.Hits[0].MatchReasons) == 0 {
		t.Fatal("hit has no match reasons")
	}
	hit := report.Hits[0]
	score := hit.Score
	if score <= 0 || score >= 1 {
		t.Fatalf("score = %v, want (0, 1)", score)
	}
	explain := hit.Explain
	sum := explain.BM25 + explain.Recency + explain.Importance + explain.Pin + explain.Link
	if math.Abs(sum-score) > 1e-9 {
		t.Fatalf("contributions = %.9f, score = %.9f", sum, score)
	}
	if math.Abs(sum/score-1) > 1e-9 {
		t.Fatalf("share of contributions = %.9f, want 1", sum/score)
	}
	kept, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{Text: "storage", ProjectID: "demo", MinScore: score})
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.Hits) != 1 {
		t.Fatalf("min-score hits = %d, want the hit at its own score", len(kept.Hits))
	}
	empty, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{Text: "storage", ProjectID: "demo", MinScore: score + 0.01})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Hits) != 0 {
		t.Fatalf("min-score hits = %d, want 0", len(empty.Hits))
	}
	if _, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{Text: "storage", MinScore: 2}); err == nil {
		t.Fatal("min-score 2 was accepted")
	}
}

// An unknown match mode is a validation error, not a silent default.
func TestSearchRejectsUnknownMatchMode(t *testing.T) {
	fixture := newSearchFixture(t)
	_, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{Text: "x", MatchMode: "sometimes"})
	var invalid *domain.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want a validation error", err)
	}
}

// RecordBody truncates to the token budget with a marker and reports it.
func TestRecordBodyTokenBudget(t *testing.T) {
	fixture := newSearchFixture(t)
	id := "decision-2026-11-01-long"
	fixture.decision(t, id, "Long decision",
		strings.TrimSpace(strings.Repeat("the cache keeps entries ", 60)), searchClock.Add(-time.Hour), domain.DecisionAccepted)

	body, err := fixture.app.RecordBody(context.Background(), "demo", id, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !body.Truncated {
		t.Fatal("long record was not truncated")
	}
	if !strings.Contains(body.Body, "truncated") {
		t.Fatalf("body has no truncation marker: %q", body.Body)
	}
	if body.Tokens > 20 || body.MaxTokens != 20 {
		t.Fatalf("tokens = %d max = %d, want <= 20", body.Tokens, body.MaxTokens)
	}
	whole, err := fixture.app.RecordBody(context.Background(), "demo", id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if whole.Truncated || !strings.Contains(whole.Body, "Context:") {
		t.Fatalf("unlimited body truncated = %v, body = %q", whole.Truncated, whole.Body)
	}
}

// Timeline returns the neighbours of one record, oldest first.
func TestTimelineWindowAroundAnchor(t *testing.T) {
	fixture := newSearchFixture(t)
	ids := []string{
		"note-2026-11-01-a", "note-2026-11-01-b", "note-2026-11-01-c",
		"note-2026-11-01-d", "note-2026-11-01-e",
	}
	for index, id := range ids {
		fixture.note(t, id, "Note "+string(rune('A'+index)), "timeline sequence",
			searchClock.Add(time.Duration(index)*time.Hour))
	}
	timeline, err := fixture.app.Timeline(context.Background(), "demo", ids[2], 1)
	if err != nil {
		t.Fatal(err)
	}
	if timeline.Total != 5 || timeline.Before != 1 || timeline.After != 1 || timeline.AnchorIndex != 1 {
		t.Fatalf("timeline = total %d before %d after %d index %d, want 5/1/1/1",
			timeline.Total, timeline.Before, timeline.After, timeline.AnchorIndex)
	}
	got := make([]string, 0, len(timeline.Records))
	for _, record := range timeline.Records {
		got = append(got, record.ID)
	}
	if want := strings.Join(ids[1:4], ","); strings.Join(got, ",") != want {
		t.Fatalf("window = %v, want %s", got, want)
	}
	if _, err := fixture.app.Timeline(context.Background(), "demo", "missing-id", 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing anchor error = %v, want ErrNotFound", err)
	}
}

// The text budget package is the shared source of the preview and truncation
// rules the app relies on.
func TestPreviewAndTruncateShareTheBudget(t *testing.T) {
	long := strings.Repeat("a", 500)
	if got := utf8.RuneCountInString(textbudget.Preview(long, app.PreviewRunes)); got > app.PreviewRunes {
		t.Fatalf("preview length = %d", got)
	}
	cut, truncated := textbudget.Truncate(strings.Repeat("word ", 200), 10)
	if !truncated || textbudget.Tokens(cut) > 10 {
		t.Fatalf("truncate = %q (truncated %v, tokens %d)", cut, truncated, textbudget.Tokens(cut))
	}
	if text, wasCut := textbudget.Truncate("short", 10); wasCut || text != "short" {
		t.Fatalf("short text = %q, truncated %v", text, wasCut)
	}
}
