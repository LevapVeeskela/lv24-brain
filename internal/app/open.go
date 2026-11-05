package app

import (
	"context"
	"errors"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Open loads the active project context. A project without state.md opens with
// an empty state instead of failing, because legacy data may predate state.
func (a *App) Open(ctx context.Context, opts OpenOptions) (OpenResult, error) {
	if err := a.requireStore(); err != nil {
		return OpenResult{}, err
	}
	project, err := a.resolveProject(ctx, opts.ProjectID)
	if err != nil {
		return OpenResult{}, err
	}
	state, err := a.deps.Store.GetState(ctx, project.ID)
	if errors.Is(err, domain.ErrNotFound) {
		state = domain.State{ProjectID: project.ID}
	} else if err != nil {
		return OpenResult{}, err
	}
	return OpenResult{Project: project, State: state}, nil
}
