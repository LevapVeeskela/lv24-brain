package domain

import "fmt"

// Origin names the channel that authored a curated record. An empty origin is
// human: files written before provenance existed, and every CLI write, are
// treated as the owner's own text.
type Origin string

const (
	// OriginHuman is a write by the owner through the CLI or a hand edit.
	OriginHuman Origin = "human"
	// OriginAgent is a write by an agent through MCP or a session hook.
	OriginAgent Origin = "agent"
	// OriginConnector is a record promoted from a connector proposal.
	OriginConnector Origin = "connector"
	// OriginImport is a record created by a migration or importer.
	OriginImport Origin = "import"
)

// Valid reports whether o is a known origin.
func (o Origin) Valid() bool {
	switch o {
	case OriginHuman, OriginAgent, OriginConnector, OriginImport:
		return true
	default:
		return false
	}
}

// Effective returns the origin an empty value stands for. Unknown values are
// returned unchanged so a reader can still report them.
func (o Origin) Effective() Origin {
	if o == "" {
		return OriginHuman
	}
	return o
}

// Trust names how strongly a record's claim is supported.
type Trust string

const (
	// TrustVerified is a claim confirmed by evidence or by the owner. It is the
	// default, so records written before trust existed stay verified.
	TrustVerified Trust = "verified"
	// TrustStated is a claim asserted by its author (a promoted proposal).
	TrustStated Trust = "stated"
	// TrustInferred is a claim derived or guessed rather than asserted.
	TrustInferred Trust = "inferred"
)

// Valid reports whether t is a known trust level.
func (t Trust) Valid() bool {
	switch t {
	case TrustVerified, TrustStated, TrustInferred:
		return true
	default:
		return false
	}
}

// Effective returns the trust level an empty value stands for. An unknown
// value is treated as inferred: the safest level, never as verified.
func (t Trust) Effective() Trust {
	switch {
	case t == "":
		return TrustVerified
	case !t.Valid():
		return TrustInferred
	default:
		return t
	}
}

// Provenance is the authorship, review state and trust level of a curated
// record. It is embedded in Note, Decision, Attempt and Handoff, so the fields
// serialize next to the record fields.
//
// The zero value keeps the pre-provenance behavior: a human write, reviewed,
// verified. A record that must be quarantined sets Reviewed to false and an
// origin other than human (agent, connector or import).
type Provenance struct {
	Origin   Origin `json:"origin,omitempty"`
	Reviewed bool   `json:"reviewed,omitempty"`
	Trust    Trust  `json:"trust,omitempty"`
}

// Validate rejects origin and trust values outside the documented enums. An
// empty value is valid and means human/verified. Reads preserve unknown values
// instead of rejecting the file, so only write paths call Validate.
func (p Provenance) Validate() error {
	if p.Origin != "" && !p.Origin.Valid() {
		return NewValidationError("origin", fmt.Sprintf("unknown origin %q (human, agent, connector, import)", p.Origin))
	}
	if p.Trust != "" && !p.Trust.Valid() {
		return NewValidationError("trust", fmt.Sprintf("unknown trust %q (verified, stated, inferred)", p.Trust))
	}
	return nil
}

// Normalized fills the defaults of an omitted provenance. A human write is
// reviewed by definition: the CLI action is the review. Agent, connector and
// import writes keep Reviewed as given, so an unreviewed write stays
// quarantined.
func (p Provenance) Normalized() Provenance {
	if p.Origin == "" {
		p.Origin = OriginHuman
	}
	if p.Origin == OriginHuman {
		p.Reviewed = true
	}
	if p.Trust == "" {
		p.Trust = TrustVerified
	}
	return p
}

// ReviewedState reports whether the record counts as reviewed. A human origin
// is reviewed by definition: the owner typed it, or it predates provenance. To
// quarantine a record, write it with an agent, connector or import origin and
// Reviewed false.
func (p Provenance) ReviewedState() bool {
	if p.Origin == "" || p.Origin == OriginHuman {
		return true
	}
	return p.Reviewed
}

// Quarantined reports whether the record must stay out of trusted context:
// an explicit unreviewed non-human write, or an origin this engine does not
// know. An unknown origin is preserved on read, but it cannot be trusted to
// mean human.
func (p Provenance) Quarantined() bool {
	if p.Origin != "" && !p.Origin.Valid() {
		return true
	}
	return !p.ReviewedState()
}

// NeedsReview reports whether marking the record reviewed clears its
// quarantine. A malformed origin is not fixed by review; it needs a hand edit.
func (p Provenance) NeedsReview() bool {
	return p.Origin != "" && p.Origin.Valid() && p.Origin != OriginHuman && !p.Reviewed
}

// EffectiveTrust returns the trust level to act on, folding unknown values to
// inferred.
func (p Provenance) EffectiveTrust() Trust {
	return p.Trust.Effective()
}
