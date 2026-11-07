package fsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	registryFile    = "registry.json"
	registryVersion = 1
)

// projectRegistry is the root registry.json project index, compatible with the
// legacy brain data layout {"version":1,"projects":[...]}.
type projectRegistry struct {
	Version  int               `json:"version"`
	Projects []registryProject `json:"projects"`
}

type registryProject struct {
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Paths         []string           `json:"paths,omitempty"`
	Aliases       []string           `json:"aliases,omitempty"`
	Kind          domain.ProjectKind `json:"kind"`
	RepoURL       string             `json:"repo_url,omitempty"`
	DefaultBranch string             `json:"default_branch,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
}

// PutProject inserts or replaces one project in the root registry.json. The
// original CreatedAt is preserved on updates.
func (s *Store) PutProject(ctx context.Context, project domain.Project) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	project, err := normalizeProject(project)
	if err != nil {
		return err
	}
	if _, err := s.projectDir(project.ID); err != nil {
		return err
	}
	// registry.json is read, updated and rewritten as one step under its lock, so
	// concurrent PutProject calls cannot drop each other's project.
	registryPath := filepath.Join(s.root, registryFile)
	return withFileLock(ctx, registryPath, func() error {
		registry, err := s.readRegistry()
		if err != nil {
			return err
		}

		index := -1
		for i := range registry.Projects {
			if registry.Projects[i].ID == project.ID {
				index = i
				break
			}
		}
		now := s.nowUTC()
		if index >= 0 && !registry.Projects[index].CreatedAt.IsZero() {
			project.CreatedAt = registry.Projects[index].CreatedAt.UTC()
		} else if project.CreatedAt.IsZero() {
			project.CreatedAt = now
		} else {
			project.CreatedAt = project.CreatedAt.UTC()
		}
		project.UpdatedAt = now
		record := registryProjectFromDomain(project)
		if index >= 0 {
			registry.Projects[index] = record
		} else {
			registry.Projects = append(registry.Projects, record)
		}
		if err := s.writeRegistry(registry); err != nil {
			return fmt.Errorf("fsstore: write project %q: %w", project.ID, err)
		}
		return nil
	})
}

// GetProject loads one project from registry.json.
func (s *Store) GetProject(ctx context.Context, id string) (domain.Project, error) {
	if err := contextError(ctx); err != nil {
		return domain.Project{}, err
	}
	return s.loadProject(id)
}

// ListProjects returns registry projects ordered like the server: lowercased
// name, then id.
func (s *Store) ListProjects(ctx context.Context) ([]domain.Project, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	registry, err := s.readRegistry()
	if err != nil {
		return nil, err
	}
	projects := make([]domain.Project, 0, len(registry.Projects))
	for _, record := range registry.Projects {
		project := record.project()
		if project.Name == "" {
			project.Name = project.ID
		}
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool {
		left := strings.ToLower(projects[i].Name)
		right := strings.ToLower(projects[j].Name)
		if left != right {
			return left < right
		}
		return projects[i].ID < projects[j].ID
	})
	return projects, nil
}

func (s *Store) loadProject(projectID string) (domain.Project, error) {
	if err := validateProjectID(projectID); err != nil {
		return domain.Project{}, err
	}
	registry, err := s.readRegistry()
	if err != nil {
		return domain.Project{}, err
	}
	for _, record := range registry.Projects {
		if record.ID == projectID {
			project := record.project()
			if project.Name == "" {
				project.Name = project.ID
			}
			return project, nil
		}
	}
	return domain.Project{}, notFound("project", projectID)
}

func (s *Store) requireProject(projectID string) error {
	_, err := s.loadProject(projectID)
	return err
}

func (s *Store) readRegistry() (projectRegistry, error) {
	path := filepath.Join(s.root, registryFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return projectRegistry{Version: registryVersion, Projects: []registryProject{}}, nil
		}
		return projectRegistry{}, fmt.Errorf("fsstore: read registry: %w", err)
	}
	var registry projectRegistry
	if err := json.Unmarshal(raw, &registry); err != nil {
		return projectRegistry{}, fmt.Errorf("fsstore: parse registry: %w", err)
	}
	if registry.Version > registryVersion {
		return projectRegistry{}, domain.NewValidationError(
			"registry.version",
			fmt.Sprintf("data schema %d requires a newer engine (this engine supports %d)", registry.Version, registryVersion),
		)
	}
	if registry.Projects == nil {
		registry.Projects = []registryProject{}
	}
	return registry, nil
}

func (s *Store) writeRegistry(registry projectRegistry) error {
	registry.Version = registryVersion
	if registry.Projects == nil {
		registry.Projects = []registryProject{}
	}
	sort.Slice(registry.Projects, func(i, j int) bool {
		left := strings.ToLower(registry.Projects[i].Name)
		right := strings.ToLower(registry.Projects[j].Name)
		if left != right {
			return left < right
		}
		return registry.Projects[i].ID < registry.Projects[j].ID
	})
	raw, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return fmt.Errorf("fsstore: encode registry: %w", err)
	}
	raw = append(raw, '\n')
	if err := writeFileAtomic(filepath.Join(s.root, registryFile), raw); err != nil {
		return fmt.Errorf("fsstore: write registry: %w", err)
	}
	return nil
}

func normalizeProject(project domain.Project) (domain.Project, error) {
	project.ID = strings.TrimSpace(project.ID)
	project.Name = strings.TrimSpace(project.Name)
	if project.Kind == "" {
		project.Kind = domain.ProjectKindOther
	}
	if project.Paths == nil {
		project.Paths = []string{}
	}
	if project.Aliases == nil {
		project.Aliases = []string{}
	}
	if err := project.Validate(); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

func registryProjectFromDomain(project domain.Project) registryProject {
	return registryProject{
		ID:            project.ID,
		Name:          project.Name,
		Paths:         nonNilStrings(project.Paths),
		Aliases:       nonNilStrings(project.Aliases),
		Kind:          project.Kind,
		RepoURL:       project.RepoURL,
		DefaultBranch: project.DefaultBranch,
		CreatedAt:     project.CreatedAt.UTC(),
		UpdatedAt:     project.UpdatedAt.UTC(),
	}
}

func (record registryProject) project() domain.Project {
	kind := record.Kind
	if kind == "" {
		kind = domain.ProjectKindOther
	}
	if !kind.Valid() {
		kind = domain.ProjectKindOther
	}
	return domain.Project{
		ID:            record.ID,
		Name:          record.Name,
		Paths:         nonNilStrings(record.Paths),
		Aliases:       nonNilStrings(record.Aliases),
		Kind:          kind,
		RepoURL:       record.RepoURL,
		DefaultBranch: record.DefaultBranch,
		CreatedAt:     record.CreatedAt.UTC(),
		UpdatedAt:     record.UpdatedAt.UTC(),
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
