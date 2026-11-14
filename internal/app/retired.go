package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// RetiredPathMatch reports whether path is a retired checkout: it equals a
// configured retired path or lies under it. Only paths absolute on this
// operating system can match, so one config shared by Windows and macOS does
// not redirect the other machine. Nothing is created or deleted here.
func RetiredPathMatch(cfg config.BrainConfig, path string) (config.RetiredPath, bool) {
	target := strings.TrimSpace(path)
	if target == "" || !filepath.IsAbs(filepath.FromSlash(target)) {
		return config.RetiredPath{}, false
	}
	full := normalizePath(target)
	for _, entry := range cfg.EffectiveRetiredPaths() {
		retired := strings.TrimSpace(entry.Path)
		if retired == "" || !filepath.IsAbs(filepath.FromSlash(retired)) {
			continue
		}
		if pathWithin(full, normalizePath(retired)) {
			return entry, true
		}
	}
	return config.RetiredPath{}, false
}

// retiredPathBySlug matches a bare selector ("old-checkout",
// "lv24-old-checkout") against the directory name of a retired checkout, so a
// former project name redirects instead of failing as an unknown id.
func retiredPathBySlug(cfg config.BrainConfig, selector string) (config.RetiredPath, bool) {
	name := strings.TrimSpace(selector)
	if name == "" || looksLikePath(name) {
		return config.RetiredPath{}, false
	}
	for _, entry := range cfg.EffectiveRetiredPaths() {
		base := filepath.Base(filepath.FromSlash(strings.TrimSpace(entry.Path)))
		if base == "" || base == "." || base == string(filepath.Separator) {
			continue
		}
		if strings.EqualFold(name, base) || strings.EqualFold(name, strings.TrimPrefix(base, "lv24-")) {
			return entry, true
		}
	}
	return config.RetiredPath{}, false
}

// resolveRetired selects the project that replaced a retired checkout. It
// never registers or creates a project: when the replacement is unknown the
// selector is a validation error naming both paths.
func (a *App) resolveRetired(projects []domain.Project, entry config.RetiredPath) (Resolved, error) {
	replacement := strings.TrimSpace(entry.ReplacedBy)
	for _, project := range projects {
		if !strings.EqualFold(project.ID, replacement) && !containsFold(project.Aliases, replacement) {
			continue
		}
		path := firstPath(project)
		warning := fmt.Sprintf("%s is a retired checkout; resolved to %s", cleanPath(entry.Path), project.ID)
		if path != "" {
			warning += " (" + path + ")"
		}
		return Resolved{Project: project, Path: path, Warning: warning}, nil
	}
	return Resolved{}, domain.NewValidationError("project", fmt.Sprintf(
		"%q is a retired checkout replaced by %q, which is not registered here; register %s or pass an existing checkout",
		cleanPath(entry.Path), replacement, replacement))
}

// doctorRetiredPaths reports retired checkouts that still exist on disk or are
// still registered to a project. It reports only: deleting a directory or
// rewriting the registry stays the owner's decision.
func (a *App) doctorRetiredPaths(ctx context.Context) DoctorCheck {
	entries := a.cfg.EffectiveRetiredPaths()
	if len(entries) == 0 {
		return DoctorCheck{Name: "retired-paths", Status: doctorOK, Message: "no retired paths configured"}
	}
	var projects []domain.Project
	if a.deps.Store != nil {
		listed, err := a.deps.Store.ListProjects(ctx)
		if err != nil {
			return DoctorCheck{Name: "retired-paths", Status: doctorFail, Message: err.Error()}
		}
		projects = listed
	}
	var findings []string
	for _, entry := range entries {
		retired := strings.TrimSpace(entry.Path)
		if retired == "" || !filepath.IsAbs(filepath.FromSlash(retired)) {
			continue // another machine's checkout
		}
		clean := cleanPath(retired)
		if info, err := os.Stat(clean); err == nil && info.IsDir() {
			findings = append(findings, fmt.Sprintf("%s still exists (replaced by %s)", clean, entry.ReplacedBy))
		}
		for _, project := range projects {
			for _, path := range LocalPaths(project.Paths) {
				if pathWithin(normalizePath(path), normalizePath(retired)) {
					findings = append(findings, fmt.Sprintf("%s is registered to %s", cleanPath(path), project.ID))
					break
				}
			}
		}
	}
	if len(findings) == 0 {
		return DoctorCheck{Name: "retired-paths", Status: doctorOK, Message: "no retired checkout exists or is registered"}
	}
	return DoctorCheck{
		Name:    "retired-paths",
		Status:  doctorWarn,
		Code:    "retired-path",
		Message: strings.Join(findings, "; ") + " (reported only; remove the directory and repoint the project by hand)",
	}
}
