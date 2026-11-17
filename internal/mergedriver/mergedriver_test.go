package mergedriver_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/mergedriver"
)

func stateDoc(t *testing.T, updated time.Time, goal, next string, active, blocked, questions []string) []byte {
	t.Helper()
	var body strings.Builder
	body.WriteString("# demo — State\n\n## Goal\n\n")
	body.WriteString(goal)
	body.WriteString("\n\n## Active\n\n")
	writeItems(&body, active, "_Nothing active._")
	body.WriteString("\n## Blocked\n\n")
	writeItems(&body, blocked, "_Nothing blocked._")
	body.WriteString("\n## Next action\n\n")
	body.WriteString(next)
	body.WriteString("\n\n## Open questions\n\n")
	writeItems(&body, questions, "_None._")
	raw, err := markdown.Render(map[string]any{
		"project_id":     "demo",
		"kind":           "state",
		"schema_version": 1,
		"updated_at":     updated.UTC().Format(time.RFC3339),
	}, body.String())
	if err != nil {
		t.Fatalf("render state: %v", err)
	}
	return raw
}

func writeItems(out *strings.Builder, items []string, fallback string) {
	if len(items) == 0 {
		out.WriteString(fallback)
		out.WriteString("\n")
		return
	}
	for _, item := range items {
		out.WriteString("- ")
		out.WriteString(item)
		out.WriteString("\n")
	}
}

func TestMergeStateCombinesDifferentFields(t *testing.T) {
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	base := stateDoc(t, at, "ship 0.15", "write docs", []string{"state merge"}, nil, nil)
	ours := stateDoc(t, at.Add(time.Minute), "ship 0.15 safely", "write docs", []string{"state merge", "sync lock"}, nil, nil)
	theirs := stateDoc(t, at.Add(2*time.Minute), "ship 0.15", "write runbook", []string{"state merge"}, []string{"macOS signing"}, nil)

	result, err := mergedriver.MergeState(base, ours, theirs, mergedriver.Options{Path: "projects/demo/state.md"})
	if err != nil {
		t.Fatalf("MergeState: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", result.Conflicts)
	}
	text := string(result.Merged)
	for _, want := range []string{"ship 0.15 safely", "write runbook", "sync lock", "macOS signing"} {
		if !strings.Contains(text, want) {
			t.Fatalf("merged state misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "<<<<<<<") {
		t.Fatalf("merged state has conflict markers:\n%s", text)
	}
}

func TestMergeStateSameFieldKeepsOursAndWritesConflict(t *testing.T) {
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	base := stateDoc(t, at, "ship 0.15", "write docs", nil, nil, nil)
	ours := stateDoc(t, at.Add(time.Minute), "ship 0.15 on windows", "write docs", nil, nil, nil)
	theirs := stateDoc(t, at.Add(2*time.Minute), "ship 0.15 on linux", "write docs", nil, nil, nil)

	result, err := mergedriver.MergeState(base, ours, theirs, mergedriver.Options{Path: "projects/demo/state.md"})
	if err != nil {
		t.Fatalf("MergeState: %v", err)
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0].Field != "goal" {
		t.Fatalf("conflicts = %+v, want one goal conflict", result.Conflicts)
	}
	if !strings.Contains(string(result.Merged), "ship 0.15 on windows") {
		t.Fatalf("merged state did not keep the current value:\n%s", result.Merged)
	}
	dir := t.TempDir()
	path, err := mergedriver.WriteConflict(dir, result.Conflicts[0], at.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("WriteConflict: %v", err)
	}
	if filepath.Dir(path) != dir || !strings.HasSuffix(path, "-goal.md") {
		t.Fatalf("conflict path = %q", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ship 0.15 on windows", "ship 0.15 on linux", "projects/demo/state.md"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("conflict record misses %q:\n%s", want, raw)
		}
	}
}

func TestMergeStateListUnionAndRemoval(t *testing.T) {
	at := time.Unix(0, 0).UTC()
	base := stateDoc(t, at, "", "", []string{"keep", "drop"}, nil, nil)
	ours := stateDoc(t, at, "", "", []string{"keep", "drop", "ours-add"}, nil, nil)
	theirs := stateDoc(t, at, "", "", []string{"keep", "theirs-add"}, nil, nil)

	result, err := mergedriver.MergeState(base, ours, theirs, mergedriver.Options{})
	if err != nil {
		t.Fatalf("MergeState: %v", err)
	}
	text := string(result.Merged)
	for _, want := range []string{"- keep", "- ours-add", "- theirs-add"} {
		if !strings.Contains(text, want) {
			t.Fatalf("merged list misses %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "- drop") {
		t.Fatalf("removed item came back:\n%s", text)
	}
}

func noteBlock(id, title, body string, created time.Time) string {
	metadata, _ := json.Marshal(map[string]any{"id": id, "title": title, "created_at": created.Format(time.RFC3339)})
	return "<!-- brain:note " + string(metadata) + " -->\n## " + title + "\n\n" + body + "\n"
}

func notesDoc(t *testing.T, entries ...string) []byte {
	t.Helper()
	raw, err := markdown.Render(map[string]any{
		"project_id": "demo",
		"kind":       "notes",
		"date":       "2026-11-15",
		"updated_at": "2026-11-15T10:00:00Z",
	}, strings.Join(entries, "\n"))
	if err != nil {
		t.Fatalf("render notes: %v", err)
	}
	return raw
}

func TestMergeNotesUnionsAndDeduplicates(t *testing.T) {
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	first := noteBlock("note-a", "First", "body a", at)
	second := noteBlock("note-b", "Second", "body b", at.Add(time.Hour))
	third := noteBlock("note-c", "Third", "body c", at.Add(2*time.Hour))

	base := notesDoc(t, first)
	ours := notesDoc(t, first, third)
	theirs := notesDoc(t, first, second)

	result, err := mergedriver.MergeNotes(base, ours, theirs, mergedriver.Options{Path: "projects/demo/notes/2026-11-15.md"})
	if err != nil {
		t.Fatalf("MergeNotes: %v", err)
	}
	text := string(result.Merged)
	if strings.Count(text, "## First") != 1 {
		t.Fatalf("first note is not deduplicated:\n%s", text)
	}
	secondIndex := strings.Index(text, "## Second")
	thirdIndex := strings.Index(text, "## Third")
	if secondIndex < 0 || thirdIndex < 0 || secondIndex > thirdIndex {
		t.Fatalf("notes are not ordered by time:\n%s", text)
	}
}

func registryDoc(t *testing.T, projects ...map[string]any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(map[string]any{"version": 1, "projects": projects}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func registryProject(id, name string, aliases []string, updated time.Time) map[string]any {
	project := map[string]any{
		"id":         id,
		"name":       name,
		"kind":       "repository",
		"paths":      []string{},
		"created_at": updated.Format(time.RFC3339),
		"updated_at": updated.Format(time.RFC3339),
	}
	if aliases != nil {
		project["aliases"] = aliases
	}
	return project
}

func TestMergeRegistryUnionsProjects(t *testing.T) {
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	base := registryDoc(t, registryProject("alpha", "Alpha", nil, at))
	ours := registryDoc(t,
		registryProject("alpha", "Alpha", nil, at),
		registryProject("beta", "Beta", nil, at.Add(time.Hour)),
	)
	theirs := registryDoc(t,
		registryProject("alpha", "Alpha", nil, at),
		registryProject("gamma", "Gamma", nil, at.Add(2*time.Hour)),
	)

	result, err := mergedriver.MergeRegistry(base, ours, theirs, mergedriver.Options{Path: "registry.json"})
	if err != nil {
		t.Fatalf("MergeRegistry: %v", err)
	}
	var decoded struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(result.Merged, &decoded); err != nil {
		t.Fatalf("merged registry is not JSON: %v\n%s", err, result.Merged)
	}
	ids := make([]string, 0, len(decoded.Projects))
	for _, project := range decoded.Projects {
		ids = append(ids, project.ID)
	}
	if strings.Join(ids, ",") != "alpha,beta,gamma" {
		t.Fatalf("project ids = %v", ids)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v", result.Conflicts)
	}
}

func TestMergeRegistryAliasConflictKeepsOneOwner(t *testing.T) {
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	base := registryDoc(t,
		registryProject("alpha", "Alpha", nil, at),
		registryProject("beta", "Beta", nil, at),
	)
	ours := registryDoc(t,
		registryProject("alpha", "Alpha", []string{"web"}, at.Add(time.Hour)),
		registryProject("beta", "Beta", nil, at),
	)
	theirs := registryDoc(t,
		registryProject("alpha", "Alpha", nil, at),
		registryProject("beta", "Beta", []string{"web"}, at.Add(2*time.Hour)),
	)

	result, err := mergedriver.MergeRegistry(base, ours, theirs, mergedriver.Options{Path: "registry.json"})
	if err != nil {
		t.Fatalf("MergeRegistry: %v", err)
	}
	if len(result.Conflicts) != 1 || !strings.Contains(result.Conflicts[0].Field, "web") {
		t.Fatalf("conflicts = %+v, want one web alias conflict", result.Conflicts)
	}
	var decoded struct {
		Projects []struct {
			ID      string   `json:"id"`
			Aliases []string `json:"aliases"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(result.Merged, &decoded); err != nil {
		t.Fatal(err)
	}
	owners := 0
	for _, project := range decoded.Projects {
		for _, alias := range project.Aliases {
			if alias == "web" {
				owners++
			}
		}
	}
	if owners != 1 {
		t.Fatalf("alias web has %d owners:\n%s", owners, result.Merged)
	}
}

// registryProjectWithPaths builds a project record carrying checkout paths and
// the machine id that registered each of them.
func registryProjectWithPaths(id string, paths []string, provenance map[string]string, updated time.Time) map[string]any {
	project := registryProject(id, id, nil, updated)
	project["paths"] = paths
	if len(provenance) > 0 {
		project["path_provenance"] = provenance
	}
	return project
}

func TestMergeRegistryUnionsPathsWithProvenance(t *testing.T) {
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	// Two machines each add a checkout to the same project while the other
	// side touches the record: neither path may be dropped, and each path
	// keeps the machine id that registered it.
	base := registryDoc(t,
		registryProjectWithPaths("alpha", []string{"/a"}, map[string]string{"/a": "m1"}, at))
	ours := registryDoc(t,
		registryProjectWithPaths("alpha", []string{"/a", "/b"}, map[string]string{"/a": "m1", "/b": "m1"}, at.Add(time.Hour)))
	theirs := registryDoc(t,
		registryProjectWithPaths("alpha", []string{"/a", "/c"}, map[string]string{"/a": "m1", "/c": "m2"}, at.Add(2*time.Hour)))

	result, err := mergedriver.MergeRegistry(base, ours, theirs, mergedriver.Options{Path: "registry.json"})
	if err != nil {
		t.Fatalf("MergeRegistry: %v", err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none for a union of paths", result.Conflicts)
	}
	var decoded struct {
		Projects []struct {
			ID             string            `json:"id"`
			Paths          []string          `json:"paths"`
			PathProvenance map[string]string `json:"path_provenance"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(result.Merged, &decoded); err != nil {
		t.Fatalf("merged registry is not JSON: %v\n%s", err, result.Merged)
	}
	if len(decoded.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(decoded.Projects))
	}
	alpha := decoded.Projects[0]
	got := map[string]bool{}
	for _, path := range alpha.Paths {
		got[path] = true
	}
	for _, want := range []string{"/a", "/b", "/c"} {
		if !got[want] {
			t.Fatalf("paths %v missing %q (union lost a machine's checkout)", alpha.Paths, want)
		}
	}
	if len(alpha.Paths) != 3 {
		t.Fatalf("paths = %v, want exactly the 3-way union", alpha.Paths)
	}
	for path, owner := range map[string]string{"/a": "m1", "/b": "m1", "/c": "m2"} {
		if alpha.PathProvenance[path] != owner {
			t.Fatalf("provenance[%q] = %q, want %q (full map %+v)",
				path, alpha.PathProvenance[path], owner, alpha.PathProvenance)
		}
	}
}

func TestMergeRegistryPathClaimedTwiceKeepsOneOwner(t *testing.T) {
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	base := registryDoc(t,
		registryProjectWithPaths("alpha", []string{}, nil, at),
		registryProjectWithPaths("beta", []string{}, nil, at))
	ours := registryDoc(t,
		registryProjectWithPaths("alpha", []string{"/shared"}, map[string]string{"/shared": "m1"}, at.Add(time.Hour)),
		registryProjectWithPaths("beta", []string{}, nil, at))
	theirs := registryDoc(t,
		registryProjectWithPaths("alpha", []string{}, nil, at),
		registryProjectWithPaths("beta", []string{"/shared"}, map[string]string{"/shared": "m2"}, at.Add(2*time.Hour)))

	result, err := mergedriver.MergeRegistry(base, ours, theirs, mergedriver.Options{Path: "registry.json"})
	if err != nil {
		t.Fatalf("MergeRegistry: %v", err)
	}
	if len(result.Conflicts) != 1 ||
		!strings.Contains(result.Conflicts[0].Field, "path") ||
		!strings.Contains(result.Conflicts[0].Field, "shared") {
		t.Fatalf("conflicts = %+v, want one /shared path claim conflict", result.Conflicts)
	}
	var decoded struct {
		Projects []struct {
			ID             string            `json:"id"`
			Paths          []string          `json:"paths"`
			PathProvenance map[string]string `json:"path_provenance"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(result.Merged, &decoded); err != nil {
		t.Fatal(err)
	}
	owners := make([]string, 0, 2)
	for _, project := range decoded.Projects {
		for _, path := range project.Paths {
			if path == "/shared" {
				owners = append(owners, project.ID)
				if project.PathProvenance[path] != "m2" {
					t.Fatalf("winner provenance = %+v, want m2 (the later updated_at)", project.PathProvenance)
				}
			}
		}
		if project.ID == "alpha" && len(project.Paths) != 0 {
			t.Fatalf("losing alpha kept paths %+v", project.Paths)
		}
		if project.ID == "alpha" && len(project.PathProvenance) != 0 {
			t.Fatalf("losing alpha kept provenance %+v", project.PathProvenance)
		}
	}
	if strings.Join(owners, ",") != "beta" {
		t.Fatalf("/shared owners = %v, want only beta", owners)
	}
}

func TestWriteConflictRecordsMachine(t *testing.T) {
	conflict := mergedriver.Conflict{
		Kind:    "registry",
		Field:   "path-/shared",
		Ours:    "beta",
		Theirs:  "alpha",
		Path:    "registry.json",
		Machine: "m1",
	}
	path, err := mergedriver.WriteConflict(t.TempDir(), conflict, time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("WriteConflict: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "- machine: m1") {
		t.Fatalf("conflict record misses the machine provenance:\n%s", raw)
	}
}

func TestMergedStateFrontmatterStaysReadable(t *testing.T) {
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	base := stateDoc(t, at, "ship 0.15", "write docs", nil, nil, nil)
	ours := stateDoc(t, at.Add(time.Minute), "ship 0.15", "write docs", []string{"merge drivers"}, nil, nil)
	theirs := stateDoc(t, at.Add(2*time.Minute), "ship 0.15", "write docs", nil, []string{"signing"}, nil)

	result, err := mergedriver.MergeState(base, ours, theirs, mergedriver.Options{})
	if err != nil {
		t.Fatalf("MergeState: %v", err)
	}
	var frontmatter struct {
		ProjectID string    `yaml:"project_id"`
		UpdatedAt time.Time `yaml:"updated_at"`
	}
	body, err := markdown.Parse(result.Merged, &frontmatter)
	if err != nil {
		t.Fatalf("merged state is not readable: %v\n%s", err, result.Merged)
	}
	if frontmatter.ProjectID != "demo" || frontmatter.UpdatedAt.IsZero() {
		t.Fatalf("frontmatter = %+v", frontmatter)
	}
	if !strings.Contains(body, "merge drivers") || !strings.Contains(body, "signing") {
		t.Fatalf("merged body misses a list item:\n%s", body)
	}
}
