package domain

import (
	"errors"
	"testing"
)

func TestProvenanceZeroValueIsHumanReviewedVerified(t *testing.T) {
	zero := Provenance{}
	if zero.Quarantined() {
		t.Fatal("zero provenance is quarantined")
	}
	if zero.NeedsReview() {
		t.Fatal("zero provenance needs review")
	}
	if zero.Origin.Effective() != OriginHuman {
		t.Fatalf("effective origin = %q, want human", zero.Origin.Effective())
	}
	if zero.EffectiveTrust() != TrustVerified {
		t.Fatalf("effective trust = %q, want verified", zero.EffectiveTrust())
	}
	normalized := zero.Normalized()
	if normalized.Origin != OriginHuman || !normalized.Reviewed || normalized.Trust != TrustVerified {
		t.Fatalf("normalized zero = %+v, want human/reviewed/verified", normalized)
	}
}

func TestProvenanceNormalizationKeepsAgentQuarantine(t *testing.T) {
	agent := Provenance{Origin: OriginAgent}.Normalized()
	if agent.Reviewed {
		t.Fatal("normalizing an agent write reviewed it")
	}
	if !agent.Quarantined() {
		t.Fatal("unreviewed agent write is not quarantined")
	}
	connector := Provenance{Origin: OriginConnector, Reviewed: true, Trust: TrustStated}.Normalized()
	if connector.Quarantined() || connector.Trust != TrustStated {
		t.Fatalf("promoted provenance = %+v", connector)
	}
}

func TestProvenanceValidateRejectsUnknownEnums(t *testing.T) {
	for name, provenance := range map[string]Provenance{
		"origin": {Origin: Origin("robot"), Reviewed: true},
		"trust":  {Origin: OriginHuman, Reviewed: true, Trust: Trust("guessed")},
	} {
		if err := provenance.Validate(); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: Validate() = %v, want a validation error", name, err)
		}
	}
	records := map[string]interface{ Validate() error }{
		"note":     Note{ProjectID: "p", Body: "b", Provenance: Provenance{Origin: Origin("robot")}},
		"decision": Decision{ProjectID: "p", Title: "t", Provenance: Provenance{Origin: Origin("robot")}},
		"attempt":  Attempt{ProjectID: "p", Title: "t", Provenance: Provenance{Origin: Origin("robot")}},
		"handoff":  Handoff{ProjectID: "p", Summary: "s", Provenance: Provenance{Origin: Origin("robot")}},
	}
	for name, record := range records {
		if err := record.Validate(); !errors.Is(err, ErrValidation) {
			t.Fatalf("%s: Validate() = %v, want a validation error", name, err)
		}
	}
}

func TestProvenanceUnknownReadIsPreservedAndUntrusted(t *testing.T) {
	read := Provenance{Origin: Origin("robot"), Reviewed: true, Trust: Trust("guessed")}
	// The values survive a read, so a reader can report them...
	if read.Origin != "robot" || read.Trust != "guessed" {
		t.Fatalf("unknown values were not preserved: %+v", read)
	}
	// ...but they are never trusted.
	if !read.Quarantined() {
		t.Fatal("unknown origin is not quarantined")
	}
	if read.NeedsReview() {
		t.Fatal("an unknown origin is not fixed by review")
	}
	if read.EffectiveTrust() != TrustInferred {
		t.Fatalf("unknown trust = %q, want inferred", read.EffectiveTrust())
	}
}
