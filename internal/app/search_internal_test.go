package app

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestWantsRawText covers which requests ask for raw-zone text.
func TestWantsRawText(t *testing.T) {
	cases := []struct {
		name    string
		kinds   []string
		dumps   bool
		wantRaw bool
	}{
		{"curated default", nil, false, false},
		{"include dumps", nil, true, true},
		{"kind item", []string{KindItem}, false, true},
		{"kind dump", []string{KindDump}, false, true},
		{"kind note with dumps", []string{"note"}, true, false},
		{"kind note", []string{"note"}, false, false},
	}
	for _, tc := range cases {
		if got := wantsRawText(tc.kinds, tc.dumps); got != tc.wantRaw {
			t.Errorf("%s: wantsRawText(%v, %v) = %v", tc.name, tc.kinds, tc.dumps, got)
		}
	}
}

// TestDropRawKinds is the curated-search safety net: even a store that ignored
// the exclude filter cannot surface raw kinds in a curated response.
func TestDropRawKinds(t *testing.T) {
	hits := []ports.Hit{
		{ID: "n1", Kind: "note"},
		{ID: "item:x", Kind: KindItem},
		{ID: "dump:mail/1.md", Kind: KindDump},
		{ID: "d1", Kind: "decision"},
	}
	kept := dropRawKinds(hits)
	if len(kept) != 2 || kept[0].ID != "n1" || kept[1].ID != "d1" {
		t.Fatalf("kept = %+v", kept)
	}
}
