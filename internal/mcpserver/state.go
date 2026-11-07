package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
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
// state. It is a separate interface so state tools are listed only when the
// backend supports them.
type StateBackend interface {
	GetState(ctx context.Context, projectID string) (State, error)
	SetState(ctx context.Context, in StateInput) (State, error)
}

func (s *Server) stateTools() []tool {
	if _, ok := s.backend.(StateBackend); !ok {
		return nil
	}
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return []tool{
		{
			Name:        "brain_state_get",
			Description: "Read a project's state: goal, next action, active work, blockers and open questions.",
			InputSchema: objectSchema(nil, map[string]any{"project": project}),
			run:         s.stateGet,
		},
		{
			Name: "brain_state_set",
			Description: "Update a project's state so the next session starts from current facts. Only the given fields change; " +
				"a list replaces the stored list and an empty list clears it. Call when the goal or the next step changes.",
			InputSchema: objectSchema(nil, map[string]any{
				"project":         project,
				"goal":            stringProp("Project goal."),
				"next":            stringProp("The next action."),
				"active":          stringArrayProp("Active work items (replaces the list)."),
				"blocked":         stringArrayProp("Blockers (replaces the list)."),
				"open_questions":  stringArrayProp("Open questions (replaces the list)."),
				"fresh_for_hours": intProp("Mark the state current for this many hours; 0 keeps the stored value.", 0),
			}),
			run: s.stateSet,
		},
	}
}

func (s *Server) stateGet(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Project string `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	state, err := s.backend.(StateBackend).GetState(ctx, target.ID)
	if err != nil {
		return toolResult{}, err
	}
	return stateResult(state), nil
}

func (s *Server) stateSet(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Project       string    `json:"project"`
		Goal          *string   `json:"goal"`
		Next          *string   `json:"next"`
		Active        *[]string `json:"active"`
		Blocked       *[]string `json:"blocked"`
		OpenQuestions *[]string `json:"open_questions"`
		FreshForHours int       `json:"fresh_for_hours"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	if args.FreshForHours < 0 {
		return toolResult{}, errors.New("fresh_for_hours must not be negative")
	}
	in := StateInput{Goal: args.Goal, NextAction: args.Next, Active: args.Active, Blocked: args.Blocked, OpenQuestions: args.OpenQuestions}
	if args.FreshForHours > 0 {
		until := time.Now().Add(time.Duration(args.FreshForHours) * time.Hour)
		in.FreshUntil = &until
	}
	if in.Goal == nil && in.NextAction == nil && in.Active == nil && in.Blocked == nil && in.OpenQuestions == nil && in.FreshUntil == nil {
		return toolResult{}, errors.New("nothing to change; pass at least one field")
	}
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	in.ProjectID = target.ID
	state, err := s.backend.(StateBackend).SetState(ctx, in)
	if err != nil {
		return toolResult{}, err
	}
	return stateResult(state), nil
}

func stateResult(state State) toolResult {
	text := fmt.Sprintf("project: %s\ngoal: %s\nnext_action: %s\nactive: %s\nblocked: %s\nopen_questions: %s",
		state.ProjectID, orNone(state.Goal), orNone(state.NextAction),
		orNone(strings.Join(state.Active, "; ")), orNone(strings.Join(state.Blocked, "; ")),
		orNone(strings.Join(state.OpenQuestions, "; ")))
	return toolResult{Text: text, Structured: state}
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}
