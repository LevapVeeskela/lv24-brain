package sqliteindex

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// rankClock is the fixed instant every ranking test measures recency from.
var rankClock = time.Date(2026, time.November, 15, 12, 0, 0, 0, time.UTC)

// newRankIndex opens an in-memory index filled with docs.
func newRankIndex(t *testing.T, docs ...ports.Document) *Index {
	t.Helper()
	index, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	for _, doc := range docs {
		if err := index.Upsert(context.Background(), doc); err != nil {
			t.Fatalf("upsert %s: %v", doc.ID, err)
		}
	}
	return index
}

// rankDocs returns the ids of the hits in rank order.
func rankIDs(hits []ports.Hit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

func searchRanked(t *testing.T, index *Index, query ports.Query) []ports.Hit {
	t.Helper()
	if query.Now.IsZero() {
		query.Now = rankClock
	}
	hits, err := index.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("search %q: %v", query.Text, err)
	}
	return hits
}

// A 30-day half-life means a two-half-life-old record keeps a quarter of the
// recency weight, and it loses to a fresh record with the same text.
func TestRankingRecencyHalfLife30Days(t *testing.T) {
	index := newRankIndex(t,
		ports.Document{ID: "p/decision-old", ProjectID: "p", Kind: "decision", Title: "Retention probe",
			Body: "retention policy", UpdatedAt: rankClock.Add(-60 * 24 * time.Hour)},
		ports.Document{ID: "p/decision-fresh", ProjectID: "p", Kind: "decision", Title: "Retention probe",
			Body: "retention policy", UpdatedAt: rankClock.Add(-24 * time.Hour)},
	)
	hits := searchRanked(t, index, ports.Query{Text: "retention", Explain: true})
	if got := rankIDs(hits); len(got) != 2 || got[0] != "decision-fresh" || got[1] != "decision-old" {
		t.Fatalf("rank order = %v, want the fresh record first", got)
	}
	// 60 days is two half-lives: the recency contribution is
	// 0.20 * 0.25 * (1 - score), and the same holds for the fresh record at
	// one day (0.20 * 0.5^(1/30)).
	if got, want := hits[1].Explain.Recency, WeightRecency*0.25*(1-hits[1].Score); math.Abs(got-want) > 1e-9 {
		t.Fatalf("old record recency contribution = %.6f, want %.6f", got, want)
	}
	if got, want := hits[0].Explain.Recency, WeightRecency*math.Pow(0.5, 1.0/30.0)*(1-hits[0].Score); math.Abs(got-want) > 1e-9 {
		t.Fatalf("fresh record recency contribution = %.6f, want %.6f", got, want)
	}
	if hits[0].Explain.Recency <= hits[1].Explain.Recency {
		t.Fatalf("fresh recency %.6f <= old recency %.6f", hits[0].Explain.Recency, hits[1].Explain.Recency)
	}
	if !strings.Contains(strings.Join(hits[0].MatchReasons, ","), ports.ReasonRecency) {
		t.Fatalf("fresh reasons = %v, want recency", hits[0].MatchReasons)
	}
}

// Kind importance puts a decision above a note when everything else is equal.
func TestRankingImportanceOrdering(t *testing.T) {
	index := newRankIndex(t,
		ports.Document{ID: "p/note-one", ProjectID: "p", Kind: "note", Title: "Storage probe",
			Body: "storage policy", UpdatedAt: rankClock.Add(-time.Hour)},
		ports.Document{ID: "p/decision-one", ProjectID: "p", Kind: "decision", Title: "Storage probe",
			Body: "storage policy", UpdatedAt: rankClock.Add(-time.Hour)},
	)
	hits := searchRanked(t, index, ports.Query{Text: "storage", Explain: true})
	if got := rankIDs(hits); len(got) != 2 || got[0] != "decision-one" || got[1] != "note-one" {
		t.Fatalf("rank order = %v, want the decision first", got)
	}
	if got, want := hits[0].Explain.Importance, WeightImportance*1.0*(1-hits[0].Score); math.Abs(got-want) > 1e-9 {
		t.Fatalf("decision importance = %.6f, want %.6f", got, want)
	}
	if got, want := hits[1].Explain.Importance, WeightImportance*0.5*(1-hits[1].Score); math.Abs(got-want) > 1e-9 {
		t.Fatalf("note importance = %.6f, want %.6f", got, want)
	}
	if !strings.Contains(strings.Join(hits[0].MatchReasons, ","), ports.ReasonImportance) {
		t.Fatalf("decision reasons = %v, want importance", hits[0].MatchReasons)
	}
	if strings.Contains(strings.Join(hits[1].MatchReasons, ","), ports.ReasonImportance) {
		t.Fatalf("note reasons = %v, want no importance", hits[1].MatchReasons)
	}
}

// The FTS5 field weights make a title term worth three body terms.
func TestRankingTitleWeightBeatsBody(t *testing.T) {
	index := newRankIndex(t,
		ports.Document{ID: "p/note-title", ProjectID: "p", Kind: "note", Title: "Quasar rollout",
			Body: "unrelated filler about passwords", UpdatedAt: rankClock.Add(-time.Hour)},
		ports.Document{ID: "p/note-body", ProjectID: "p", Kind: "note", Title: "Unrelated rollout",
			Body: "quasar mentioned once", UpdatedAt: rankClock.Add(-time.Hour)},
	)
	hits := searchRanked(t, index, ports.Query{Text: "quasar", Explain: true})
	if got := rankIDs(hits); len(got) != 2 || got[0] != "note-title" {
		t.Fatalf("rank order = %v, want the title match first: %+v", got, hits)
	}
	if hits[0].Explain.BM25 <= hits[1].Explain.BM25 {
		t.Fatalf("title bm25 %.4f <= body bm25 %.4f", hits[0].Explain.BM25, hits[1].Explain.BM25)
	}
}

// MatchModeAll requires every term, MatchModeAny accepts one, and the empty
// mode keeps the historical fallback.
func TestMatchModeAllAnyAndLegacyFallback(t *testing.T) {
	index := newRankIndex(t,
		ports.Document{ID: "p/note-both", ProjectID: "p", Kind: "note", Title: "Alpha beta", Body: "alpha beta", UpdatedAt: rankClock},
		ports.Document{ID: "p/note-alpha", ProjectID: "p", Kind: "note", Title: "Alpha only", Body: "alpha", UpdatedAt: rankClock},
	)
	t.Run("all", func(t *testing.T) {
		hits := searchRanked(t, index, ports.Query{Text: "alpha beta", MatchMode: ports.MatchModeAll})
		if got := rankIDs(hits); len(got) != 1 || got[0] != "note-both" {
			t.Fatalf("all = %v, want only note-both", got)
		}
	})
	t.Run("any", func(t *testing.T) {
		hits := searchRanked(t, index, ports.Query{Text: "alpha beta", MatchMode: ports.MatchModeAny})
		if got := rankIDs(hits); len(got) != 2 {
			t.Fatalf("any = %v, want both notes", got)
		}
	})
	t.Run("legacy fallback", func(t *testing.T) {
		hits := searchRanked(t, index, ports.Query{Text: "alpha gamma"})
		if got := rankIDs(hits); len(got) != 2 {
			t.Fatalf("legacy fallback = %v, want both notes", got)
		}
	})
	t.Run("all without fallback", func(t *testing.T) {
		hits := searchRanked(t, index, ports.Query{Text: "alpha gamma", MatchMode: ports.MatchModeAll})
		if len(hits) != 0 {
			t.Fatalf("strict all = %v, want no hits", rankIDs(hits))
		}
	})
}

// MinScore drops hits below the composite threshold.
func TestMinScoreFiltersHits(t *testing.T) {
	index := newRankIndex(t,
		ports.Document{ID: "p/note-one", ProjectID: "p", Kind: "note", Title: "Threshold probe", Body: "threshold", UpdatedAt: rankClock},
	)
	all := searchRanked(t, index, ports.Query{Text: "threshold", Explain: true})
	if len(all) != 1 {
		t.Fatalf("unfiltered hits = %d, want 1", len(all))
	}
	score := all[0].Score
	if score <= 0 || score > 1 {
		t.Fatalf("score = %v, want it in (0, 1]", score)
	}
	if hits := searchRanked(t, index, ports.Query{Text: "threshold", MinScore: score}); len(hits) != 1 {
		t.Fatalf("hits at the exact score = %d, want 1", len(hits))
	}
	if hits := searchRanked(t, index, ports.Query{Text: "threshold", MinScore: math.Min(1, score+0.01)}); len(hits) != 0 {
		t.Fatalf("hits above the score = %d, want 0", len(hits))
	}
}

// Explain contributions sum to the score, and only firing signals are named.
func TestExplainShapeAndReasons(t *testing.T) {
	link := "See [[note-other]] for the rollout"
	index := newRankIndex(t,
		ports.Document{ID: "p/decision-locked", ProjectID: "p", Kind: "decision", Title: "Pin the release",
			Body: "release gate " + link, Status: "accepted", Confidence: 0.9, UpdatedAt: rankClock.Add(-time.Hour)},
		ports.Document{ID: "p/note-plain", ProjectID: "p", Kind: "note", Title: "Release notes",
			Body: "release notes", UpdatedAt: rankClock.Add(-time.Hour)},
	)
	hits := searchRanked(t, index, ports.Query{Text: "release", Explain: true})
	if len(hits) != 2 || hits[0].ID != "decision-locked" {
		t.Fatalf("hits = %v, want the pinned decision first", rankIDs(hits))
	}
	pinned := hits[0]
	if pinned.Explain == nil {
		t.Fatal("Explain is nil")
	}
	sum := pinned.Explain.BM25 + pinned.Explain.Recency + pinned.Explain.Importance + pinned.Explain.Pin + pinned.Explain.Link
	if math.Abs(sum-pinned.Score) > 1e-9 {
		t.Fatalf("contributions sum = %.6f, score = %.6f", sum, pinned.Score)
	}
	if got, want := pinned.Explain.Pin, WeightPin*1.0*(1-pinned.Score); math.Abs(got-want) > 1e-9 {
		t.Fatalf("pin contribution = %.6f, want %.6f", got, want)
	}
	reasons := strings.Join(pinned.MatchReasons, ",")
	for _, want := range []string{ports.ReasonBM25, ports.ReasonRecency, ports.ReasonImportance, ports.ReasonPin, ports.ReasonLink} {
		if !strings.Contains(reasons, want) {
			t.Fatalf("reasons = %q, want %s", reasons, want)
		}
	}
	plain := strings.Join(hits[1].MatchReasons, ",")
	for _, unwanted := range []string{ports.ReasonImportance, ports.ReasonPin, ports.ReasonLink} {
		if strings.Contains(plain, unwanted) {
			t.Fatalf("plain note reasons = %q, want no %s", plain, unwanted)
		}
	}
}

// Paging by limit returns the same total order in consecutive slices: the
// guarantee the CLI cursor relies on.
func TestRankingPaginationIsStable(t *testing.T) {
	docs := make([]ports.Document, 0, 7)
	for i := 0; i < 7; i++ {
		docs = append(docs, ports.Document{
			ID: fmt.Sprintf("p/note-%d", i), ProjectID: "p", Kind: "note",
			Title: "Paging probe", Body: "paging probe", UpdatedAt: rankClock.Add(-time.Duration(i) * time.Hour),
		})
	}
	index := newRankIndex(t, docs...)
	whole := rankIDs(searchRanked(t, index, ports.Query{Text: "paging", Limit: 20}))
	if len(whole) != 7 {
		t.Fatalf("whole window = %v", whole)
	}
	var paged []string
	for offset := 0; offset < 7; offset += 3 {
		hits := searchRanked(t, index, ports.Query{Text: "paging", Limit: offset + 3})
		if len(hits) < offset {
			t.Fatalf("window %d returned %d hits", offset+3, len(hits))
		}
		paged = append(paged, rankIDs(hits)[offset:]...)
	}
	if strings.Join(paged, ",") != strings.Join(whole, ",") {
		t.Fatalf("paged order = %v, whole order = %v", paged, whole)
	}
	if strings.Join(whole, ",") != "note-0,note-1,note-2,note-3,note-4,note-5,note-6" {
		t.Fatalf("order = %v, want newest first, then id", whole)
	}
}

// Bodies are read only when the query asks for them.
func TestIncludeBodyOnlyWhenRequested(t *testing.T) {
	index := newRankIndex(t,
		ports.Document{ID: "p/note-body", ProjectID: "p", Kind: "note", Title: "Body probe", Body: "the whole body text", UpdatedAt: rankClock},
	)
	preview := searchRanked(t, index, ports.Query{Text: "probe"})
	if len(preview) != 1 || preview[0].Body != "" {
		t.Fatalf("preview hit body = %q, want empty", preview[0].Body)
	}
	full := searchRanked(t, index, ports.Query{Text: "probe", IncludeBody: true})
	if len(full) != 1 || full[0].Body != "the whole body text" {
		t.Fatalf("full hit body = %q", full[0].Body)
	}
}

// The bounded score keeps a term repeated many times inside [0, 1) while the
// lexical signal still owns most of the share.
func TestScoreStaysInUnitRangeForStrongMatches(t *testing.T) {
	docs := []ports.Document{{
		ID: "p/note-repeat", ProjectID: "p", Kind: "note", Title: "Repeat probe",
		Body: strings.TrimSpace(strings.Repeat("saturate ", 200)), UpdatedAt: rankClock,
	}}
	for index := 0; index < 10; index++ {
		docs = append(docs, ports.Document{
			ID: fmt.Sprintf("p/note-filler-%d", index), ProjectID: "p", Kind: "note",
			Title: "Filler", Body: "unrelated filler", UpdatedAt: rankClock,
		})
	}
	index := newRankIndex(t, docs...)
	hits := searchRanked(t, index, ports.Query{Text: "saturate", Explain: true})
	if len(hits) != 1 || hits[0].ID != "note-repeat" {
		t.Fatalf("hits = %v, want the repeated note", rankIDs(hits))
	}
	got := hits[0]
	if got.Score >= 1 || got.Score <= 0 {
		t.Fatalf("score = %v, want (0, 1)", got.Score)
	}
	if share := got.Explain.BM25 / got.Score; share <= 0.5 {
		t.Fatalf("bm25 share = %v, want the lexical match to own most of the score", share)
	}
	if got.Explain.Link != 0 || got.Explain.Pin != 0 {
		t.Fatalf("unexpected pin/link contributions: %+v", got.Explain)
	}
}
