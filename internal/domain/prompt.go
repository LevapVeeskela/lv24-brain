package domain

import "time"

// Prompt is one captured user prompt from the opt-in prompt zone. Prompts are
// machine-local: they live outside the curated record tree and are never
// indexed or published.
type Prompt struct {
	ProjectID string    `json:"projectId"`
	At        time.Time `json:"at"`
	Session   string    `json:"session,omitempty"`
	Text      string    `json:"text"`
	// Path is the data-directory path of the file the prompt is stored in,
	// with forward slashes. It is informational.
	Path string `json:"path,omitempty"`
}
