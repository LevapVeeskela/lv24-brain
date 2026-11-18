package main

import (
	"math"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// The share of every signal is its contribution over the score, so the shares
// add up to 1 and the text rendering prints both columns. A zero score
// explains zeros instead of dividing by zero.
func TestExplainSharesAddUpToOne(t *testing.T) {
	hit := ports.Hit{
		Score:        0.5,
		MatchReasons: []string{ports.ReasonBM25},
		Explain: &ports.Explain{
			BM25: 0.3, Recency: 0.1, Importance: 0.05, Pin: 0.05, Link: 0,
		},
	}
	contributions := []float64{hit.Explain.BM25, hit.Explain.Recency, hit.Explain.Importance, hit.Explain.Pin, hit.Explain.Link}
	shares := explainShares(hit)
	sum := 0.0
	for index, share := range shares {
		sum += share
		if want := contributions[index] / hit.Score; math.Abs(share-want) > 1e-12 {
			t.Fatalf("share %d = %.9f, want %.9f", index, share, want)
		}
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Fatalf("shares = %.9f, want 1", sum)
	}

	line := explainLine(hit)
	for _, part := range []string{"reasons: bm25", "contribution", "share", "bm25 0.300", "bm25 0.600"} {
		if !strings.Contains(line, part) {
			t.Fatalf("explain line %q does not contain %q", line, part)
		}
	}

	zero := hit
	zero.Score = 0
	for index, share := range explainShares(zero) {
		if share != 0 {
			t.Fatalf("zero-score share %d = %v, want 0", index, share)
		}
	}
}
