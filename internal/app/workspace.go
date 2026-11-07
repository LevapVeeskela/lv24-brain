package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// WorkspaceError is returned when a directory contains the checkouts of
// several registered projects (a workspace root such as ~/Projects), so no
// single project can be chosen from it.
type WorkspaceError struct {
	Dir      string
	Projects []domain.Project
}

func (e *WorkspaceError) Error() string {
	ids := make([]string, 0, len(e.Projects))
	for _, project := range e.Projects {
		ids = append(ids, project.ID)
	}
	return fmt.Sprintf("%s is a workspace with %d registered projects (%s); pass a project id",
		e.Dir, len(ids), strings.Join(ids, ", "))
}

// Unwrap keeps the error a validation error for exit codes and callers.
func (e *WorkspaceError) Unwrap() error { return domain.ErrValidation }

// AsWorkspace reports whether err is a WorkspaceError.
func AsWorkspace(err error) (*WorkspaceError, bool) {
	var workspace *WorkspaceError
	ok := errors.As(err, &workspace)
	return workspace, ok
}

// WorkspaceOverview renders the projects under a workspace root with their
// goal, next action and latest handoff, so an agent opened at the root can
// pick a project instead of failing.
func (a *App) WorkspaceOverview(ctx context.Context, workspace *WorkspaceError) (string, error) {
	projects := append([]domain.Project(nil), workspace.Projects...)
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	var b strings.Builder
	fmt.Fprintf(&b, "# Company Brain workspace: %s\n\n", workspace.Dir)
	fmt.Fprintf(&b, "%d registered projects live under this directory. Pick one: call brain_open (or `brain open`) with its id, or work inside its checkout.\n\n", len(projects))
	for _, project := range projects {
		_, state, err := a.GetState(ctx, project.ID)
		if err != nil {
			return "", err
		}
		_, _, handoffs, err := a.RecentRecords(ctx, project.ID, 1)
		if err != nil {
			return "", err
		}
		line := fmt.Sprintf("- %s", project.ID)
		if path := firstPath(project); path != "" {
			line += " (" + path + ")"
		}
		if goal := SummarizeLine(state.Goal, 90); goal != "" {
			line += "\n  goal: " + goal
		}
		if next := SummarizeLine(state.NextAction, 90); next != "" {
			line += "\n  next: " + next
		}
		if len(handoffs) > 0 {
			line += "\n  last handoff: " + handoffs[0].CreatedAt.UTC().Format("2006-01-02")
		}
		b.WriteString(line + "\n")
	}
	return b.String(), nil
}
