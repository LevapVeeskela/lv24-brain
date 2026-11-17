package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
	// A retired project name never (re)enters the registry either.
	if creating {
		if entry, retired := retiredPathBySlug(a.cfg, projectID); retired {
			return domain.Project{}, domain.NewValidationError("project.id",
				fmt.Sprintf("%q is a retired project name (replaced by %s); use that id instead", projectID, entry.ReplacedBy))
		}
	}
	// A retired checkout never (re)enters the registry; a project that already
	// stored one keeps it, so unrelated edits are not blocked.
	if input.Paths != nil {
		stored := make(map[string]bool, len(project.Paths))
		for _, path := range project.Paths {
			stored[normalizePath(path)] = true
		}
		for _, path := range *input.Paths {
			clean := cleanPath(path)
			entry, retired := RetiredPathMatch(a.cfg, clean)
			if !retired || stored[normalizePath(clean)] {
				continue
			}
			return domain.Project{}, domain.NewValidationError("project.paths",
				fmt.Sprintf("%q is a retired checkout (replaced by %s); register the replacement project instead", clean, entry.ReplacedBy))
		}
	}

	if input.Name != nil {
		project.Name = strings.TrimSpace(*input.Name)
	}
	if input.Paths != nil {
		project.Paths = append([]string{}, (*input.Paths)...)
		project.PathProvenance = stampPathProvenance(project.PathProvenance, project.Paths, a.cfg.EffectiveMachineID())
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

// stampPathProvenance keeps the recorded owner of every path that stays
// registered and stamps a newly added path with the current machine id, so a
// shared registry says which machine contributed which checkout. Provenance is
// optional: without a machine id and without stored entries there is none.
func stampPathProvenance(existing map[string]string, paths []string, machine string) map[string]string {
	if len(existing) == 0 && strings.TrimSpace(machine) == "" {
		return nil
	}
	out := make(map[string]string, len(paths))
	for _, path := range paths {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			continue
		}
		if owner, ok := lookupPathProvenance(existing, trimmed); ok {
			out[trimmed] = owner
			continue
		}
		if strings.TrimSpace(machine) != "" {
			out[trimmed] = strings.TrimSpace(machine)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// lookupPathProvenance finds the recorded owner of path, tolerating a stored
// key that differs only in case or slash direction (paths travel between
// Windows and macOS checkouts).
func lookupPathProvenance(existing map[string]string, path string) (string, bool) {
	if owner, ok := existing[path]; ok {
		return owner, true
	}
	want := provenanceKey(path)
	for key, owner := range existing {
		if provenanceKey(key) == want {
			return owner, true
		}
	}
	return "", false
}

func provenanceKey(path string) string {
	return strings.ToLower(strings.TrimSuffix(filepath.ToSlash(strings.TrimSpace(path)), "/"))
}
