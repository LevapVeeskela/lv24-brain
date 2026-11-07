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
	} else if !looksLikePath(target) {
		return Resolved{}, domain.NewValidationError("project",
			"\""+selector+"\" is not a registered project id or alias (pass a path with a separator, e.g. ./dir, to select by path)")
	}
	full := normalizePath(target)

	var best domain.Project
	bestPath := ""
	for _, project := range projects {
		for _, raw := range LocalPaths(project.Paths) {
			candidate := normalizePath(raw)
			if !pathWithin(full, candidate) {
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
		for _, raw := range LocalPaths(project.Paths) {
			candidate := normalizePath(raw)
			if candidate != full && pathWithin(candidate, full) {
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

// firstPath returns the first registered path that exists on this machine
// (projects list one checkout per machine, e.g. a Windows and a macOS path),
// falling back to the first non-empty path.
func firstPath(project domain.Project) string {
	fallback := ""
	for _, path := range LocalPaths(project.Paths) {
		clean := cleanPath(path)
		if info, err := os.Stat(clean); err == nil && info.IsDir() {
			return clean
		}
		if fallback == "" {
			fallback = clean
		}
	}
	return fallback
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

// LocalPaths keeps the registered paths that are absolute on this operating
// system. A project lists one checkout per machine, so a Windows path
// ("C:\\0project\\x") is not absolute on macOS and a macOS path ("/Users/x")
// is not absolute on Windows; treating them as relative would join them to
// the working directory and produce bogus matches.
func LocalPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path != "" && filepath.IsAbs(filepath.FromSlash(path)) {
			out = append(out, path)
		}
	}
	return out
}

// looksLikePath reports whether a selector should be treated as a filesystem
// path: absolute, relative with a separator, or starting with ".". A bare word
// such as a mistyped project id is not a path, so it cannot silently resolve
// to the working directory's project.
func looksLikePath(selector string) bool {
	if filepath.IsAbs(selector) || strings.HasPrefix(selector, ".") || strings.HasPrefix(selector, "~") {
		return true
	}
	return strings.ContainsAny(selector, `/\`)
}
