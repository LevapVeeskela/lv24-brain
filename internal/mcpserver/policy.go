package mcpserver

import (
	"fmt"
	"slices"
	"strings"
)

// Policy is the least-privilege profile of one server process: which tools are
// listed, which projects they may reach, and where a write may land. It is the
// server-side half of `brain mcp --read-only`, `--projects` and `--allow-write`.
//
// The zero Policy serves every tool over every registered project, which is
// what New does for embedded callers and tests. `brain mcp` always sets
// RestrictWrites, so the command enforces its write scope even when the caller
// never touches mcpserver directly.
type Policy struct {
	// ReadOnly lists read-only tools only. Every other tool is omitted from
	// tools/list, and a call to one is refused with UNSUPPORTED before the
	// backend sees it.
	ReadOnly bool
	// Projects is the allowlist of project ids for reads and writes, already
	// resolved from ids and aliases. Empty allows every registered project.
	Projects []string
	// AllowWrite lets write tools target any project of Projects. With
	// RestrictWrites and without AllowWrite, a write may only target
	// DefaultProject.
	AllowWrite bool
	// DefaultProject is the project of the working directory, the same project
	// `brain open` picks. Empty means the working directory is not a project.
	DefaultProject string
	// RestrictWrites enables the write scope. The zero value is unrestricted so
	// an embedded server keeps the behaviour of earlier releases; the CLI sets
	// it unconditionally.
	RestrictWrites bool
}

// PolicyInfo is the JSON form of an effective policy. `brain mcp --list-tools
// --json` prints it so a host can diff the profile it approved.
type PolicyInfo struct {
	ReadOnly   bool     `json:"read_only"`
	AllowWrite bool     `json:"allow_write"`
	Projects   []string `json:"projects"`
	// AllProjects is true when no allowlist was given.
	AllProjects bool `json:"all_projects"`
}

// Info describes the policy. Projects always marshals as a list, never null.
func (p Policy) Info() PolicyInfo {
	projects := p.Projects
	if projects == nil {
		projects = []string{}
	}
	return PolicyInfo{
		ReadOnly:    p.ReadOnly,
		AllowWrite:  p.AllowWrite && !p.ReadOnly,
		Projects:    projects,
		AllProjects: len(p.Projects) == 0,
	}
}

// ToolListed reports whether the policy exposes a tool. Read-only tools are
// always listed; every other tool needs a writable server.
func (p Policy) ToolListed(spec ToolSpec) bool {
	return !p.ReadOnly || spec.Annotations.ReadOnlyHint
}

// listsTool is ToolListed for the internal tool type.
func (p Policy) listsTool(t tool) bool {
	return !p.ReadOnly || t.Annotations.ReadOnlyHint
}

// allowsProject reports whether a project is inside the allowlist.
func (p Policy) allowsProject(id string) bool {
	return len(p.Projects) == 0 || slices.Contains(p.Projects, id)
}

// projectList renders the allowlist for a refusal message.
func (p Policy) projectList() string {
	return strings.Join(p.Projects, ", ")
}

// checkProject enforces the allowlist on a project a tool resolved.
func (p Policy) checkProject(projectID string) error {
	if p.allowsProject(projectID) {
		return nil
	}
	return refusalInvalidf("project %s is outside this server's allowed projects (%s)", projectID, p.projectList())
}

// readProject scopes an unscoped read. Without an allowlist every project stays
// searchable (""); with one allowed project the read binds to it; with several
// the caller must name one, so a single result stream cannot span projects the
// pagination cursor does not describe.
func (p Policy) readProject() (string, error) {
	switch len(p.Projects) {
	case 0:
		return "", nil
	case 1:
		return p.Projects[0], nil
	default:
		return "", refusalInvalidf("project is required: this server is limited to %s", p.projectList())
	}
}

// allowsWrite reports whether a write may target the project.
func (p Policy) allowsWrite(projectID string) bool {
	if p.ReadOnly {
		return false
	}
	if !p.RestrictWrites {
		return true
	}
	if p.AllowWrite {
		return true
	}
	return projectID != "" && projectID == p.DefaultProject
}

// checkWrite enforces the write scope on a project a tool resolved.
func (p Policy) checkWrite(projectID string) error {
	if p.allowsWrite(projectID) {
		return nil
	}
	if p.DefaultProject == "" {
		return refusalUnsupported(
			"write tools are unavailable: this server was started outside a project and --allow-write is not set",
			"run brain mcp from a project checkout, or start it with --allow-write (optionally bounded by --projects)")
	}
	return refusalUnsupported(
		fmt.Sprintf("writes are limited to the project of the working directory (%s); %s is outside it", p.DefaultProject, projectID),
		"start brain mcp with --allow-write to write to another project, or run the equivalent brain command in that checkout")
}

// filterProjects drops projects outside the allowlist from a listing.
func (p Policy) filterProjects(projects []Project) []Project {
	if len(p.Projects) == 0 {
		return projects
	}
	out := make([]Project, 0, len(projects))
	for _, project := range projects {
		if p.allowsProject(project.ID) {
			out = append(out, project)
		}
	}
	return out
}
