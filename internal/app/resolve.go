package app

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Resolved is a project selected by id, alias or filesystem path.
type Resolved struct {
	Project domain.Project
	// Path is the project checkout the selector resolved through (or the
	// project's first registered path for an id/alias match); "" if none.
	Path string
}

// ResolveSelector maps a selector to a registered project, matching the
// legacy brain.ps1 rules: an exact id or alias first; otherwise the selector
// is a path ("" means the working directory) and the project with the longest
// registered path containing it wins; otherwise, if the path is a parent of
// exactly one registered project, that project is used.
func (a *App) ResolveSelector(ctx context.Context, selector string) (Resolved, error) {
	if err := a.requireStore(); err != nil {
		return Resolved{}, err
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return Resolved{}, err
	}
	selector = strings.TrimSpace(selector)
	if selector != "" && selector != "." {
		for _, project := range projects {
			if strings.EqualFold(project.ID, selector) || containsFold(project.Aliases, selector) {
				return Resolved{Project: project, Path: firstPath(project)}, nil
			}
		}
	}
	target := selector
	if target == "" || target == "." {
		wd, err := os.Getwd()
		if err != nil {
			return Resolved{}, err
		}
		target = wd
	}
	full := normalizePath(target)

	var best domain.Project
	bestPath := ""
	for _, project := range projects {
		for _, raw := range project.Paths {
			candidate := normalizePath(raw)
			if raw == "" || !pathWithin(full, candidate) {
				continue
			}
			if len(candidate) > len(bestPath) {
				best, bestPath = project, cleanPath(raw)
			}
		}
	}
	if bestPath != "" {
		return Resolved{Project: best, Path: bestPath}, nil
	}

	var children []domain.Project
	for _, project := range projects {
		for _, raw := range project.Paths {
			candidate := normalizePath(raw)
			if raw != "" && candidate != full && pathWithin(candidate, full) {
				children = append(children, project)
				break
			}
		}
	}
	switch len(children) {
	case 1:
		return Resolved{Project: children[0], Path: firstPath(children[0])}, nil
	case 0:
		return Resolved{}, domain.NewValidationError("project",
			"\""+selector+"\" is not a registered project id or path; register it with: brain project set <id> --path <dir>")
	default:
		return Resolved{}, domain.NewValidationError("project",
			"\""+selector+"\" is a parent of several registered projects; pass a project id")
	}
}

func firstPath(project domain.Project) string {
	for _, path := range project.Paths {
		if strings.TrimSpace(path) != "" {
			return cleanPath(path)
		}
	}
	return ""
}

func cleanPath(path string) string {
	if abs, err := filepath.Abs(strings.TrimSpace(path)); err == nil {
		return abs
	}
	return filepath.Clean(strings.TrimSpace(path))
}

// normalizePath is the comparison key: absolute, cleaned, and case-folded on
// Windows and macOS, whose default filesystems are case-insensitive.
func normalizePath(path string) string {
	clean := cleanPath(filepath.FromSlash(path))
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		clean = strings.ToLower(clean)
	}
	return clean
}

// pathWithin reports whether child equals parent or lies under it.
func pathWithin(child, parent string) bool {
	if child == parent {
		return true
	}
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}
