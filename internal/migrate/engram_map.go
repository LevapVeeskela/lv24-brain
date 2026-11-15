package migrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// QuarantineProject receives engram rows whose project identity is unknown.
// Its records are imported with reviewed: false and stay out of trusted
// context until a person judges them.
const QuarantineProject = "engram-unmapped"

// PersonalProject is the fallback project for personal-scope rows when the
// source session does not name a known repository.
const PersonalProject = "personal"

// ProjectMap overrides the engram project remapping rules. A file with
// {"map": {...}, "directories": {...}, "skip": [...], "quarantine": [...],
// "personal": [...]} is accepted; unknown keys are rejected so a typo cannot
// silently fall back to defaults.
type ProjectMap struct {
	// Map remaps an engram project name to a brain project id.
	Map map[string]string `json:"map,omitempty"`
	// Directories maps a session directory prefix to a brain project id. The
	// longest matching prefix wins; it attributes `main` rows and path-named
	// projects.
	Directories map[string]string `json:"directories,omitempty"`
	// Skip lists source project names whose rows are not imported at all.
	Skip []string `json:"skip,omitempty"`
	// Quarantine lists source project names imported into engram-unmapped.
	Quarantine []string `json:"quarantine,omitempty"`
	// Personal lists source project names that become personal-scope records.
	Personal []string `json:"personal,omitempty"`
}

// DefaultProjectMap is the owner-approved engram mapping from the 0.17 audit.
// Every value can be overridden with --project-map.
func DefaultProjectMap() ProjectMap {
	return ProjectMap{
		Map: map[string]string{
			"lv":                     "lv24",
			"lv24":                   "lv24",
			"lv24-brotato":           "lv24-harvest",
			"lv24-youtube-knowledge": "lv24-ytk",
			"inst":                   "instaagent",
		},
		Directories: map[string]string{
			`c:\0project\ims`:          "IMS",
			`c:\0project\lv24`:         "lv24",
			`c:\0project\lv24-brotato`: "lv24-harvest",
			`c:\0project\lv24-harvest`: "lv24-harvest",
		},
		Skip:       []string{"bats"},
		Quarantine: []string{"", `\`, "unknown", ".github"},
		Personal:   []string{"veeskela"},
	}
}

// LoadProjectMap reads a JSON override file and merges it over the defaults.
func LoadProjectMap(path string) (ProjectMap, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return DefaultProjectMap(), nil
	}
	raw, err := os.ReadFile(trimmed)
	if err != nil {
		return ProjectMap{}, fmt.Errorf("migrate: read project map: %w", err)
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var override ProjectMap
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&override); err != nil {
		return ProjectMap{}, fmt.Errorf("migrate: parse project map %s: %w", trimmed, err)
	}
	merged := mergeProjectMap(override)
	return merged, merged.Validate()
}

// mergeProjectMap layers overrides on the owner-approved defaults. Map and
// directory entries are added or overridden; a non-empty skip, quarantine or
// personal list replaces the default list.
func mergeProjectMap(overrides ProjectMap) ProjectMap {
	merged := DefaultProjectMap()
	for key, value := range overrides.Map {
		merged.Map[key] = value
	}
	for key, value := range overrides.Directories {
		merged.Directories[key] = value
	}
	if len(overrides.Skip) > 0 {
		merged.Skip = overrides.Skip
	}
	if len(overrides.Quarantine) > 0 {
		merged.Quarantine = overrides.Quarantine
	}
	if len(overrides.Personal) > 0 {
		merged.Personal = overrides.Personal
	}
	return merged
}

// Validate checks every target project id and every directory prefix.
func (m ProjectMap) Validate() error {
	for _, target := range m.Map {
		if err := validateProjectID(target); err != nil {
			return fmt.Errorf("migrate: project map target: %w", err)
		}
	}
	for directory, target := range m.Directories {
		if strings.TrimSpace(directory) == "" {
			return fmt.Errorf("migrate: project map directory prefix is empty")
		}
		if err := validateProjectID(target); err != nil {
			return fmt.Errorf("migrate: project map directory %q: %w", directory, err)
		}
	}
	return nil
}

// projectRule is the outcome of resolving one engram project value.
type projectRule struct {
	Project string
	Scope   string
	Action  string
	Reason  string
}

const (
	projectActionImport     = "import"
	projectActionQuarantine = "quarantine"
	projectActionSkip       = "skip"
)

type normalizedProjectMap struct {
	names       map[string]string
	directories map[string]string
	skip        map[string]bool
	quarantine  map[string]bool
	personal    map[string]bool
}

func (m ProjectMap) normalized() normalizedProjectMap {
	out := normalizedProjectMap{
		names:       map[string]string{},
		directories: map[string]string{},
		skip:        map[string]bool{},
		quarantine:  map[string]bool{},
		personal:    map[string]bool{},
	}
	for key, value := range m.Map {
		out.names[normalizeSourceProject(key)] = value
	}
	for key, value := range m.Directories {
		out.directories[normalizeSourceDirectory(key)] = value
	}
	for _, value := range m.Skip {
		out.skip[normalizeSourceProject(value)] = true
	}
	for _, value := range m.Quarantine {
		out.quarantine[normalizeSourceProject(value)] = true
	}
	for _, value := range m.Personal {
		out.personal[normalizeSourceProject(value)] = true
	}
	return out
}

func normalizeSourceProject(value string) string {
	normalized := strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
	normalized = strings.TrimSuffix(normalized, "/")
	return strings.ToLower(normalized)
}

func normalizeSourceDirectory(value string) string {
	normalized := strings.TrimSpace(strings.ReplaceAll(value, `\`, "/"))
	normalized = strings.TrimSuffix(normalized, "/")
	return strings.ToLower(normalized)
}

func (m normalizedProjectMap) directoryProject(directory string) string {
	normalized := normalizeSourceDirectory(directory)
	if normalized == "" {
		return ""
	}
	best, bestLength := "", 0
	for prefix, project := range m.directories {
		if normalized != prefix && !strings.HasPrefix(normalized, prefix+"/") {
			continue
		}
		if len(prefix) > bestLength {
			best, bestLength = project, len(prefix)
		}
	}
	return best
}

// resolve maps one raw engram project value. directory is the source session
// directory when one is known. The returned rule always names a target project
// unless the action is skip.
func (m normalizedProjectMap) resolve(raw, directory string) projectRule {
	key := normalizeSourceProject(raw)
	switch {
	case m.skip[key]:
		return projectRule{Action: projectActionSkip, Reason: "skip-list"}
	case m.quarantine[key]:
		return projectRule{Project: QuarantineProject, Action: projectActionQuarantine, Reason: "quarantine-list"}
	case m.personal[key]:
		project := m.directoryProject(directory)
		reason := "personal-scope"
		if project == "" {
			project = PersonalProject
			reason = "personal-scope-no-known-repo"
		}
		return projectRule{Project: project, Scope: "personal", Action: projectActionImport, Reason: reason}
	}
	if target, ok := m.names[key]; ok {
		return projectRule{Project: target, Action: projectActionImport, Reason: "explicit-map"}
	}
	if key == "main" {
		if project := m.directoryProject(directory); project != "" {
			return projectRule{Project: project, Action: projectActionImport, Reason: "session-directory"}
		}
		return projectRule{Project: QuarantineProject, Action: projectActionQuarantine, Reason: "main-without-directory"}
	}
	if looksLikeSourcePath(raw) {
		// A path-named row is attributed by its own path, never by the
		// session directory: a test prompt run inside a real repo must not
		// inherit that repo's identity.
		if project := m.directoryProject(raw); project != "" {
			return projectRule{Project: project, Action: projectActionImport, Reason: "directory-prefix"}
		}
		return projectRule{Project: QuarantineProject, Action: projectActionQuarantine, Reason: "path-not-registered"}
	}
	if validateProjectID(raw) == nil {
		return projectRule{Project: strings.TrimSpace(raw), Action: projectActionImport, Reason: "direct"}
	}
	if key == "" {
		return projectRule{Project: QuarantineProject, Action: projectActionQuarantine, Reason: "empty-project"}
	}
	return projectRule{Project: QuarantineProject, Action: projectActionQuarantine, Reason: "invalid-project-id"}
}

func looksLikeSourcePath(value string) bool {
	trimmed := strings.TrimSpace(value)
	return strings.ContainsAny(trimmed, `\/:`) || strings.HasPrefix(trimmed, ".")
}

// projectMappingReport aggregates one mapping decision per source value and
// session directory so a report lists every rule the importer applied.
type projectMappingReport struct {
	entries map[string]*ProjectMapping
}

func newProjectMappingReport() *projectMappingReport {
	return &projectMappingReport{entries: map[string]*ProjectMapping{}}
}

func (r *projectMappingReport) add(raw, directory string, rule projectRule) {
	key := raw + "\x00" + normalizeSourceDirectory(directory) + "\x00" + rule.Project + "\x00" + rule.Action + "\x00" + rule.Reason
	entry, ok := r.entries[key]
	if !ok {
		entry = &ProjectMapping{
			Source: raw, Directory: directory, Target: rule.Project,
			Scope: rule.Scope, Action: rule.Action, Reason: rule.Reason,
		}
		r.entries[key] = entry
	}
	entry.Rows++
}

func (r *projectMappingReport) list() []ProjectMapping {
	out := make([]ProjectMapping, 0, len(r.entries))
	for _, entry := range r.entries {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		if out[i].Directory != out[j].Directory {
			return out[i].Directory < out[j].Directory
		}
		return out[i].Target < out[j].Target
	})
	return out
}
