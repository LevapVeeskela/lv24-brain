package app

import (
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestPromotedProvenanceIsReviewedConnectorStated(t *testing.T) {
	provenance := promotedProvenance().Normalized()
	if provenance.Origin != domain.OriginConnector || !provenance.Reviewed || provenance.Trust != domain.TrustStated {
		t.Fatalf("promoted provenance = %+v, want connector/reviewed/stated", provenance)
	}
	if provenance.Quarantined() {
		t.Fatal("a promoted record is quarantined")
	}
}
