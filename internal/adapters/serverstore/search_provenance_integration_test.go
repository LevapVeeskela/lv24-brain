package serverstore

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestSearchQuarantineFilter proves server search drops explicitly quarantined
// records before ranking while keeping pre-provenance rows, whose empty origin
// and reviewed=false mean a human write (domain.Provenance.Quarantined).
func TestSearchQuarantineFilter(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	mustProject(t, ctx, store, "prov")

	legacy, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: "prov", Title: "Rollout legacy", Body: "rollout before provenance",
	})
	if err != nil {
		t.Fatalf("CreateNote legacy: %v", err)
	}
	if legacy.Note.Provenance.Quarantined() {
		t.Fatalf("legacy row (empty origin, reviewed=false) must not be quarantined: %+v", legacy.Note.Provenance)
	}
	quarantined, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: "prov", Title: "Rollout agent", Body: "rollout pending review",
		Provenance: domain.Provenance{Origin: domain.OriginAgent},
	})
	if err != nil {
		t.Fatalf("CreateNote quarantined: %v", err)
	}
	reviewed, _, err := store.CreateNote(ctx, domain.Note{
		ProjectID: "prov", Title: "Rollout reviewed", Body: "rollout reviewed agent",
		Provenance: domain.Provenance{Origin: domain.OriginAgent, Reviewed: true},
	})
	if err != nil {
		t.Fatalf("CreateNote reviewed: %v", err)
	}

	hits, err := store.Search(ctx, ports.Query{Text: "rollout", Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := make(map[string]bool, len(hits))
	for _, hit := range hits {
		found[hit.ID] = true
	}
	if !found[legacy.ID] {
		t.Fatalf("pre-provenance row %s missing from search: %+v", legacy.ID, hits)
	}
	if !found[reviewed.ID] {
		t.Fatalf("reviewed agent row %s missing from search: %+v", reviewed.ID, hits)
	}
	if found[quarantined.ID] {
		t.Fatalf("quarantined row %s leaked into search: %+v", quarantined.ID, hits)
	}

	// A review clears the quarantine, so the same row is searchable afterwards.
	if _, err := store.UpdateRecordProvenance(ctx, ports.RecordNote, quarantined.ID,
		domain.Provenance{Origin: domain.OriginAgent, Reviewed: true, Trust: domain.TrustVerified},
		quarantined.Revision); err != nil {
		t.Fatalf("UpdateRecordProvenance: %v", err)
	}
	hits, err = store.Search(ctx, ports.Query{Text: "rollout", Limit: 10})
	if err != nil {
		t.Fatalf("Search after review: %v", err)
	}
	for _, hit := range hits {
		if hit.ID == quarantined.ID {
			return
		}
	}
	t.Fatalf("reviewed row %s missing from search after review: %+v", quarantined.ID, hits)
}
