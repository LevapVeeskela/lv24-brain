package domain

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// SourceRef records where a curated record came from. It is optional: legacy
// and hand-written records have no source. A promoted proposal references its
// proposal id (and usually the raw item that produced it).
type SourceRef struct {
	ProjectID    string    `json:"projectId,omitempty"`
	ConnectionID string    `json:"connectionId,omitempty"`
	ItemKey      string    `json:"itemKey,omitempty"`
	ExternalID   string    `json:"externalId,omitempty"`
	URL          string    `json:"url,omitempty"`
	ObservedAt   time.Time `json:"observedAt,omitzero"`
	ProposalID   string    `json:"proposalId,omitempty"`
	Confidence   float64   `json:"confidence,omitempty"`
}

// Validate checks the source fields that are present. A source must identify
// at least one item, proposal or URL; a connection id alone does not name
// provenance.
func (s SourceRef) Validate() error {
	if s.ProjectID != "" {
		if s.ProjectID != strings.TrimSpace(s.ProjectID) {
			return NewValidationError("source.projectId", "must not contain surrounding whitespace")
		}
		if len(s.ProjectID) > 100 || !utf8.ValidString(s.ProjectID) {
			return NewValidationError("source.projectId", "must be valid UTF-8 of at most 100 bytes")
		}
	}
	if s.ConnectionID != "" {
		if err := ValidateConnectionID("source.connectionId", s.ConnectionID); err != nil {
			return err
		}
	}
	if s.ItemKey != "" {
		if err := ValidateItemKey("source.itemKey", s.ItemKey); err != nil {
			return err
		}
		if err := validateItemKeyConnection("source.itemKey", s.ConnectionID, s.ItemKey); err != nil {
			return err
		}
	}
	if s.ExternalID != "" {
		if err := ValidateExternalID("source.externalId", s.ExternalID); err != nil {
			return err
		}
	}
	if s.URL != "" {
		if err := ValidateSourceURL("source.url", s.URL); err != nil {
			return err
		}
	}
	if s.ProposalID != "" {
		if err := ValidateProposalID("source.proposalId", s.ProposalID); err != nil {
			return err
		}
	}
	if math.IsNaN(s.Confidence) || math.IsInf(s.Confidence, 0) || s.Confidence < 0 || s.Confidence > 1 {
		return NewValidationError("source.confidence", "must be between 0 and 1")
	}
	if s.ProposalID == "" && s.ExternalID == "" && s.ItemKey == "" && s.URL == "" {
		return NewValidationError("source", "needs a proposal, item key, external id or URL")
	}
	return nil
}

func validateRecordSource(field, projectID string, source *SourceRef) error {
	if source == nil {
		return nil
	}
	if source.ProjectID != "" && source.ProjectID != projectID {
		return NewValidationError(field+".projectId", fmt.Sprintf("is %q, record is %q", source.ProjectID, projectID))
	}
	return source.Validate()
}

// Note is a dated Markdown section attached to a project.
type Note struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"projectId"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Tags      []string   `json:"tags,omitempty"`
	Permalink string     `json:"permalink,omitempty"`
	Source    *SourceRef `json:"source,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// Validate checks required note fields.
func (n Note) Validate() error {
	if err := ValidateRequired("note.projectId", n.ProjectID); err != nil {
		return err
	}
	if err := ValidateRequired("note.body", n.Body); err != nil {
		return err
	}
	return validateRecordSource("note.source", n.ProjectID, n.Source)
}

// DecisionStatus is the lifecycle state of a decision.
type DecisionStatus string

const (
	DecisionProposed   DecisionStatus = "proposed"
	DecisionAccepted   DecisionStatus = "accepted"
	DecisionRejected   DecisionStatus = "rejected"
	DecisionSuperseded DecisionStatus = "superseded"
)

func (s DecisionStatus) Valid() bool {
	switch s {
	case DecisionProposed, DecisionAccepted, DecisionRejected, DecisionSuperseded:
		return true
	default:
		return false
	}
}

// Decision records a choice with context, alternatives and consequences.
type Decision struct {
	ID           string         `json:"id"`
	ProjectID    string         `json:"projectId"`
	Title        string         `json:"title"`
	Status       DecisionStatus `json:"status"`
	Context      string         `json:"context,omitempty"`
	Alternatives []string       `json:"alternatives,omitempty"`
	Consequences []string       `json:"consequences,omitempty"`
	Evidence     []string       `json:"evidence,omitempty"`
	// Paths are repo-relative files or directories the decision governs.
	Paths     []string   `json:"paths,omitempty"`
	Permalink string     `json:"permalink,omitempty"`
	Source    *SourceRef `json:"source,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// NormalizePaths trims each path, converts backslashes to forward slashes and
// drops empty entries. It rejects absolute paths (POSIX or drive-letter) and
// any ".." segment, so a decision can only govern files inside its checkout.
func NormalizePaths(paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		clean := strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/")
		if clean == "" {
			continue
		}
		if strings.HasPrefix(clean, "/") || (len(clean) >= 2 && clean[1] == ':') {
			return nil, NewValidationError("decision.paths", fmt.Sprintf("path %q must be relative to the project checkout", raw))
		}
		for _, segment := range strings.Split(clean, "/") {
			if segment == ".." {
				return nil, NewValidationError("decision.paths", fmt.Sprintf("path %q must not contain \"..\"", raw))
			}
		}
		out = append(out, clean)
	}
	return out, nil
}

// Validate checks required decision fields.
func (d Decision) Validate() error {
	if err := ValidateRequired("decision.projectId", d.ProjectID); err != nil {
		return err
	}
	if err := ValidateRequired("decision.title", d.Title); err != nil {
		return err
	}
	if d.Status != "" && !d.Status.Valid() {
		return NewValidationError("decision.status", fmt.Sprintf("unknown status %q", d.Status))
	}
	if _, err := NormalizePaths(d.Paths); err != nil {
		return err
	}
	return validateRecordSource("decision.source", d.ProjectID, d.Source)
}

// Attempt records a hypothesis and why it did or did not work.
type Attempt struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"projectId"`
	Title      string     `json:"title"`
	Hypothesis string     `json:"hypothesis,omitempty"`
	Result     string     `json:"result,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Permalink  string     `json:"permalink,omitempty"`
	Source     *SourceRef `json:"source,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// Validate checks required attempt fields.
func (a Attempt) Validate() error {
	if err := ValidateRequired("attempt.projectId", a.ProjectID); err != nil {
		return err
	}
	if err := ValidateRequired("attempt.title", a.Title); err != nil {
		return err
	}
	return validateRecordSource("attempt.source", a.ProjectID, a.Source)
}

// Handoff captures session context for the next person or agent.
type Handoff struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"projectId"`
	Summary    string    `json:"summary"`
	GitContext string    `json:"gitContext,omitempty"`
	NextAction string    `json:"nextAction,omitempty"`
	Permalink  string    `json:"permalink,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// Validate checks required handoff fields.
func (h Handoff) Validate() error {
	if err := ValidateRequired("handoff.projectId", h.ProjectID); err != nil {
		return err
	}
	return ValidateRequired("handoff.summary", h.Summary)
}
