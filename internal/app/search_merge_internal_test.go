package app

import (
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// hit builds one synthetic hit for the merge tests.
func hit(id string, score float64) ports.Hit {
	return ports.Hit{ID: id, ProjectID: "p", Kind: ports.RecordNote, Title: id, Score: score}
}

// Strict hits own the top of a merged page: the OR pass normalizes over a
// larger match set, so its raw scores are not comparable with the strict ones
// and an OR-only record must never outrank a record that matched every term.
func TestMergeRelaxedHitsKeepsStrictHitsFirst(t *testing.T) {
	strict := []ports.Hit{hit("strict-1", 0.40), hit("strict-2", 0.30), hit("strict-3", 0.20)}
	relaxed := []ports.Hit{
		hit("or-1", 0.95), hit("or-2", 0.90), hit("or-3", 0.85),
		// A strict record repeated by the OR pass keeps its strict score.
		hit("strict-1", 0.10),
	}
	merged, used := mergeRelaxedHits(strict, relaxed, 20)
	if !used {
		t.Fatal("merge reported no relaxed contribution")
	}
	ids := make([]string, len(merged))
	for index, entry := range merged {
		ids[index] = entry.ID
	}
	if got, want := strings.Join(ids, ","),
		"strict-1,strict-2,strict-3,or-1,or-2,or-3"; got != want {
		t.Fatalf("merged order = %s, want %s", got, want)
	}
	if merged[0].Score != 0.40 {
		t.Fatalf("strict-1 score = %v, want the strict 0.40, not the OR 0.10", merged[0].Score)
	}
}

// The partition also holds when the OR page fills the whole limit: the strict
// group is cut first, never the OR tail above it.
func TestMergeRelaxedHitsPartitionSurvivesLimit(t *testing.T) {
	strict := []ports.Hit{hit("strict-1", 0.40), hit("strict-2", 0.30), hit("strict-3", 0.20)}
	relaxed := []ports.Hit{hit("or-1", 0.95), hit("or-2", 0.90), hit("or-3", 0.85)}
	merged, used := mergeRelaxedHits(strict, relaxed, 4)
	if !used {
		t.Fatal("merge reported no relaxed contribution")
	}
	ids := make([]string, len(merged))
	for index, entry := range merged {
		ids[index] = entry.ID
	}
	if got, want := strings.Join(ids, ","), "strict-1,strict-2,strict-3,or-1"; got != want {
		t.Fatalf("merged order = %s, want %s", got, want)
	}
}
