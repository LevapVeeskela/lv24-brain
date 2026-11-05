package app

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// ProjectInput is a registry upsert. Nil fields keep the stored value; a
// non-nil pointer is applied even when it is empty, which clears paths,
// aliases, or text fields. An explicit empty name is rejected; a new project
// without a name input (nil) uses its ID.
type ProjectInput struct {
	ID            string
	Name          *string
	Paths         *[]string
	Aliases       *[]string
	Kind          *domain.ProjectKind
	RepoURL       *string
	DefaultBranch *string
}

// ListProjects returns the project registry sorted by ID.
func (a *App) ListProjects(ctx context.Context) ([]domain.Project, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].ID < projects[j].ID
	})
	return projects, nil
}

// GetProject returns one registry project.
func (a *App) GetProject(ctx context.Context, id string) (domain.Project, error) {
	if err := a.requireStore(); err != nil {
		return domain.Project{}, err
	}
	projectID := strings.TrimSpace(id)
	if projectID == "" {
		return domain.Project{}, domain.NewValidationError("project.id", "is required")
	}
	return a.deps.Store.GetProject(ctx, projectID)
}

// SaveProject upserts a registry project. Omitted fields keep their stored
// value, so callers can update one attribute without resending the rest.
func (a *App) SaveProject(ctx context.Context, input ProjectInput) (domain.Project, error) {
	if err := a.requireStore(); err != nil {
		return domain.Project{}, err
	}
	projectID := strings.TrimSpace(input.ID)
	if projectID == "" {
		return domain.Project{}, domain.NewValidationError("project.id", "is required")
	}

	creating := false
	project, err := a.deps.Store.GetProject(ctx, projectID)
	if errors.Is(err, domain.ErrNotFound) {
		creating = true
		project = domain.Project{ID: projectID, Name: projectID, Kind: domain.ProjectKindOther}
	} else if err != nil {
		return domain.Project{}, err
	}
	if project.ID == "" {
		project.ID = projectID
	}

	if input.Name != nil {
		project.Name = strings.TrimSpace(*input.Name)
	}
	if input.Paths != nil {
		project.Paths = append([]string{}, (*input.Paths)...)
	}
	if input.Aliases != nil {
		project.Aliases = append([]string{}, (*input.Aliases)...)
	}
	if input.Kind != nil {
		project.Kind = *input.Kind
	}
	if input.RepoURL != nil {
		project.RepoURL = strings.TrimSpace(*input.RepoURL)
	}
	if input.DefaultBranch != nil {
		project.DefaultBranch = strings.TrimSpace(*input.DefaultBranch)
	}

	if project.Kind == "" {
		project.Kind = domain.ProjectKindOther
	}
	if input.Name != nil && strings.TrimSpace(project.Name) == "" {
		return domain.Project{}, domain.NewValidationError("project.name", "is required")
	}
	if creating && input.Name == nil {
		project.Name = projectID
	}
	if err := project.Validate(); err != nil {
		return domain.Project{}, err
	}
	if err := a.deps.Store.PutProject(ctx, project); err != nil {
		return domain.Project{}, err
	}
	return a.deps.Store.GetProject(ctx, projectID)
}
