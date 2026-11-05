package domain

import (
	"fmt"
	"time"
)

// ProjectKind classifies a tracked project.
type ProjectKind string

const (
	ProjectKindApp        ProjectKind = "app"
	ProjectKindService    ProjectKind = "service"
	ProjectKindRepository ProjectKind = "repository"
	ProjectKindInfra      ProjectKind = "infra"
	ProjectKindOther      ProjectKind = "other"
)

func (k ProjectKind) Valid() bool {
	switch k {
	case ProjectKindApp, ProjectKindService, ProjectKindRepository, ProjectKindInfra, ProjectKindOther:
		return true
	default:
		return false
	}
}

// Project is a stable brain project with aliases and repository metadata.
type Project struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Paths         []string    `json:"paths,omitempty"`
	Aliases       []string    `json:"aliases,omitempty"`
	Kind          ProjectKind `json:"kind"`
	RepoURL       string      `json:"repoUrl,omitempty"`
	DefaultBranch string      `json:"defaultBranch,omitempty"`
	CreatedAt     time.Time   `json:"createdAt"`
	UpdatedAt     time.Time   `json:"updatedAt"`
}

// Validate checks required project fields.
func (p Project) Validate() error {
	if err := ValidateRequired("project.id", p.ID); err != nil {
		return err
	}
	if err := ValidateRequired("project.name", p.Name); err != nil {
		return err
	}
	if p.Kind != "" && !p.Kind.Valid() {
		return NewValidationError("project.kind", fmt.Sprintf("unknown kind %q", p.Kind))
	}
	return nil
}
