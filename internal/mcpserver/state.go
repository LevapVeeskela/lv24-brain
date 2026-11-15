package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// State is a project's curated state as returned to clients.
type State struct {
	ProjectID     string    `json:"project_id"`
	Goal          string    `json:"goal"`
	NextAction    string    `json:"next_action"`
	Active        []string  `json:"active"`
	Blocked       []string  `json:"blocked"`
	OpenQuestions []string  `json:"open_questions"`
	UpdatedAt     time.Time `json:"updated_at,omitzero"`
	FreshUntil    time.Time `json:"fresh_until,omitzero"`
	// Revision is the opaque revision of the stored state; empty when the
	// backend does not track one.
	Revision string `json:"revision,omitempty"`
}

// StateInput changes selected state fields; nil keeps the stored value and an
// empty slice or string clears it.
type StateInput struct {
	ProjectID     string
	Goal          *string
	NextAction    *string
	Active        *[]string
	Blocked       *[]string
	OpenQuestions *[]string
	FreshUntil    *time.Time
}

// StateBackend is implemented by backends that can read and update project
// state; the tool refuses with UNSUPPORTED otherwise.
type StateBackend interface {
	GetState(ctx context.Context, projectID string) (State, error)
	SetState(ctx context.Context, in StateInput) (State, error)
}

func (s *Server) stateTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_state",
		Annotations: mutatingAnnotations("Read or replace project state"),
		Description: "Read a project's state (action=get, the default) or update it (action=set). " +
			"On set, only the given fields change; a list replaces the stored list and an empty list clears it. " +
			"expected_revision is compared with the stored state before the write and refuses it with CONFLICT when they differ; the check is best effort, not a transaction.",
		InputSchema: objectSchema(nil, map[string]any{
			"action":            enumProp("get reads (default); set replaces the given fields.", []string{"get", "set"}),
			"project":           project,
			"goal":              stringProp("Project goal (set)."),
			"next":              stringProp("The next action (set)."),
			"active":            stringArrayProp("Active work items (set; replaces the list)."),
			"blocked":           stringArrayProp("Blockers (set; replaces the list)."),
			"open_questions":    stringArrayProp("Open questions (set; replaces the list)."),
			"fresh_for_hours":   intProp("Mark the state current for this many hours; 0 keeps the stored value (set).", 0),
			"expected_revision": stringProp("Revision from a previous get; the set is refused with CONFLICT when it no longer matches."),
		}),
		OutputSchema: stateSchema(),
		ReadActions:  []string{"get"},
		run:          s.state,
	}
}

// stateSchema is the structured output of brain_state.
func stateSchema() map[string]any {
	return outputObject(map[string]any{
		"project_id":     stringProp("Project the state belongs to."),
		"goal":           stringProp("Project goal."),
		"next_action":    stringProp("The next action."),
		"active":         arrayProp("Active work items.", stringProp("Work item.")),
		"blocked":        arrayProp("Blockers.", stringProp("Blocker.")),
		"open_questions": arrayProp("Open questions.", stringProp("Question.")),
		"updated_at":     stringProp("RFC3339 last update."),
		"fresh_until":    stringProp("RFC3339 time the state stops being current."),
		"revision":       stringProp("Opaque revision of the stored state."),
	}, "project_id", "goal", "next_action")
}

func (s *Server) state(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Action           string    `json:"action"`
		Project          string    `json:"project"`
		Goal             *string   `json:"goal"`
		Next             *string   `json:"next"`
		Active           *[]string `json:"active"`
		Blocked          *[]string `json:"blocked"`
		OpenQuestions    *[]string `json:"open_questions"`
		FreshForHours    int       `json:"fresh_for_hours"`
		ExpectedRevision string    `json:"expected_revision"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "get"
	}
	switch action {
	case "get":
		hasSetFields := args.Goal != nil || args.Next != nil || args.Active != nil || args.Blocked != nil ||
			args.OpenQuestions != nil || args.FreshForHours != 0 || strings.TrimSpace(args.ExpectedRevision) != ""
		if hasSetFields {
			return toolResult{}, refusalInvalidWith("action=get only accepts project", "brain_state(action=get) with project, or action=set to update fields")
		}
		return s.stateGet(ctx, strings.TrimSpace(args.Project))
	case "set":
		return s.stateSet(ctx, stateSetArgs{
			Project: args.Project, Goal: args.Goal, Next: args.Next, Active: args.Active, Blocked: args.Blocked,
			OpenQuestions: args.OpenQuestions, FreshForHours: args.FreshForHours, ExpectedRevision: strings.TrimSpace(args.ExpectedRevision),
		})
	default:
		return toolResult{}, refusalInvalidf("unknown action %q (get, set)", action)
	}
}

func (s *Server) stateGet(ctx context.Context, selector string) (toolResult, error) {
	backend, ok := s.backend.(StateBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_state(action=get)", "brain_open, which always carries the project state")
	}
	target, err := s.resolveTarget(ctx, selector)
	if err != nil {
		return toolResult{}, err
	}
	state, err := backend.GetState(ctx, target.ID)
	if err != nil {
		return toolResult{}, err
	}
	return stateResult(state), nil
}

// stateSetArgs is one brain_state(action=set) request.
type stateSetArgs struct {
	Project          string
	Goal             *string
	Next             *string
	Active           *[]string
	Blocked          *[]string
	OpenQuestions    *[]string
	FreshForHours    int
	ExpectedRevision string
}

func (s *Server) stateSet(ctx context.Context, args stateSetArgs) (toolResult, error) {
	backend, ok := s.backend.(StateBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_state(action=set)", "brain state set in a shell")
	}
	if args.FreshForHours < 0 {
		return toolResult{}, refusalInvalid("fresh_for_hours must not be negative")
	}
	in := StateInput{Goal: args.Goal, NextAction: args.Next, Active: args.Active, Blocked: args.Blocked, OpenQuestions: args.OpenQuestions}
	if args.FreshForHours > 0 {
		until := time.Now().Add(time.Duration(args.FreshForHours) * time.Hour)
		in.FreshUntil = &until
	}
	if in.Goal == nil && in.NextAction == nil && in.Active == nil && in.Blocked == nil && in.OpenQuestions == nil && in.FreshUntil == nil {
		return toolResult{}, refusalInvalid("nothing to change; pass at least one field")
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	if err := s.policy.checkWrite(target.ID); err != nil {
		return toolResult{}, err
	}
	if args.ExpectedRevision != "" {
		current, err := backend.GetState(ctx, target.ID)
		if err != nil {
			return toolResult{}, err
		}
		currentRevision := current.Revision
		if currentRevision == "" {
			currentRevision = stateRevision(current)
		}
		if currentRevision != args.ExpectedRevision {
			return toolResult{}, refusalConflict(
				fmt.Sprintf("state of %s changed since revision %s", target.ID, args.ExpectedRevision),
				currentRevision,
				"read the state with brain_state(action=get) and retry the change")
		}
	}
	in.ProjectID = target.ID
	state, err := backend.SetState(ctx, in)
	if err != nil {
		return toolResult{}, err
	}
	return stateResult(state), nil
}

func stateResult(state State) toolResult {
	if state.Revision == "" {
		state.Revision = stateRevision(state)
	}
	text := fmt.Sprintf("project: %s\ngoal: %s\nnext_action: %s\nactive: %s\nblocked: %s\nopen_questions: %s",
		state.ProjectID, orNone(state.Goal), orNone(state.NextAction),
		orNone(strings.Join(state.Active, "; ")), orNone(strings.Join(state.Blocked, "; ")),
		orNone(strings.Join(state.OpenQuestions, "; ")))
	if state.Revision != "" {
		text += "\nrevision: " + state.Revision
	}
	return toolResult{Text: text, Structured: state}
}

// stateRevision derives the opaque revision of a state from its last update.
func stateRevision(state State) string {
	if state.UpdatedAt.IsZero() {
		return "none"
	}
	return state.UpdatedAt.UTC().Format(time.RFC3339Nano)
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}
