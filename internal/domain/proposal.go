package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Proposal contracts (contracts-v1). A proposal is a machine-local candidate
// record derived from one external source item. It never becomes curated
// knowledge until a human or agent promotes it; ingestion and digest never
// write Notes, Decisions or Attempts directly.

const (
	// ProposalSchemaVersion is the newest proposal layout this engine writes.
	ProposalSchemaVersion = 1

	// ProposalHashPrefix prefixes every Proposal.PayloadHash value.
	ProposalHashPrefix = "sha256:"

	maxProposalRuleLength           = 64
	maxProposalTitleLength          = 512
	maxProposalBodyBytes            = 256 << 10
	maxProposalSections             = 16
	maxProposalSectionHeadingLength = 128
	maxProposalCitations            = 50
	maxProposalCitationURLLength    = 2048
	maxProposalCitationTitleLength  = 512
	maxProposalRecordIDLength       = 256
	proposalIDDigestLength          = 12
	proposalIDPrefix                = "proposal-"
	proposalIDMaxLength             = 128
)

// ProposalStatus is the lifecycle state of a proposal. pending and stale are
// mutable; promoted and dismissed are terminal.
type ProposalStatus string

const (
	ProposalPending   ProposalStatus = "pending"
	ProposalPromoted  ProposalStatus = "promoted"
	ProposalDismissed ProposalStatus = "dismissed"
	ProposalStale     ProposalStatus = "stale"
)

// Valid reports whether s is a known status.
func (s ProposalStatus) Valid() bool {
	switch s {
	case ProposalPending, ProposalPromoted, ProposalDismissed, ProposalStale:
		return true
	default:
		return false
	}
}

// Terminal reports whether s can no longer be overwritten by a digest rerun.
func (s ProposalStatus) Terminal() bool {
	return s == ProposalPromoted || s == ProposalDismissed
}

// ProposalKind is the curated record a proposal would become.
type ProposalKind string

const (
	ProposalDecision ProposalKind = "decision"
	ProposalAttempt  ProposalKind = "attempt"
	ProposalNote     ProposalKind = "note"
)

// Valid reports whether k is a known proposal kind.
func (k ProposalKind) Valid() bool {
	switch k {
	case ProposalDecision, ProposalAttempt, ProposalNote:
		return true
	default:
		return false
	}
}

// ProposalSection is one ordered Markdown section of a candidate record.
type ProposalSection struct {
	Heading string `json:"heading"`
	Body    string `json:"body,omitempty"`
}

// ProposalCitation points back to the external item or page the proposal was
// derived from. ObservedAt is informational and excluded from PayloadHash, so
// a replay that only observes the item again does not count as new content.
type ProposalCitation struct {
	ConnectionID string    `json:"connectionId,omitempty"`
	ItemKey      string    `json:"itemKey,omitempty"`
	ExternalID   string    `json:"externalId,omitempty"`
	URL          string    `json:"url,omitempty"`
	Title        string    `json:"title,omitempty"`
	ObservedAt   time.Time `json:"observedAt,omitzero"`
}

// Validate checks the citation identity and display fields.
func (c ProposalCitation) Validate() error {
	if c.ConnectionID != "" {
		if err := ValidateConnectionID("citation.connectionId", c.ConnectionID); err != nil {
			return err
		}
	}
	if c.ItemKey != "" {
		if err := ValidateItemKey("citation.itemKey", c.ItemKey); err != nil {
			return err
		}
		if err := validateItemKeyConnection("citation.itemKey", c.ConnectionID, c.ItemKey); err != nil {
			return err
		}
	}
	if c.ExternalID != "" {
		if err := ValidateExternalID("citation.externalId", c.ExternalID); err != nil {
			return err
		}
	}
	if c.URL != "" {
		if err := validateSourceURL("citation.url", c.URL, maxProposalCitationURLLength); err != nil {
			return err
		}
	}
	if c.URL == "" && c.ExternalID == "" && c.ItemKey == "" {
		return NewValidationError("citation", "needs a URL, external id or item key")
	}
	if err := validateLabel("citation.title", c.Title, maxProposalCitationTitleLength); err != nil {
		return err
	}
	return nil
}

// Proposal is a pending candidate curated record for one source item.
type Proposal struct {
	// SchemaVersion is the proposal layout version (0 means "current").
	SchemaVersion int `json:"schemaVersion"`
	// ProjectID is the brain project the proposal belongs to. This type only
	// requires a trimmed non-empty value; the fsstore adapter additionally
	// enforces its path-safe project alphabet before using it as a directory.
	ProjectID string `json:"projectId"`
	// ID is deterministic: proposal-<connection>-<sha256(project NUL
	// connection NUL externalID)[:12]>.
	ID string `json:"id"`
	// Kind is the curated record the proposal would become.
	Kind ProposalKind `json:"kind"`
	// Rule names the deterministic digest rule that produced it.
	Rule string `json:"rule"`
	// Confidence is a heuristic in [0,1]. It is never auto-accepted.
	Confidence float64 `json:"confidence"`
	// ConnectionID is the source connection the item came from.
	ConnectionID string `json:"connectionId"`
	// ExternalID is the provider's stable id (issue node id, Jira key...).
	ExternalID string `json:"externalId"`
	// Title is the candidate record title.
	Title string `json:"title"`
	// Body is free-form Markdown for note candidates and preambles.
	Body string `json:"body,omitempty"`
	// Sections are ordered Markdown sections mapped to record fields.
	Sections []ProposalSection `json:"sections,omitempty"`
	// Citations point back to the source items.
	Citations []ProposalCitation `json:"citations,omitempty"`
	// PayloadHash is ComputePayloadHash; it excludes status and timestamps.
	PayloadHash string `json:"payloadHash"`
	// Status is pending until promotion or dismissal.
	Status ProposalStatus `json:"status"`
	// CreatedAt is preserved across pending rewrites.
	CreatedAt time.Time `json:"createdAt,omitzero"`
	// UpdatedAt is the last content or status change.
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	// PromotedRecordID is the curated record created by promotion.
	PromotedRecordID string `json:"promotedRecordId,omitempty"`
	// PromotedAt is when promotion succeeded.
	PromotedAt time.Time `json:"promotedAt,omitzero"`
}

// ProposalID returns the deterministic ID for one source item. The hash covers
// the exact project, connection and external id separated by NUL, so path-like
// or dotted ids never collide.
func ProposalID(projectID, connectionID, externalID string) string {
	sum := sha256.Sum256([]byte(projectID + "\x00" + connectionID + "\x00" + externalID))
	return proposalIDPrefix + connectionID + "-" + hex.EncodeToString(sum[:])[:proposalIDDigestLength]
}

// ValidateProposalID checks the deterministic proposal id shape. The
// connection segment is validated as a connection ID and the suffix as 12
// lowercase hex characters, so an id is always safe as a file name.
func ValidateProposalID(field, id string) error {
	if id == "" {
		return NewValidationError(field, "is required")
	}
	if len(id) > proposalIDMaxLength {
		return NewValidationError(field, fmt.Sprintf("must be at most %d bytes", proposalIDMaxLength))
	}
	rest, ok := strings.CutPrefix(id, proposalIDPrefix)
	if !ok {
		return NewValidationError(field, "must start with "+proposalIDPrefix)
	}
	separator := strings.LastIndexByte(rest, '-')
	if separator <= 0 || separator+1+proposalIDDigestLength != len(rest) {
		return NewValidationError(field, "must be proposal-<connection>-<12 hex>")
	}
	if err := ValidateConnectionID(field, rest[:separator]); err != nil {
		return err
	}
	if !isLowerHexString(rest[separator+1:], proposalIDDigestLength) {
		return NewValidationError(field, "must end with 12 lowercase hex characters")
	}
	return nil
}

// EffectiveSchemaVersion returns the explicit schema version or the current
// version when unset, so a hash computed before Put stays valid when Put
// normalizes it.
func (p Proposal) EffectiveSchemaVersion() int {
	if p.SchemaVersion == 0 {
		return ProposalSchemaVersion
	}
	return p.SchemaVersion
}

// StatusOrPending returns the explicit status or pending when unset.
func (p Proposal) StatusOrPending() ProposalStatus {
	if p.Status == "" {
		return ProposalPending
	}
	return p.Status
}

// IsTerminal reports whether the stored status is promoted or dismissed.
func (p Proposal) IsTerminal() bool { return p.Status.Terminal() }

// Validate checks identity, bounds and payload hash consistency. SchemaVersion
// 0 and Status "" are accepted for values that have not been stored yet; the
// store normalizes them before writing. The project-id alphabet is only
// enforced by stores that map the id to a directory (fsstore); server-backed
// stores may keep their own project ids.
func (p Proposal) Validate() error {
	if p.SchemaVersion != 0 && p.SchemaVersion != ProposalSchemaVersion {
		return NewValidationError("proposal.schemaVersion", fmt.Sprintf("unsupported version %d", p.SchemaVersion))
	}
	if err := ValidateRequired("proposal.projectId", p.ProjectID); err != nil {
		return err
	}
	if p.ProjectID != strings.TrimSpace(p.ProjectID) {
		return NewValidationError("proposal.projectId", "must not contain surrounding whitespace")
	}
	if err := ValidateProposalID("proposal.id", p.ID); err != nil {
		return err
	}
	if !p.IDEqualsSource() {
		return NewValidationError("proposal.id", "does not match project, connection and external id")
	}
	if !p.Kind.Valid() {
		return NewValidationError("proposal.kind", fmt.Sprintf("unknown kind %q", p.Kind))
	}
	if err := validateProposalRule(p.Rule); err != nil {
		return err
	}
	if math.IsNaN(p.Confidence) || math.IsInf(p.Confidence, 0) || p.Confidence < 0 || p.Confidence > 1 {
		return NewValidationError("proposal.confidence", "must be between 0 and 1")
	}
	if err := ValidateConnectionID("proposal.connectionId", p.ConnectionID); err != nil {
		return err
	}
	if err := ValidateExternalID("proposal.externalId", p.ExternalID); err != nil {
		return err
	}
	if err := validateLabel("proposal.title", p.Title, maxProposalTitleLength); err != nil {
		return err
	}
	if strings.TrimSpace(p.Title) == "" {
		return NewValidationError("proposal.title", "is required")
	}
	if len(p.Body) > maxProposalBodyBytes {
		return NewValidationError("proposal.body", fmt.Sprintf("must be at most %d bytes", maxProposalBodyBytes))
	}
	if !utf8.ValidString(p.Body) {
		return NewValidationError("proposal.body", "must be valid UTF-8")
	}
	if len(p.Sections) > maxProposalSections {
		return NewValidationError("proposal.sections", fmt.Sprintf("must have at most %d entries", maxProposalSections))
	}
	headings := make(map[string]struct{}, len(p.Sections))
	for _, section := range p.Sections {
		heading := strings.TrimSpace(section.Heading)
		if heading == "" {
			return NewValidationError("proposal.sections.heading", "is required")
		}
		if err := validateLabel("proposal.sections.heading", section.Heading, maxProposalSectionHeadingLength); err != nil {
			return err
		}
		if len(section.Body) > maxProposalBodyBytes || !utf8.ValidString(section.Body) {
			return NewValidationError("proposal.sections.body", fmt.Sprintf("must be valid UTF-8 of at most %d bytes", maxProposalBodyBytes))
		}
		key := strings.ToLower(heading)
		if _, duplicate := headings[key]; duplicate {
			return NewValidationError("proposal.sections", fmt.Sprintf("duplicate heading %q", heading))
		}
		headings[key] = struct{}{}
	}
	if len(p.Citations) > maxProposalCitations {
		return NewValidationError("proposal.citations", fmt.Sprintf("must have at most %d entries", maxProposalCitations))
	}
	for _, citation := range p.Citations {
		if err := citation.Validate(); err != nil {
			return err
		}
	}
	if p.PayloadHash != "" && p.PayloadHash != p.ComputePayloadHash() {
		return NewValidationError("proposal.payloadHash", "does not match content")
	}
	if p.Status != "" && !p.Status.Valid() {
		return NewValidationError("proposal.status", fmt.Sprintf("unknown status %q", p.Status))
	}
	if !p.CreatedAt.IsZero() && !p.UpdatedAt.IsZero() && p.UpdatedAt.Before(p.CreatedAt) {
		return NewValidationError("proposal.updatedAt", "must not be before createdAt")
	}
	if p.PromotedRecordID != "" && p.StatusOrPending() != ProposalPromoted {
		return NewValidationError("proposal.promotedRecordId", "is only valid for a promoted proposal")
	}
	if !p.PromotedAt.IsZero() && p.StatusOrPending() != ProposalPromoted {
		return NewValidationError("proposal.promotedAt", "is only valid for a promoted proposal")
	}
	if p.StatusOrPending() == ProposalPromoted && p.PromotedRecordID == "" {
		return NewValidationError("proposal.promotedRecordId", "is required for a promoted proposal")
	}
	if len(p.PromotedRecordID) > maxProposalRecordIDLength {
		return NewValidationError("proposal.promotedRecordId", fmt.Sprintf("must be at most %d bytes", maxProposalRecordIDLength))
	}
	return nil
}

// IDEqualsSource reports whether ID is the deterministic id of the current
// project, connection and external id. It returns false for a malformed id.
func (p Proposal) IDEqualsSource() bool {
	return p.ID == ProposalID(p.ProjectID, p.ConnectionID, p.ExternalID)
}

// ComputePayloadHash returns "sha256:<hex>" over the stable proposal content:
// schema version, project, id, kind, rule, confidence, connection, external
// id, title, body, ordered sections and citation identity. Status, timestamps
// and promotion fields are excluded, so promoting a proposal never changes its
// hash. ObservedAt on citations is also excluded: re-observing an unchanged
// item must not look like new content.
func (p Proposal) ComputePayloadHash() string {
	var buf bytes.Buffer
	writeField := func(value string) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		buf.Write(size[:])
		buf.WriteString(value)
	}
	writeField("proposal-v1")
	writeField(strconv.Itoa(p.EffectiveSchemaVersion()))
	writeField(p.ProjectID)
	writeField(p.ID)
	writeField(string(p.Kind))
	writeField(strings.TrimSpace(p.Rule))
	// Normalize negative zero: encoding omits a zero confidence, so a stored
	// -0.0 would read back as +0.0 and no longer match a hash computed from
	// the caller's value.
	confidence := p.Confidence
	if confidence == 0 {
		confidence = 0
	}
	writeField(strconv.FormatFloat(confidence, 'g', -1, 64))
	writeField(p.ConnectionID)
	writeField(p.ExternalID)
	writeField(collapseSpace(p.Title))
	writeField(NormalizeMarkdown(p.Body))
	for _, section := range p.Sections {
		writeField(collapseSpace(section.Heading))
		writeField(NormalizeMarkdown(section.Body))
	}
	for _, citation := range p.Citations {
		writeField(citation.ConnectionID)
		writeField(citation.ItemKey)
		writeField(citation.ExternalID)
		writeField(strings.TrimSpace(citation.URL))
		writeField(collapseSpace(citation.Title))
	}
	sum := sha256.Sum256(buf.Bytes())
	return ProposalHashPrefix + hex.EncodeToString(sum[:])
}

// WithPayloadHash returns a copy of p with PayloadHash computed.
func (p Proposal) WithPayloadHash() Proposal {
	p.PayloadHash = p.ComputePayloadHash()
	return p
}

// validateItemKeyConnection checks that an item key belongs to connectionID
// when both are present, so a citation or source can never name one connection
// while pointing at another connection's item.
func validateItemKeyConnection(field, connectionID, itemKey string) error {
	if connectionID == "" || itemKey == "" {
		return nil
	}
	keyConnection, _, _ := strings.Cut(itemKey, "/")
	if keyConnection != connectionID {
		return NewValidationError(field, "belongs to another connection")
	}
	return nil
}

// ValidateItemKey checks "<connectionID>/<40 lowercase hex>" as used by ItemRef.Key.
func ValidateItemKey(field, key string) error {
	connectionID, hexPart, ok := strings.Cut(key, "/")
	if !ok || !isLowerHexString(hexPart, itemKeyHashLength) {
		return NewValidationError(field, "must be <connectionID>/<40 lowercase hex>")
	}
	return ValidateConnectionID(field, connectionID)
}

// ValidateExternalID checks a provider's stable item identifier: non-empty
// valid UTF-8 without NUL, at most maxExternalIDLength bytes.
func ValidateExternalID(field, externalID string) error {
	if externalID == "" {
		return NewValidationError(field, "is required")
	}
	if len(externalID) > maxExternalIDLength {
		return NewValidationError(field, fmt.Sprintf("must be at most %d bytes", maxExternalIDLength))
	}
	if !utf8.ValidString(externalID) || strings.IndexFunc(externalID, unicode.IsControl) >= 0 {
		return NewValidationError(field, "must be valid UTF-8 without control characters")
	}
	return nil
}

// ValidateSourceURL checks an absolute http(s) URL used in a citation or
// source reference.
func ValidateSourceURL(field, raw string) error {
	return validateSourceURL(field, raw, maxProposalCitationURLLength)
}

func validateSourceURL(field, raw string, maxLength int) error {
	if len(raw) > maxLength {
		return NewValidationError(field, fmt.Sprintf("must be at most %d bytes", maxLength))
	}
	if !utf8.ValidString(raw) || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return NewValidationError(field, "must be valid UTF-8 without control characters")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return NewValidationError(field, "must be an absolute http(s) URL")
	}
	if parsed.User != nil {
		return NewValidationError(field, "must not contain credentials")
	}
	return nil
}

func validateProposalRule(rule string) error {
	if rule == "" {
		return NewValidationError("proposal.rule", "is required")
	}
	if rule != strings.TrimSpace(rule) {
		return NewValidationError("proposal.rule", "must not contain surrounding whitespace")
	}
	if len(rule) > maxProposalRuleLength {
		return NewValidationError("proposal.rule", fmt.Sprintf("must be at most %d bytes", maxProposalRuleLength))
	}
	if !utf8.ValidString(rule) {
		return NewValidationError("proposal.rule", "must be valid UTF-8")
	}
	for _, char := range rule {
		allowed := char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9' ||
			char == '-' || char == '_' || char == '.'
		if !allowed {
			return NewValidationError("proposal.rule", fmt.Sprintf("%q must match [a-z0-9._-]+", rule))
		}
	}
	return nil
}

func isLowerHexString(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}
