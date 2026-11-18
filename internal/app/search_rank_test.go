package app_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// A strict AND pass that finds a single record is widened with OR, and the
// relaxed page is merged with the strict one instead of replacing it: the
// record the strict pass found keeps its place on top.
func TestSearchRelaxedFallbackMergesStrictAndOrHits(t *testing.T) {
	fixture := newSearchFixture(t)
	fixture.note(t, "note-2026-11-01-both", "Quartz ledger reconciliation", "quartz ledger", searchClock.Add(-time.Hour))
	for index := 0; index < 4; index++ {
		fixture.note(t, fmt.Sprintf("note-2026-11-01-only-%d", index),
			"Quartz note", fmt.Sprintf("quartz entry %d", index), searchClock.Add(-time.Duration(index+1)*time.Hour))
	}
	fixture.reindex(t)

	report, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{
		Text: "quartz ledger", ProjectID: "demo",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if !report.Relaxed {
		t.Fatal("report is not relaxed, want the OR fallback")
	}
	if len(report.Hits) < 3 {
		t.Fatalf("hits = %d, want at least 3 from the merged page", len(report.Hits))
	}
	if report.Hits[0].ID != "note-2026-11-01-both" {
		t.Fatalf("first hit = %s, want the strict hit to keep the top", report.Hits[0].ID)
	}
	seen := make(map[string]struct{}, len(report.Hits))
	for _, hit := range report.Hits {
		if _, ok := seen[hit.ID]; ok {
			t.Fatalf("hit %s appears twice after the merge", hit.ID)
		}
		seen[hit.ID] = struct{}{}
	}
	for index := 1; index < len(report.Hits); index++ {
		if report.Hits[index-1].Score < report.Hits[index].Score {
			t.Fatalf("merged page is not ordered at %d: %.4f < %.4f",
				index, report.Hits[index-1].Score, report.Hits[index].Score)
		}
	}
}

// An explicit AND stays strict: the fallback is for the default mode, not a
// silent widening of a caller that asked for every term.
func TestSearchExplicitAllStaysStrict(t *testing.T) {
	fixture := newSearchFixture(t)
	fixture.note(t, "note-2026-11-01-both", "Quartz ledger reconciliation", "quartz ledger", searchClock.Add(-time.Hour))
	fixture.note(t, "note-2026-11-01-quartz", "Quartz note", "quartz entry", searchClock.Add(-2*time.Hour))
	fixture.reindex(t)

	report, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{
		Text: "quartz ledger", ProjectID: "demo", MatchMode: "all",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if report.Relaxed {
		t.Fatal("explicit all relaxed, want strict AND")
	}
	if len(report.Hits) != 1 || report.Hits[0].ID != "note-2026-11-01-both" {
		t.Fatalf("hits = %+v, want only the strict match", report.Hits)
	}
}

// A strict page at or above the fallback threshold is not widened: enough
// records already answer the query.
func TestSearchStrictPageAboveThresholdDoesNotRelax(t *testing.T) {
	fixture := newSearchFixture(t)
	for index := 0; index < app.SearchFallbackThreshold+1; index++ {
		fixture.note(t, fmt.Sprintf("note-2026-11-01-both-%d", index), "Quartz ledger",
			fmt.Sprintf("quartz ledger entry %d", index), searchClock.Add(-time.Duration(index+1)*time.Hour))
	}
	fixture.note(t, "note-2026-11-01-quartz", "Quartz note", "quartz entry", searchClock.Add(-24*time.Hour))
	fixture.reindex(t)

	report, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{
		Text: "quartz ledger", ProjectID: "demo",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if report.Relaxed {
		t.Fatal("report relaxed above the fallback threshold")
	}
	if len(report.Hits) != app.SearchFallbackThreshold+1 {
		t.Fatalf("hits = %d, want the strict page only", len(report.Hits))
	}
	for _, hit := range report.Hits {
		if hit.ID == "note-2026-11-01-quartz" {
			t.Fatal("the OR-only record leaked into a strict page")
		}
	}
}

// The explained contributions of every hit add up to its score and their
// shares add up to 1.
func TestSearchExplainSharesSumToOne(t *testing.T) {
	fixture := newSearchFixture(t)
	fixture.decision(t, "decision-2026-11-01-cache", "Cache storage limit",
		"storage limit for the cache", searchClock.Add(-30*time.Hour), domain.DecisionAccepted)
	fixture.note(t, "note-2026-11-01-storage", "Storage notes", "storage notes storage", searchClock.Add(-time.Minute))
	fixture.note(t, "note-2026-11-01-old", "Old storage", "storage", searchClock.Add(-120*24*time.Hour))
	fixture.reindex(t)

	report, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{
		Text: "storage", ProjectID: "demo", Explain: true,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(report.Hits) < 2 {
		t.Fatalf("hits = %d, want a ranked page", len(report.Hits))
	}
	for _, hit := range report.Hits {
		if hit.Explain == nil {
			t.Fatalf("%s has no explanation", hit.ID)
		}
		explain := hit.Explain
		sum := explain.BM25 + explain.Recency + explain.Importance + explain.Pin + explain.Link
		if math.Abs(sum-hit.Score) > 1e-9 {
			t.Fatalf("%s: contributions = %.9f, score = %.9f", hit.ID, sum, hit.Score)
		}
		if math.Abs(sum/hit.Score-1) > 1e-9 {
			t.Fatalf("%s: shares = %.9f, want 1", hit.ID, sum/hit.Score)
		}
	}
}

// noteWithProvenance appends one note that carries an explicit provenance.
func noteWithProvenance(t *testing.T, fixture *searchFixture, id, title, body string, created time.Time, provenance domain.Provenance) {
	t.Helper()
	if _, err := fixture.store.AppendNote(context.Background(), domain.Note{
		ID: id, ProjectID: "demo", Title: title, Body: body, CreatedAt: created, UpdatedAt: created,
		Provenance: provenance,
	}); err != nil {
		t.Fatalf("note %s: %v", id, err)
	}
}

// Provenance travels from the frontmatter through the reindex into the
// ranking: two notes with the same subject and timestamp, one verified and one
// inferred, rank in that order because the trust term separates them. Their
// text differs by one word so content-hash duplicate collapsing keeps both.
func TestSearchProjectsProvenanceTrust(t *testing.T) {
	fixture := newSearchFixture(t)
	at := searchClock.Add(-time.Hour)
	noteWithProvenance(t, fixture, "note-2026-11-01-verified", "Provenance probe verified",
		"provenance ranking probe verified", at,
		domain.Provenance{Origin: domain.OriginHuman, Trust: domain.TrustVerified})
	noteWithProvenance(t, fixture, "note-2026-11-01-inferred", "Provenance probe inferred",
		"provenance ranking probe inferred", at,
		domain.Provenance{Origin: domain.OriginAgent, Reviewed: true, Trust: domain.TrustInferred})
	fixture.reindex(t)

	report, err := fixture.app.SearchWithReport(context.Background(), app.SearchOptions{
		Text: "provenance", ProjectID: "demo", Explain: true,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(report.Hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(report.Hits))
	}
	verified, inferred := report.Hits[0], report.Hits[1]
	if verified.ID != "note-2026-11-01-verified" || inferred.ID != "note-2026-11-01-inferred" {
		t.Fatalf("rank order = %s, %s; want the verified note first", verified.ID, inferred.ID)
	}
	if verified.Score <= inferred.Score {
		t.Fatalf("verified score %.9f <= inferred score %.9f, want the trust term to decide", verified.Score, inferred.Score)
	}
	if verified.Explain == nil || inferred.Explain == nil {
		t.Fatal("--explain returned no explanation")
	}
	if verified.Explain.Importance <= inferred.Explain.Importance {
		t.Fatalf("verified authority %.6f <= inferred authority %.6f", verified.Explain.Importance, inferred.Explain.Importance)
	}
	for _, hit := range report.Hits {
		sum := hit.Explain.BM25 + hit.Explain.Recency + hit.Explain.Importance + hit.Explain.Pin + hit.Explain.Link
		if math.Abs(sum-hit.Score) > 1e-9 {
			t.Fatalf("%s: contributions = %.9f, score = %.9f", hit.ID, sum, hit.Score)
		}
	}
}
