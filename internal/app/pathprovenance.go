package app

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// PathOwnership classifies one registered checkout path for the current
// machine. State is "owned" (a machine is already recorded), "stamped" (the
// path exists here and this machine becomes its owner) or "missing" (no local
// checkout, so ownership cannot be asserted from this machine).
type PathOwnership struct {
	Path    string `json:"path"`
	Machine string `json:"machine,omitempty"`
	State   string `json:"state"`
}

// ProjectOwnership groups the path classification of one project.
type ProjectOwnership struct {
	ProjectID string          `json:"project_id"`
	Paths     []PathOwnership `json:"paths"`
}

// AdoptReport is the machine-readable result of AdoptLocalPaths.
type AdoptReport struct {
	Machine  string             `json:"machine"`
	Applied  bool               `json:"applied"`
	Stamped  int                `json:"stamped"`
	Owned    int                `json:"owned"`
	Missing  int                `json:"missing"`
	Projects []ProjectOwnership `json:"projects"`
}

// LocalPaths reports how many registered paths this machine can claim.
func (r AdoptReport) LocalPaths() int { return r.Stamped + r.Owned }

// AdoptLocalPaths records the current machine as the owner of registered
// checkout paths that exist on this machine and have no recorded owner.
//
// Registries written before path provenance existed have paths without owners;
// this backfills them without touching paths that belong to another machine.
// An owner is never overwritten, so a shared registry keeps the first machine
// that registered a path. With apply false nothing is written and the report
// is a plan.
func (a *App) AdoptLocalPaths(ctx context.Context, apply bool) (AdoptReport, error) {
	if err := a.requireStore(); err != nil {
		return AdoptReport{}, err
	}
	machine := strings.TrimSpace(a.cfg.EffectiveMachineID())
	report := AdoptReport{Machine: machine, Applied: apply}
	if machine == "" {
		return report, fmt.Errorf("app: machine id is empty; set machine_id in brain.yaml")
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return report, err
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })

	for _, project := range projects {
		entry := ProjectOwnership{ProjectID: project.ID}
		owners := make(map[string]string, len(project.PathProvenance)+len(project.Paths))
		for path, owner := range project.PathProvenance {
			owners[path] = owner
		}
		changed := false
		for _, raw := range project.Paths {
			path := strings.TrimSpace(raw)
			if path == "" {
				continue
			}
			if owner, ok := lookupPathProvenance(owners, path); ok {
				entry.Paths = append(entry.Paths, PathOwnership{Path: path, Machine: owner, State: "owned"})
				report.Owned++
				continue
			}
			info, statErr := os.Stat(path)
			if statErr != nil || !info.IsDir() {
				entry.Paths = append(entry.Paths, PathOwnership{Path: path, State: "missing"})
				report.Missing++
				continue
			}
			owners[path] = machine
			entry.Paths = append(entry.Paths, PathOwnership{Path: path, Machine: machine, State: "stamped"})
			report.Stamped++
			changed = true
		}
		if len(entry.Paths) == 0 {
			continue
		}
		report.Projects = append(report.Projects, entry)
		if !changed || !apply {
			continue
		}
		project.PathProvenance = owners
		project.UpdatedAt = a.deps.Now().UTC()
		if err := a.deps.Store.PutProject(ctx, project); err != nil {
			return report, err
		}
	}
	return report, nil
}
