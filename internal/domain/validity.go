package domain

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// recordReferenceKinds are the kind prefixes every record id starts with.
var recordReferenceKinds = []string{"note", "decision", "attempt", "handoff"}

// maxRecordReferenceLength bounds a supersede link. Record ids are file name
// stems plus a 26-character ULID, so anything longer is a mistake.
const maxRecordReferenceLength = 200

// IsRecordReference reports whether value looks like a record id: a known kind
// prefix followed by a non-empty, path-safe identity. Legacy dated ids
// (decision-2026-01-01-slug) stay valid references; the ULID shape is not
// required.
func IsRecordReference(value string) bool {
	if value == "" || len(value) > maxRecordReferenceLength || !utf8.ValidString(value) {
		return false
	}
	if value != strings.TrimSpace(value) {
		return false
	}
	kind := ""
	for _, known := range recordReferenceKinds {
		prefix := known + "-"
		if strings.HasPrefix(value, prefix) && len(value) > len(prefix) {
			kind = prefix
			break
		}
	}
	if kind == "" {
		return false
	}
	body := value[len(kind):]
	if body == "" || body == "." || body == ".." {
		return false
	}
	for i := 0; i < len(body); i++ {
		char := body[i]
		allowed := char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.'
		if !allowed {
			return false
		}
	}
	return !strings.Contains(body, "..")
}

// ValidateRecordReference checks that value names a record like a stored link
// should. The field names the link in the error.
func ValidateRecordReference(field, value string) error {
	if !IsRecordReference(value) {
		return NewValidationError(field, fmt.Sprintf("%q is not a record id (want <kind>-... with kind note, decision, attempt or handoff)", value))
	}
	return nil
}

// Validity is the temporal validity and supersession state of a curated
// record. It is embedded in Note, Decision, Attempt and Handoff, so the fields
// serialize next to the record fields.
//
// The zero value keeps the pre-validity behavior: a record is current from its
// CreatedAt and never ends. A superseded record keeps its text and its file:
// only ValidTo and SupersededBy change, so git keeps the audit trail.
type Validity struct {
	// ValidFrom is when the claim became true. Zero means CreatedAt.
	ValidFrom time.Time `json:"validFrom,omitzero"`
	// ValidTo is when the claim stopped being true. Zero means current.
	ValidTo time.Time `json:"validTo,omitzero"`
	// SupersededBy is the id of the record that replaced this one.
	SupersededBy string `json:"supersededBy,omitempty"`
	// Supersedes is the id of the record this one replaced.
	Supersedes string `json:"supersedes,omitempty"`
	// Confidence is an optional 0..1 author confidence. Zero is unset.
	Confidence float64 `json:"confidence,omitempty"`
}

// Validate checks the validity fields that are set. createdAt supplies the
// default ValidFrom; selfID, when known, rejects a record that supersedes
// itself.
func (v Validity) Validate(field string, createdAt time.Time, selfID string) error {
	from := v.EffectiveFrom(createdAt)
	if !v.ValidTo.IsZero() && !from.IsZero() && !v.ValidTo.After(from) {
		return NewValidationError(field+".validTo",
			fmt.Sprintf("%s must be after valid_from %s", formatValidityTime(v.ValidTo), formatValidityTime(from)))
	}
	if v.SupersededBy != "" {
		if err := ValidateRecordReference(field+".supersededBy", v.SupersededBy); err != nil {
			return err
		}
		if v.SupersededBy == selfID {
			return NewValidationError(field+".supersededBy", "a record cannot be superseded by itself")
		}
	}
	if v.Supersedes != "" {
		if err := ValidateRecordReference(field+".supersedes", v.Supersedes); err != nil {
			return err
		}
		if v.Supersedes == selfID {
			return NewValidationError(field+".supersedes", "a record cannot supersede itself")
		}
	}
	if math.IsNaN(v.Confidence) || math.IsInf(v.Confidence, 0) || v.Confidence < 0 || v.Confidence > 1 {
		return NewValidationError(field+".confidence", "must be between 0 and 1")
	}
	return nil
}

// EffectiveFrom is the instant the record became true: ValidFrom, or the
// record's creation time when the field was never written.
func (v Validity) EffectiveFrom(createdAt time.Time) time.Time {
	if !v.ValidFrom.IsZero() {
		return v.ValidFrom
	}
	return createdAt
}

// IsCurrent reports whether the record has no end.
func (v Validity) IsCurrent() bool {
	return v.ValidTo.IsZero()
}

// EndedAt reports whether the record stopped being true at or before at.
func (v Validity) EndedAt(at time.Time) bool {
	return !v.ValidTo.IsZero() && !v.ValidTo.After(at)
}

// AllowsAt reports whether the record is valid at at: valid_from <= at and
// (valid_to is empty or valid_to > at). Both boundaries are half-open, so a
// record is current at its valid_from instant and gone at its valid_to
// instant.
func (v Validity) AllowsAt(at, createdAt time.Time) bool {
	from := v.EffectiveFrom(createdAt)
	if !from.IsZero() && from.After(at) {
		return false
	}
	if !v.ValidTo.IsZero() && !v.ValidTo.After(at) {
		return false
	}
	return true
}

// NormalizedFrom fills ValidFrom from createdAt. A superseded_by without an
// end is not repaired here: that inconsistency is a validation error, not a
// default.
func (v Validity) NormalizedFrom(createdAt time.Time) Validity {
	if v.ValidFrom.IsZero() && !createdAt.IsZero() {
		v.ValidFrom = createdAt.UTC()
	}
	return v
}

func formatValidityTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// ValidityFilter selects records by their temporal validity. The zero value is
// the default query: every record that is current now.
type ValidityFilter struct {
	// AsOf selects the records valid at that instant instead of now.
	AsOf time.Time
	// IncludeSuperseded keeps records whose validity ended. An AsOf instant
	// already selects the point in time, so this flag does not widen it.
	IncludeSuperseded bool
}

// Now returns the instant a default query evaluates validity at.
func (f ValidityFilter) Now(now time.Time) time.Time {
	if !f.AsOf.IsZero() {
		return f.AsOf
	}
	return now
}

// Allows reports whether a record with validity v and createdAt passes the
// filter at now.
func (f ValidityFilter) Allows(v Validity, createdAt, now time.Time) bool {
	if !f.AsOf.IsZero() {
		return v.AllowsAt(f.AsOf, createdAt)
	}
	if f.IncludeSuperseded {
		return true
	}
	// A record that ends in the future is still current; only an end at or
	// before now hides it.
	return v.ValidTo.IsZero() || v.ValidTo.After(now)
}

// ParseAsOf turns an as-of value into the instant a query asks about. It
// accepts an RFC 3339 (or RFC 3339 nano) time and a calendar date, which means
// midnight UTC because record timestamps are stored in UTC. An empty value
// returns the zero time, which means "now".
func ParseAsOf(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return at.UTC(), nil
	}
	if at, err := time.Parse(sinceDateLayout, value); err == nil {
		return at.UTC(), nil
	}
	return time.Time{}, NewValidationError("as-of",
		fmt.Sprintf("%q: want an RFC3339 time (2026-11-01T12:00:00Z) or a date (2026-11-01)", value))
}
