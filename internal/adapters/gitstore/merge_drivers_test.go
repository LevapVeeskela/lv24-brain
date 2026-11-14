package gitstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestRegisterMergeDriversIsIdempotent(t *testing.T) {
	requireGit(t)
	repo := initRepo(t, t.TempDir(), false)
	provider := newTestProvider(t, repo.dir)

	missing, err := provider.MissingMergeDrivers(context.Background())
	if err != nil {
		t.Fatalf("MissingMergeDrivers: %v", err)
	}
	if len(missing) != len(MergeDrivers()) {
		t.Fatalf("missing = %v, want every driver", missing)
	}

	executable := filepath.Join(t.TempDir(), "brain")
	changed, err := provider.RegisterMergeDrivers(context.Background(), executable)
	if err != nil {
		t.Fatalf("RegisterMergeDrivers: %v", err)
	}
	if !changed {
		t.Fatal("first registration reported no change")
	}
	missing, err = provider.MissingMergeDrivers(context.Background())
	if err != nil {
		t.Fatalf("MissingMergeDrivers after register: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing after register = %v", missing)
	}

	changed, err = provider.RegisterMergeDrivers(context.Background(), executable)
	if err != nil {
		t.Fatalf("second RegisterMergeDrivers: %v", err)
	}
	if changed {
		t.Fatal("second registration changed the config")
	}

	for _, driver := range MergeDrivers() {
		value := repo.run("config", "--local", "--get", "merge."+driver.Name+".driver")
		if !strings.Contains(value, "merge-driver") || !strings.Contains(value, driverCommand(driver.Name)) {
			t.Fatalf("%s driver = %q", driver.Name, value)
		}
		if runtime.GOOS == "windows" && !strings.Contains(value, "/") {
			t.Fatalf("%s driver uses backslashes on Windows: %q", driver.Name, value)
		}
	}
}

func TestRegisterMergeDriversOutsideRepositoryIsNoop(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	provider := newTestProvider(t, dir)

	missing, err := provider.MissingMergeDrivers(context.Background())
	if !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("MissingMergeDrivers error = %v, want ErrUnsupported", err)
	}
	if missing != nil {
		t.Fatalf("missing = %v, want nil", missing)
	}
	changed, err := provider.RegisterMergeDrivers(context.Background(), filepath.Join(dir, "brain"))
	if err != nil {
		t.Fatalf("RegisterMergeDrivers: %v", err)
	}
	if changed {
		t.Fatal("registration outside a repository reported a change")
	}
}

// buildBrainBinary compiles the CLI once so git can execute the real hidden
// merge-driver command. A test binary cannot be the driver: Go would parse the
// driver arguments as test flags.
func buildBrainBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("building the CLI is skipped in -short mode")
	}
	name := "brain"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", exe, "./cmd/brain")
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve the test source path")
	}
	cmd.Dir = filepath.Join(filepath.Dir(source), "..", "..", "..")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/brain: %v\n%s", err, out)
	}
	if info, err := os.Stat(exe); err != nil || info.Size() == 0 {
		t.Fatalf("built binary is missing: %v", err)
	}
	return exe
}

type mergeFixture struct {
	root     string
	bare     string
	ours     *testRepo
	theirs   *testRepo
	provider *Provider
}

// newMergeFixture seeds a bare remote with the merge-driver attributes and one
// project, clones two working copies, and registers the compiled CLI as the
// driver in both.
func newMergeFixture(t *testing.T) *mergeFixture {
	t.Helper()
	requireGit(t)
	exe := buildBrainBinary(t)

	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	initRepo(t, bare, true)

	seed := initRepo(t, filepath.Join(root, "seed"), false)
	writeFile(t, seed.dir, ".gitattributes", `journal/*.jsonl merge=union
projects/*/state.md merge=lv24-state
registry.json merge=lv24-registry
projects/*/notes/*.md merge=lv24-notes
`)
	writeFile(t, seed.dir, "projects/demo/state.md", mergeStateFile("ship 0.15", "write docs", nil))
	writeFile(t, seed.dir, "projects/demo/notes/2026-11-15.md", mergeNotesFile(t, mergeNote("note-a", "First", "body a", "2026-11-15T09:00:00Z")))
	writeFile(t, seed.dir, "registry.json", mergeRegistryFile("alpha", "Alpha"))
	seed.run("add", "--all")
	seed.run("commit", "--quiet", "-m", "seed")
	seed.run("remote", "add", "origin", bare)
	seed.run("push", "--quiet", "-u", "origin", "main")

	clones := make([]*testRepo, 2)
	for i := range clones {
		dir := filepath.Join(root, "clone-"+string(rune('a'+i)))
		runGit(t, root, "clone", "--quiet", bare, dir)
		clones[i] = &testRepo{t: t, dir: dir}
		clones[i].configure()
		provider, err := Open(dir, Options{})
		if err != nil {
			t.Fatalf("Open(%q): %v", dir, err)
		}
		if _, err := provider.RegisterMergeDrivers(context.Background(), exe); err != nil {
			t.Fatalf("RegisterMergeDrivers(%q): %v", dir, err)
		}
	}
	return &mergeFixture{root: root, bare: bare, ours: clones[0], theirs: clones[1], provider: newTestProvider(t, clones[0].dir)}
}

func (f *mergeFixture) publishTheirs(t *testing.T, message string) {
	t.Helper()
	f.theirs.run("add", "--all")
	f.theirs.run("commit", "--quiet", "-m", message)
	f.theirs.run("push", "--quiet", "origin", "main")
}

func mergeStateFile(goal, next string, active []string) string {
	var body strings.Builder
	body.WriteString("# demo — State\n\n## Goal\n\n")
	body.WriteString(goal)
	body.WriteString("\n\n## Active\n\n")
	if len(active) == 0 {
		body.WriteString("_Nothing active._\n")
	} else {
		for _, item := range active {
			body.WriteString("- " + item + "\n")
		}
	}
	body.WriteString("\n## Blocked\n\n_Nothing blocked._\n")
	body.WriteString("\n## Next action\n\n" + next)
	body.WriteString("\n\n## Open questions\n\n_None._\n")
	return "---\nproject_id: demo\nkind: state\nschema_version: 1\nupdated_at: 2026-11-15T09:00:00Z\n---\n\n" + body.String()
}

func mergeNote(id, title, body, created string) string {
	return "<!-- brain:note {\"id\":\"" + id + "\",\"title\":\"" + title + "\",\"created_at\":\"" + created + "\"} -->\n## " + title + "\n\n" + body + "\n"
}

func mergeNotesFile(t *testing.T, entries ...string) string {
	t.Helper()
	return "---\nproject_id: demo\nkind: notes\ndate: 2026-11-15\nschema_version: 1\nupdated_at: 2026-11-15T09:00:00Z\n---\n\n" + strings.Join(entries, "\n")
}

func mergeRegistryFile(projects ...string) string {
	var records []string
	for _, id := range projects {
		name := strings.ToUpper(id[:1]) + id[1:]
		records = append(records, `    {
      "id": "`+id+`",
      "name": "`+name+`",
      "kind": "repository",
      "paths": [],
      "created_at": "2026-11-15T09:00:00Z",
      "updated_at": "2026-11-15T09:00:00Z"
    }`)
	}
	return "{\n  \"version\": 1,\n  \"projects\": [\n" + strings.Join(records, ",\n") + "\n  ]\n}\n"
}

func TestMergeDriversMergeDifferentStateFields(t *testing.T) {
	fixture := newMergeFixture(t)
	writeFile(t, fixture.ours.dir, "projects/demo/state.md", mergeStateFile("ship 0.15 on Windows", "write docs", nil))
	fixture.ours.run("add", "--all")
	fixture.ours.run("commit", "--quiet", "-m", "ours goal")
	writeFile(t, fixture.theirs.dir, "projects/demo/state.md", mergeStateFile("ship 0.15", "write the runbook", nil))
	fixture.publishTheirs(t, "theirs next")

	result, err := fixture.provider.Pull(context.Background(), ports.SyncOptions{Autostash: true})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(result.Conflicts) > 0 {
		t.Fatalf("conflicts = %v", result.Conflicts)
	}
	merged := readFile(t, fixture.ours.dir, "projects/demo/state.md")
	for _, want := range []string{"ship 0.15 on Windows", "write the runbook"} {
		if !strings.Contains(merged, want) {
			t.Fatalf("merged state misses %q:\n%s", want, merged)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(fixture.ours.dir, "projects", "demo", "state-conflicts", "*.md")); len(matches) > 0 {
		t.Fatalf("unexpected conflict records: %v", matches)
	}
}

func TestMergeDriversSameStateFieldKeepsOursAndRecordsConflict(t *testing.T) {
	fixture := newMergeFixture(t)
	writeFile(t, fixture.ours.dir, "projects/demo/state.md", mergeStateFile("ours goal", "write docs", nil))
	fixture.ours.run("add", "--all")
	fixture.ours.run("commit", "--quiet", "-m", "ours goal")
	writeFile(t, fixture.theirs.dir, "projects/demo/state.md", mergeStateFile("theirs goal", "write docs", nil))
	fixture.publishTheirs(t, "theirs goal")

	result, err := fixture.provider.Pull(context.Background(), ports.SyncOptions{Autostash: true})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(result.Conflicts) > 0 {
		t.Fatalf("conflicts = %v, want a resolved field conflict", result.Conflicts)
	}
	// During a rebase git presents the upstream version as the current %A, so
	// the merged file keeps the incoming value while the conflict record
	// preserves the replayed local value. Nothing is lost silently.
	merged := readFile(t, fixture.ours.dir, "projects/demo/state.md")
	if !strings.Contains(merged, "theirs goal") || strings.Contains(merged, "<<<<<<<") {
		t.Fatalf("merged state did not keep the current %%A value:\n%s", merged)
	}
	matches, err := filepath.Glob(filepath.Join(fixture.ours.dir, "projects", "demo", "state-conflicts", "*.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("conflict records = %v (%v), want one", matches, err)
	}
	record, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"theirs goal", "ours goal", "projects/demo/state.md"} {
		if !strings.Contains(string(record), want) {
			t.Fatalf("conflict record misses %q:\n%s", want, record)
		}
	}
}

func TestMergeDriversUnionNotes(t *testing.T) {
	fixture := newMergeFixture(t)
	first := mergeNote("note-a", "First", "body a", "2026-11-15T09:00:00Z")
	second := mergeNote("note-b", "Second", "body b", "2026-11-15T10:00:00Z")
	third := mergeNote("note-c", "Third", "body c", "2026-11-15T11:00:00Z")

	writeFile(t, fixture.ours.dir, "projects/demo/notes/2026-11-15.md", mergeNotesFile(t, first, third))
	fixture.ours.run("add", "--all")
	fixture.ours.run("commit", "--quiet", "-m", "ours note")
	writeFile(t, fixture.theirs.dir, "projects/demo/notes/2026-11-15.md", mergeNotesFile(t, first, second))
	fixture.publishTheirs(t, "theirs note")

	if _, err := fixture.provider.Pull(context.Background(), ports.SyncOptions{Autostash: true}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	merged := readFile(t, fixture.ours.dir, "projects/demo/notes/2026-11-15.md")
	for _, want := range []string{"## First", "## Second", "## Third"} {
		if strings.Count(merged, want) != 1 {
			t.Fatalf("merged notes has %d copies of %q:\n%s", strings.Count(merged, want), want, merged)
		}
	}
	if strings.Index(merged, "## Second") > strings.Index(merged, "## Third") {
		t.Fatalf("notes are not ordered by time:\n%s", merged)
	}
}

func TestMergeDriversUnionRegistry(t *testing.T) {
	fixture := newMergeFixture(t)
	writeFile(t, fixture.ours.dir, "registry.json", mergeRegistryFile("alpha", "beta"))
	fixture.ours.run("add", "--all")
	fixture.ours.run("commit", "--quiet", "-m", "ours project")
	writeFile(t, fixture.theirs.dir, "registry.json", mergeRegistryFile("alpha", "gamma"))
	fixture.publishTheirs(t, "theirs project")

	if _, err := fixture.provider.Pull(context.Background(), ports.SyncOptions{Autostash: true}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	merged := readFile(t, fixture.ours.dir, "registry.json")
	for _, want := range []string{`"alpha"`, `"beta"`, `"gamma"`} {
		if !strings.Contains(merged, want) {
			t.Fatalf("merged registry misses %s:\n%s", want, merged)
		}
	}
	if strings.Contains(merged, "<<<<<<<") {
		t.Fatalf("merged registry has conflict markers:\n%s", merged)
	}
}
