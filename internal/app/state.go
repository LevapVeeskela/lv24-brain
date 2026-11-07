package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// JournalState is the journal event for a state update.
const JournalState = "state"

// StateInput changes selected fields of a project's state. Nil fields keep the
// stored value; an empty slice or string clears the field.
type StateInput struct {
	ProjectID     string
	Goal          *string
	Active        *[]string
	Blocked       *[]string
	NextAction    *string
	OpenQuestions *[]string
	// FreshUntil marks until when the state is considered current; a zero
	// time clears it.
	FreshUntil *time.Time
}

// Empty reports whether the input changes nothing.
func (in StateInput) Empty() bool {
	return in.Goal == nil && in.Active == nil && in.Blocked == nil && in.NextAction == nil &&
		in.OpenQuestions == nil && in.FreshUntil == nil
}

// GetState returns a project's state; a project without state.md yields an
// empty state, like Open.
func (a *App) GetState(ctx context.Context, projectID string) (domain.Project, domain.State, error) {
	result, err := a.Open(ctx, OpenOptions{ProjectID: projectID})
	if err != nil {
		return domain.Project{}, domain.State{}, err
	}
	return result.Project, result.State, nil
}

// SetState merges input into the stored state of an existing project, writes
// it, refreshes the search index and records a journal line. It never
// creates a project.
func (a *App) SetState(ctx context.Context, input StateInput) (domain.State, error) {
	if err := a.requireStore(); err != nil {
		return domain.State{}, err
	}
	if input.Empty() {
		return domain.State{}, domain.NewValidationError("state", "nothing to change; pass at least one field")
	}
	project, err := a.ensureProject(ctx, input.ProjectID, false)
	if err != nil {
		return domain.State{}, err
	}
	state, err := a.deps.Store.GetState(ctx, project.ID)
	if errors.Is(err, domain.ErrNotFound) {
		state = domain.State{ProjectID: project.ID}
	} else if err != nil {
		return domain.State{}, err
	}
	changed := make([]string, 0, 6)
	if input.Goal != nil {
		state.Goal = strings.TrimSpace(*input.Goal)
		changed = append(changed, "goal")
	}
	if input.NextAction != nil {
		state.NextAction = strings.TrimSpace(*input.NextAction)
		changed = append(changed, "next_action")
	}
	if input.Active != nil {
		state.Active = cleanItems(*input.Active)
		changed = append(changed, "active")
	}
	if input.Blocked != nil {
		state.Blocked = cleanItems(*input.Blocked)
		changed = append(changed, "blocked")
	}
	if input.OpenQuestions != nil {
		state.OpenQuestions = cleanItems(*input.OpenQuestions)
		changed = append(changed, "open_questions")
	}
	if input.FreshUntil != nil {
		state.FreshUntil = input.FreshUntil.UTC()
		changed = append(changed, "fresh_until")
	}
	state.ProjectID = project.ID
	state.UpdatedAt = a.now()
	if err := state.Validate(); err != nil {
		return domain.State{}, err
	}
	if _, err := a.deps.Store.PutState(ctx, state); err != nil {
		return domain.State{}, err
	}
	journalErr := a.journalWrite(ctx, JournalState, project.ID, "", "changed: "+strings.Join(changed, ", "))
	if err := a.upsertIndex(ctx, stateDocument(project, state)); err != nil {
		return state, fmt.Errorf("state of %s was written but the search index update failed: %w", project.ID, err)
	}
	if journalErr != nil {
		return state, fmt.Errorf("state of %s was written but the journal append failed: %w", project.ID, journalErr)
	}
	return state, nil
}

func cleanItems(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
