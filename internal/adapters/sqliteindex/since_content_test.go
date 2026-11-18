package sqliteindex

import (
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestSearchSinceComparesContentTime pins the --since rule of the index: the
// window compares the last text change, so a metadata write that moved
// updated_at does not make a record recent.
func TestSearchSinceComparesContentTime(t *testing.T) {
	index := newTestIndex(t)
	// A review rewrote the frontmatter: updated_at moved, the text did not.
	reviewed := testDocument("reviewed", "brain", "retention policy unchanged")
	reviewed.ContentUpdatedAt = baseTime
	reviewed.UpdatedAt = baseTime.Add(48 * time.Hour)
	// A text edit moved both.
	edited := testDocument("edited", "brain", "retention policy rewritten")
	edited.ContentUpdatedAt = baseTime.Add(30 * time.Hour)
	edited.UpdatedAt = baseTime.Add(30 * time.Hour)
	// A record indexed before the column existed carries no content time and
	// must fall back to its revision.
	legacy := testDocument("legacy", "brain", "retention policy legacy")
	legacy.UpdatedAt = baseTime.Add(2 * time.Hour)
	mustUpsert(t, index, reviewed, edited, legacy)

	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "retention", Since: baseTime.Add(24 * time.Hour)}), "edited")
	assertResultIDs(t, mustSearch(t, index, ports.Query{Text: "retention", Since: baseTime.Add(time.Hour)}), "edited", "legacy")
}
