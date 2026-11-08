package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ContextInput is one brain_context request after argument validation.
type ContextInput struct {
	ProjectID string
	Query     string
	// Budget bounds the summary in runes; zero selects the default.
	Budget int
	// Kinds keeps only these record kinds (decision, attempt, note).
	Kinds []string
	// Since is a window (7d, 24h) or a date (2026-11-01); empty keeps all.
	Since string
}

// TaskContext is the context of a project for one task. The CLI prints the
// same shape for `brain context --json`.
type TaskContext struct {
	ProjectID      string          `json:"project_id"`
	ProjectName    string          `json:"project_name"`
	Mode           string          `json:"mode"`
	Query          string          `json:"query"`
	Since          time.Time       `json:"since,omitzero"`
	Budget         int             `json:"budget"`
	State          State           `json:"state"`
	Handoff        *ContextHandoff `json:"handoff"`
	Records        []ContextRecord `json:"records"`
	FailedAttempts []ContextRecord `json:"failed_attempts"`
	Findings       []ReviewFinding `json:"findings"`
	Skipped        []string        `json:"skipped"`
	Summary        string          `json:"summary"`
}

// ContextHandoff is the newest handoff of the project.
type ContextHandoff struct {
	ID         string    `json:"id"`
	Summary    string    `json:"summary"`
	NextAction string    `json:"next_action"`
	CreatedAt  time.Time `json:"created_at,omitzero"`
}

// ContextRecord is one ranked record relevant to the task.
type ContextRecord struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Status    string    `json:"status,omitempty"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	Score     float64   `json:"score"`
}

// ContextBackend is implemented by backends that can build task context.
// brain_context is listed only for such backends.
type ContextBackend interface {
	Context(ctx context.Context, in ContextInput) (TaskContext, error)
}

func (s *Server) contextTools() []tool {
	if _, ok := s.backend.(ContextBackend); !ok {
		return nil
	}
	return []tool{{
		Name: "brain_context",
		Description: "Load the context relevant to one task in a single call: project state, review findings, the latest handoff, " +
			"the decisions, attempts and notes that best match the task, and failed attempts to avoid repeating. " +
			"Call before starting a task; brain_open stays the general session start.",
		InputSchema: objectSchema([]string{"query"}, map[string]any{
			"query":   stringProp("The task in a few words, for example \"add retry to the sync push\"."),
			"project": stringProp("Project id, alias or checkout path. Omit to use the project of the working directory."),
			"budget":  intProp("Summary size budget in runes; 0 selects the default.", 0),
			"kinds":   stringArrayProp("Keep only these record kinds: decision, attempt, note. Omit for all three."),
			"since":   stringProp("Drop records and handoffs older than a window (24h, 7d, 2w) or a date (2026-11-01)."),
		}),
		run: s.taskContext,
	}}
}

func (s *Server) taskContext(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Query   string   `json:"query"`
		Project string   `json:"project"`
		Budget  int      `json:"budget"`
		Kinds   []string `json:"kinds"`
		Since   string   `json:"since"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	query, err := requireText("query", args.Query)
	if err != nil {
		return toolResult{}, err
	}
	if args.Budget < 0 {
		return toolResult{}, errors.New("budget must not be negative")
	}
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	result, err := s.backend.(ContextBackend).Context(ctx, ContextInput{
		ProjectID: target.ID,
		Query:     query,
		Budget:    args.Budget,
		Kinds:     args.Kinds,
		Since:     strings.TrimSpace(args.Since),
	})
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{Text: result.Summary, Structured: result.WithEmptyLists()}, nil
}

// WithEmptyLists keeps empty lists as [] rather than null in JSON.
func (c TaskContext) WithEmptyLists() TaskContext {
	if c.Records == nil {
		c.Records = []ContextRecord{}
	}
	if c.FailedAttempts == nil {
		c.FailedAttempts = []ContextRecord{}
	}
	if c.Findings == nil {
		c.Findings = []ReviewFinding{}
	}
	if c.Skipped == nil {
		c.Skipped = []string{}
	}
	c.State = c.State.withEmptyLists()
	return c
}

func (st State) withEmptyLists() State {
	for _, list := range []*[]string{&st.Active, &st.Blocked, &st.OpenQuestions} {
		if *list == nil {
			*list = []string{}
		}
	}
	return st
}
