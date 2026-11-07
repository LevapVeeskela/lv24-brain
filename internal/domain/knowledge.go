package domain

import (
	"fmt"
	"strings"
	"time"
)

// Note is a dated Markdown section attached to a project.
type Note struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Tags      []string  `json:"tags,omitempty"`
	Permalink string    `json:"permalink,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Validate checks required note fields.
func (n Note) Validate() error {
	if err := ValidateRequired("note.projectId", n.ProjectID); err != nil {
		return err
	}
	return ValidateRequired("note.body", n.Body)
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
	Paths     []string  `json:"paths,omitempty"`
	Permalink string    `json:"permalink,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
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
	_, err := NormalizePaths(d.Paths)
	return err
}

// Attempt records a hypothesis and why it did or did not work.
type Attempt struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"projectId"`
	Title      string    `json:"title"`
	Hypothesis string    `json:"hypothesis,omitempty"`
	Result     string    `json:"result,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Permalink  string    `json:"permalink,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Validate checks required attempt fields.
func (a Attempt) Validate() error {
	if err := ValidateRequired("attempt.projectId", a.ProjectID); err != nil {
		return err
	}
	return ValidateRequired("attempt.title", a.Title)
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
