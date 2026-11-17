package app_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/gitstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/mergedriver"
)

// TestMain doubles as the merge driver binary for the three-machine test. Git
// runs the test executable directly with "merge-driver <kind> %O %A %B %L %P";
// the same dispatch the production brain binary uses.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "merge-driver" {
		os.Exit(runMergeDriverHelper(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func runMergeDriverHelper(args []string) int {
	if len(args) < 4 {
		return 2
	}
	driver, basePath, oursPath, theirsPath := args[0], args[1], args[2], args[3]
	relativePath := ""
	if len(args) >= 6 {
		relativePath = args[5]
	}
	read := func(path string) ([]byte, error) {
		return os.ReadFile(path)
	}
	base, err := read(basePath)
	if err != nil {
		return 1
	}
	ours, err := read(oursPath)
	if err != nil {
		return 1
	}
	theirs, err := read(theirsPath)
	if err != nil {
		return 1
	}
	options := mergedriver.Options{Path: filepath.ToSlash(relativePath), Now: time.Now().UTC()}
	var result mergedriver.Result
	switch driver {
	case "state":
		result, err = mergedriver.MergeState(base, ours, theirs, options)
	case "notes":
		result, err = mergedriver.MergeNotes(base, ours, theirs, options)
	case "registry":
		result, err = mergedriver.MergeRegistry(base, ours, theirs, options)
	case "items":
		result, err = mergedriver.MergeItems(base, ours, theirs, options)
	default:
		return 2
	}
	if err != nil {
		return 1
	}
	if err := os.WriteFile(oursPath, result.Merged, 0o644); err != nil {
		return 1
	}
	return 0
}

// appForMachine opens the application over one clone of the shared data
// repository: the same store, sync provider and catalogue the CLI uses.
func appForMachine(t *testing.T, machines *rawZoneMachines, machine int) *app.App {
	t.Helper()
	provider, err := gitstore.Open(machines.dirs[machine], gitstore.Options{})
	if err != nil {
		t.Fatalf("gitstore.Open: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if _, err := provider.RegisterMergeDrivers(context.Background(), executable); err != nil {
		t.Fatalf("register merge drivers on machine %d: %v", machine, err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = machines.dirs[machine]
	cfg.SchemaVersion = config.CurrentSchemaVersion
	return app.New(cfg, app.Dependencies{
		Store:       machines.store(machine),
		Sync:        provider,
		Connections: machines.catalogue(machine),
	})
}

// TestThreeMachinesWriteConcurrently runs the intended topology: three clones,
// two sessions per machine, different records and state fields. Every write
// must survive the merge; no record is lost and the registry keeps one project.
func TestThreeMachinesWriteConcurrently(t *testing.T) {
	machines := newRawZoneMachinesN(t, 3)
	ctx := context.Background()
	apps := make([]*app.App, 3)
	for i := range apps {
		apps[i] = appForMachine(t, machines, i)
	}

	if _, err := apps[0].Note(ctx, app.NoteInput{
		ProjectID: "shared-three", Title: "seed", Body: "seed body", CreateProject: true,
	}); err != nil {
		t.Fatalf("seed note: %v", err)
	}
	machines.sync(0, "seed")
	machines.git(machines.dirs[1], "pull", "-q", "--rebase", "origin", "main")
	machines.git(machines.dirs[2], "pull", "-q", "--rebase", "origin", "main")

	goal := "three machines"
	next := "keep writing"
	active := []string{"machine session"}
	if _, err := apps[0].SetState(ctx, app.StateInput{ProjectID: "shared-three", Goal: &goal}); err != nil {
		t.Fatalf("state goal: %v", err)
	}
	if _, err := apps[1].SetState(ctx, app.StateInput{ProjectID: "shared-three", NextAction: &next}); err != nil {
		t.Fatalf("state next: %v", err)
	}
	if _, err := apps[2].SetState(ctx, app.StateInput{ProjectID: "shared-three", Active: &active}); err != nil {
		t.Fatalf("state active: %v", err)
	}

	// Two sessions per machine write at the same time. The titles and bodies
	// are unique per session, so a lost update is visible as a missing marker.
	var writers sync.WaitGroup
	for machine := 0; machine < 3; machine++ {
		for session := 0; session < 2; session++ {
			machine, session := machine, session
			writers.Add(1)
			go func() {
				defer writers.Done()
				for i := 0; i < 2; i++ {
					title := fmt.Sprintf("machine-%d-session-%d-note-%d", machine, session, i)
					if _, err := apps[machine].Note(ctx, app.NoteInput{
						ProjectID: "shared-three",
						Title:     title,
						Body:      title + " body",
					}); err != nil {
						t.Errorf("%s: %v", title, err)
					}
				}
			}()
		}
	}
	writers.Wait()

	machines.sync(0, "machine 0 writes")
	machines.sync(1, "machine 1 writes")
	machines.sync(2, "machine 2 writes")
	machines.git(machines.dirs[0], "pull", "-q", "--rebase", "origin", "main")
	machines.git(machines.dirs[1], "pull", "-q", "--rebase", "origin", "main")

	const wantNotes = 1 + 3*2*2
	for machine, dir := range machines.dirs {
		notes := readMarkers(t, filepath.Join(dir, "projects", "shared-three", "notes"), "<!-- brain:note ")
		if notes != wantNotes {
			t.Fatalf("machine %d has %d note markers, want %d", machine, notes, wantNotes)
		}
		state, err := os.ReadFile(filepath.Join(dir, "projects", "shared-three", "state.md"))
		if err != nil {
			t.Fatalf("machine %d state: %v", machine, err)
		}
		for _, want := range []string{goal, next, active[0]} {
			if !strings.Contains(string(state), want) {
				t.Fatalf("machine %d state misses %q:\n%s", machine, want, state)
			}
		}
	}
}

// readMarkers counts records in the day files of one project.
func readMarkers(t *testing.T, dir, marker string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		count += strings.Count(string(raw), marker)
	}
	return count
}
