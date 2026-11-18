package sqliteindex

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
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

// rankIDs returns the ids of the hits in rank order.
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

// explainSum is the absolute contribution of every explained signal.
func explainSum(hit ports.Hit) float64 {
	if hit.Explain == nil {
		return math.NaN()
	}
	return hit.Explain.BM25 + hit.Explain.Recency + hit.Explain.Importance + hit.Explain.Pin + hit.Explain.Link
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
	// The recency contribution is the weight times the half-life decay: 60
	// days is two half-lives, one day is a thirtieth of one.
	if got, want := hits[1].Explain.Recency, WeightRecency*0.25; math.Abs(got-want) > 1e-9 {
		t.Fatalf("old record recency contribution = %.6f, want %.6f", got, want)
	}
	if got, want := hits[0].Explain.Recency, WeightRecency*math.Pow(0.5, 1.0/30.0); math.Abs(got-want) > 1e-9 {
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
// The authority column is the kind importance plus the provenance trust; these
// documents carry no trust value, so that half stays neutral.
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
	if got, want := hits[0].Explain.Importance, WeightImportance*1.0+WeightTrust*defaultTrust; math.Abs(got-want) > 1e-9 {
		t.Fatalf("decision authority = %.6f, want %.6f", got, want)
	}
	if got, want := hits[1].Explain.Importance, WeightImportance*0.5+WeightTrust*defaultTrust; math.Abs(got-want) > 1e-9 {
		t.Fatalf("note authority = %.6f, want %.6f", got, want)
	}
	if !strings.Contains(strings.Join(hits[0].MatchReasons, ","), ports.ReasonImportance) {
		t.Fatalf("decision reasons = %v, want importance", hits[0].MatchReasons)
	}
	if strings.Contains(strings.Join(hits[1].MatchReasons, ","), ports.ReasonImportance) {
		t.Fatalf("note reasons = %v, want no importance", hits[1].MatchReasons)
	}
}

// The trust signal reads the projected provenance: verified 1.0, stated 0.6,
// inferred 0.3. With the same text, kind and age only trust separates the
// records, and a row without a trust value (written before schema v9) stays
// neutral instead of being guessed down.
func TestRankingTrustOrdering(t *testing.T) {
	at := rankClock.Add(-time.Hour)
	note := func(id, origin, trust string) ports.Document {
		return ports.Document{
			ID: id, ProjectID: "p", Kind: "note", Title: "Provenance probe",
			Body: "provenance body", UpdatedAt: at, Origin: origin, Trust: trust,
		}
	}
	index := newRankIndex(t,
		note("p/note-inferred", "agent", string(domain.TrustInferred)),
		note("p/note-verified", "human", string(domain.TrustVerified)),
		note("p/note-stated", "connector", string(domain.TrustStated)),
		note("p/note-legacy", "", ""),
	)
	hits := searchRanked(t, index, ports.Query{Text: "provenance", Explain: true})
	// The legacy row and the verified row tie on the score; the id order
	// breaks that tie, so the expected order is fully deterministic.
	want := []string{"note-legacy", "note-verified", "note-stated", "note-inferred"}
	if got := rankIDs(hits); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rank order = %v, want %v", got, want)
	}
	levels := map[string]float64{
		"note-verified": trustVerified,
		"note-stated":   trustStated,
		"note-inferred": trustInferred,
		"note-legacy":   defaultTrust,
	}
	for _, hit := range hits {
		wantAuthority := WeightImportance*importanceByKind["note"] + WeightTrust*levels[hit.ID]
		if got := hit.Explain.Importance; math.Abs(got-wantAuthority) > 1e-9 {
			t.Fatalf("%s authority = %.6f, want %.6f", hit.ID, got, wantAuthority)
		}
		if math.Abs(explainSum(hit)-hit.Score) > 1e-9 {
			t.Fatalf("%s contributions = %.9f, score = %.9f", hit.ID, explainSum(hit), hit.Score)
		}
	}
}

// storedProvenance reads the provenance columns of one indexed document.
func storedProvenance(t *testing.T, index *Index, id string) (origin, trust string) {
	t.Helper()
	if err := index.db.QueryRowContext(context.Background(),
		`SELECT origin, trust FROM documents WHERE id = ?`, id).Scan(&origin, &trust); err != nil {
		t.Fatalf("read provenance columns of %s: %v", id, err)
	}
	return origin, trust
}

// A metadata-only rewrite (pin, review, snooze) re-upserts the document with
// unchanged text; the projected provenance must survive it, and a real trust
// change must still reach the ranking.
func TestProvenanceSurvivesMetadataOnlyReindex(t *testing.T) {
	ctx := context.Background()
	at := rankClock.Add(-time.Hour)
	doc := ports.Document{
		ID: "p/note-meta", ProjectID: "p", Kind: "note", Title: "Metadata probe",
		Body: "metadata body", UpdatedAt: at, Origin: "agent", Trust: string(domain.TrustInferred),
	}
	index := newRankIndex(t, doc)
	if origin, trust := storedProvenance(t, index, doc.ID); origin != "agent" || trust != "inferred" {
		t.Fatalf("stored provenance = %q/%q, want agent/inferred", origin, trust)
	}
	authority := func() float64 {
		hits := searchRanked(t, index, ports.Query{Text: "metadata", Explain: true})
		if len(hits) != 1 || hits[0].Explain == nil {
			t.Fatalf("hits = %+v, want one explained hit", hits)
		}
		return hits[0].Explain.Importance
	}
	wantInferred := WeightImportance*importanceByKind["note"] + WeightTrust*trustInferred
	if got := authority(); math.Abs(got-wantInferred) > 1e-9 {
		t.Fatalf("inferred authority = %.6f, want %.6f", got, wantInferred)
	}

	// Pin the record: a metadata-only write of the same text and provenance.
	pinned := doc
	pinned.Pinned = true
	if err := index.UpsertBatch(ctx, []ports.Document{pinned}); err != nil {
		t.Fatalf("reindex after a metadata-only write: %v", err)
	}
	if origin, trust := storedProvenance(t, index, doc.ID); origin != "agent" || trust != "inferred" {
		t.Fatalf("provenance after metadata-only reindex = %q/%q, want agent/inferred", origin, trust)
	}
	if got := authority(); math.Abs(got-wantInferred) > 1e-9 {
		t.Fatalf("authority after metadata-only reindex = %.6f, want %.6f", got, wantInferred)
	}

	// A review that changes the trust must update the projection, not keep a
	// stale value: the same record now ranks as verified.
	reviewed := pinned
	reviewed.Trust = string(domain.TrustVerified)
	if err := index.Upsert(ctx, reviewed); err != nil {
		t.Fatalf("reindex after a trust change: %v", err)
	}
	if origin, trust := storedProvenance(t, index, doc.ID); origin != "agent" || trust != "verified" {
		t.Fatalf("provenance after a trust change = %q/%q, want agent/verified", origin, trust)
	}
	wantVerified := WeightImportance*importanceByKind["note"] + WeightTrust*trustVerified
	if got := authority(); math.Abs(got-wantVerified) > 1e-9 {
		t.Fatalf("verified authority = %.6f, want %.6f", got, wantVerified)
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

// Relevance is normalized per query, so the ranking keeps the bm25 ratio
// instead of dropping a record the moment it is not the single best match:
// here the stale record is 1.2x better lexically and still loses, because the
// fresh record is a fifth of a score fresher while the whole relevance swing
// is worth 0.55.
func TestFreshWeakerRelevanceOutranksStaleStronger(t *testing.T) {
	index := newRankIndex(t,
		ports.Document{ID: "p/stale", ProjectID: "p", Kind: "note", Title: "Ledger probe",
			Body: "ledger", UpdatedAt: rankClock.Add(-120 * 24 * time.Hour)},
		ports.Document{ID: "p/fresh", ProjectID: "p", Kind: "note", Title: "Ledger probe",
			Body: "ledger notes notes notes notes notes notes", UpdatedAt: rankClock},
		ports.Document{ID: "p/filler", ProjectID: "p", Kind: "note", Title: "Other",
			Body: "unrelated", UpdatedAt: rankClock.Add(-200 * 24 * time.Hour)},
	)
	hits := searchRanked(t, index, ports.Query{Text: "ledger", Explain: true})
	if got := rankIDs(hits); len(got) != 2 || got[0] != "fresh" || got[1] != "stale" {
		t.Fatalf("rank order = %v, want the fresh record first", got)
	}
	ratio := hits[1].Explain.BM25 / hits[0].Explain.BM25
	if ratio < 1.15 || ratio > 1.30 {
		t.Fatalf("lexical ratio = %.4f, want the stale record ~1.2x stronger", ratio)
	}
	if hits[0].Score <= hits[1].Score {
		t.Fatalf("fresh score %.4f <= stale score %.4f", hits[0].Score, hits[1].Score)
	}
}

// A pin is worth 0.15 of the score, a relevance gap of 2x is worth more: a
// pinned record that is stale and lexically half as good does not jump the
// fresh, better match.
func TestPinnedStaleDoesNotOutrankFreshStrongerRelevance(t *testing.T) {
	index := newRankIndex(t,
		ports.Document{ID: "p/stale-pinned", ProjectID: "p", Kind: "note", Title: "Probe only",
			Body: "ledger totals", Pinned: true, UpdatedAt: rankClock.Add(-120 * 24 * time.Hour)},
		ports.Document{ID: "p/fresh-strong", ProjectID: "p", Kind: "note", Title: "Ledger probe",
			Body: "ledger totals", UpdatedAt: rankClock},
		ports.Document{ID: "p/filler", ProjectID: "p", Kind: "note", Title: "Other",
			Body: "unrelated", UpdatedAt: rankClock.Add(-200 * 24 * time.Hour)},
	)
	hits := searchRanked(t, index, ports.Query{Text: "ledger", Explain: true})
	if got := rankIDs(hits); len(got) != 2 || got[0] != "fresh-strong" || got[1] != "stale-pinned" {
		t.Fatalf("rank order = %v, want the fresh strong match first", got)
	}
	pinned := hits[1]
	if got, want := pinned.Explain.Pin, WeightPin*1.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("pin contribution = %.6f, want %.6f", got, want)
	}
	if pinned.Explain.BM25 >= hits[0].Explain.BM25 {
		t.Fatalf("pinned relevance %.4f >= fresh relevance %.4f, want a 2x lexical gap",
			pinned.Explain.BM25, hits[0].Explain.BM25)
	}
	if !strings.Contains(strings.Join(pinned.MatchReasons, ","), ports.ReasonPin) {
		t.Fatalf("pinned reasons = %v, want pin", pinned.MatchReasons)
	}
}

// Authority is 0.10 at most, relevance 0.55: a low-authority record with a
// clearly better match still ranks first, which is what keeps auto-captured
// and inferred records visible. The records carry no explicit trust value, so
// the trust half of the authority term stays neutral here.
func TestLowAuthorityContentStillRanksOnRelevance(t *testing.T) {
	index := newRankIndex(t,
		ports.Document{ID: "p/note-strong", ProjectID: "p", Kind: "note", Title: "Ledger probe",
			Body: "ledger totals", UpdatedAt: rankClock},
		ports.Document{ID: "p/decision-weak", ProjectID: "p", Kind: "decision", Title: "Probe only",
			Body: "ledger mention", Status: "proposed", UpdatedAt: rankClock.Add(-time.Hour)},
		ports.Document{ID: "p/filler", ProjectID: "p", Kind: "note", Title: "Other",
			Body: "unrelated", UpdatedAt: rankClock.Add(-200 * 24 * time.Hour)},
	)
	hits := searchRanked(t, index, ports.Query{Text: "ledger", Explain: true})
	if got := rankIDs(hits); len(got) != 2 || got[0] != "note-strong" || got[1] != "decision-weak" {
		t.Fatalf("rank order = %v, want relevance to decide", got)
	}
	if hits[1].Explain.Importance <= hits[0].Explain.Importance {
		t.Fatalf("decision authority %.4f <= note authority %.4f, want the decision to hold the authority edge",
			hits[1].Explain.Importance, hits[0].Explain.Importance)
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

// MinScore drops hits below the composite threshold, and the threshold is the
// score the caller sees.
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
	if got := explainSum(pinned); math.Abs(got-pinned.Score) > 1e-9 {
		t.Fatalf("contributions sum = %.6f, score = %.6f", got, pinned.Score)
	}
	if got, want := pinned.Explain.Pin, WeightPin*1.0; math.Abs(got-want) > 1e-9 {
		t.Fatalf("pin contribution = %.6f, want %.6f", got, want)
	}
	reasons := strings.Join(pinned.MatchReasons, ",")
	for _, want := range []string{ports.ReasonBM25, ports.ReasonRecency, ports.ReasonImportance, ports.ReasonPin} {
		if !strings.Contains(reasons, want) {
			t.Fatalf("reasons = %q, want %s", reasons, want)
		}
	}
	plain := strings.Join(hits[1].MatchReasons, ",")
	for _, unwanted := range []string{ports.ReasonImportance, ports.ReasonPin} {
		if strings.Contains(plain, unwanted) {
			t.Fatalf("plain note reasons = %q, want no %s", plain, unwanted)
		}
	}
}

// The explained shares are the contributions over the score and add up to 1
// for every hit, whatever the mix of signals.
func TestExplainSharesSumToOne(t *testing.T) {
	index := newRankIndex(t,
		ports.Document{ID: "p/decision", ProjectID: "p", Kind: "decision", Title: "Quartz release",
			Body: "quartz gate", Status: "accepted", UpdatedAt: rankClock.Add(-72 * time.Hour)},
		ports.Document{ID: "p/note", ProjectID: "p", Kind: "note", Title: "Quartz notes",
			Body: "quartz quartz", UpdatedAt: rankClock.Add(-time.Minute)},
		ports.Document{ID: "p/attempt", ProjectID: "p", Kind: "attempt", Title: "Quartz try",
			Body: "quartz once", Confidence: 0.4, UpdatedAt: rankClock.Add(-40 * 24 * time.Hour)},
		ports.Document{ID: "p/handoff", ProjectID: "p", Kind: "handoff", Title: "Quartz handoff",
			Body: "quartz", UpdatedAt: rankClock.Add(-200 * 24 * time.Hour)},
	)
	hits := searchRanked(t, index, ports.Query{Text: "quartz", Explain: true})
	if len(hits) != 4 {
		t.Fatalf("hits = %v, want four", rankIDs(hits))
	}
	for _, hit := range hits {
		if hit.Explain == nil || hit.Score <= 0 {
			t.Fatalf("%s: explain = %+v, score = %v", hit.ID, hit.Explain, hit.Score)
		}
		sum := explainSum(hit)
		if math.Abs(sum-hit.Score) > 1e-9 {
			t.Fatalf("%s: contributions = %.9f, score = %.9f", hit.ID, sum, hit.Score)
		}
		share := sum / hit.Score
		if math.Abs(share-1) > 1e-9 {
			t.Fatalf("%s: shares = %.9f, want 1", hit.ID, share)
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

// The normalized score keeps a term repeated many times inside (0, 1) while the
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
	if math.Abs(explainSum(got)-got.Score) > 1e-9 {
		t.Fatalf("contributions = %.9f, score = %.9f", explainSum(got), got.Score)
	}
}
