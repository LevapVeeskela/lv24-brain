package domain

import "time"

// State is the current working picture for a project.
type State struct {
	ProjectID     string    `json:"projectId"`
	Goal          string    `json:"goal,omitempty"`
	Active        []string  `json:"active,omitempty"`
	Blocked       []string  `json:"blocked,omitempty"`
	NextAction    string    `json:"nextAction,omitempty"`
	OpenQuestions []string  `json:"openQuestions,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
	FreshUntil    time.Time `json:"freshUntil,omitempty"`
	Provenance
}

// Validate checks the state owner.
func (s State) Validate() error {
	if err := ValidateRequired("state.projectId", s.ProjectID); err != nil {
		return err
	}
	return s.Provenance.Validate()
}
