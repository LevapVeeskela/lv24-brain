package app_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// rawZoneMachines is one bare remote plus two data directories initialized as
// git data repositories: the two-machine setup the raw zone must survive.
type rawZoneMachines struct {
	t      *testing.T
	remote string
	dirs   []string
}

func newRawZoneMachines(t *testing.T) *rawZoneMachines {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	machines := &rawZoneMachines{t: t, remote: filepath.Join(root, "remote.git")}
	machines.git(root, "init", "-q", "--bare", "-b", "main", machines.remote)

	first := filepath.Join(root, "machine-a")
	if _, err := app.New(config.Default(), app.Dependencies{}).Init(context.Background(), app.InitOptions{
		ConfigPath: filepath.Join(first, "brain.yaml"), Mode: config.ModeGit, DataDir: first,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	machines.git(first, "init", "-q", "-b", "main")
	machines.git(first, "add", "-A")
	machines.git(first, "commit", "-q", "-m", "seed")
	machines.git(first, "remote", "add", "origin", machines.remote)
	machines.git(first, "push", "-q", "origin", "main")

	second := filepath.Join(root, "machine-b")
	machines.git(root, "clone", "-q", machines.remote, second)
	machines.dirs = []string{first, second}
	return machines
}

func (m *rawZoneMachines) git(dir string, args ...string) string {
	m.t.Helper()
	base := []string{
		"-C", dir,
		"-c", "user.name=t", "-c", "user.email=t@example.test",
		"-c", "core.autocrlf=false",
		"-c", "gc.auto=0", "-c", "maintenance.auto=false",
	}
	cmd := exec.Command("git", append(base, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		m.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// store opens the raw zone of one machine.
func (m *rawZoneMachines) store(machine int) *fsstore.Store {
	m.t.Helper()
	store, err := fsstore.New(m.dirs[machine])
	if err != nil {
		m.t.Fatalf("fsstore.New: %v", err)
	}
	return store
}

// sync commits everything git does not ignore, rebases on the remote and
// pushes: the same sequence brain sync runs.
func (m *rawZoneMachines) sync(machine int, message string) {
	m.t.Helper()
	dir := m.dirs[machine]
	m.git(dir, "add", "-A")
	m.git(dir, "commit", "-q", "-m", message)
	m.git(dir, "pull", "-q", "--rebase", "origin", "main")
	m.git(dir, "push", "-q", "origin", "main")
}

func (m *rawZoneMachines) appendJournal(machine int, line string) {
	m.t.Helper()
	dir := filepath.Join(m.dirs[machine], "journal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.t.Fatalf("mkdir journal: %v", err)
	}
	file, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("machine-%d.jsonl", machine)), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		m.t.Fatalf("open journal: %v", err)
	}
	if _, err := file.WriteString(line + "\n"); err != nil {
		_ = file.Close()
		m.t.Fatalf("append journal: %v", err)
	}
	if err := file.Close(); err != nil {
		m.t.Fatalf("close journal: %v", err)
	}
}

// rawZoneItem builds one item whose provider times (created/updated) come from
// the source and are therefore equal on both machines; only the host
// observation time differs.
func rawZoneItem(round int, observed time.Time) domain.Item {
	return domain.Item{
		Ref:        domain.ItemRef{ConnectionID: "gh-acme", ExternalID: fmt.Sprintf("issue-%d", round)},
		Kind:       domain.ItemIssue,
		Title:      fmt.Sprintf("Issue %d", round),
		Body:       fmt.Sprintf("Body of issue %d", round),
		UpdatedAt:  time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC).Add(time.Duration(round) * time.Minute),
		ObservedAt: observed,
	}.WithHash()
}

// rawZoneSyncs is how many sync operations the two-machine pair performs in the
// default run: each round syncs both machines, so 100 syncs is 50 rounds with a
// real commit from each side in every round.
const rawZoneSyncs = 100

// TestTwoMachinesSyncRawZoneWithoutConflicts: both machines ingest the same
// connection and sync 100 times. Raw items and checkpoints stay machine-local,
// so no sync ever stops on a same-path/different-bytes conflict, and the shared
// tree holds neither.
func TestTwoMachinesSyncRawZoneWithoutConflicts(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	stores := []*fsstore.Store{machines.store(0), machines.store(1)}
	rounds := rawZoneSyncs / len(stores)
	if testing.Short() {
		rounds = 2
	}
	for round := 0; round < rounds; round++ {
		for machine, store := range stores {
			observed := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(round) * time.Minute)
			item := rawZoneItem(round, observed)
			if _, err := store.Items().Upsert(ctx, item); err != nil {
				t.Fatalf("round %d machine %d: Upsert: %v", round, machine, err)
			}
			checkpoint := domain.Checkpoint{Stream: "issues", Version: 1, Data: []byte(fmt.Sprintf("machine-%d-round-%d", machine, round))}
			if err := store.Checkpoints().Put(ctx, "gh-acme", checkpoint); err != nil {
				t.Fatalf("round %d machine %d: Put checkpoint: %v", round, machine, err)
			}
			machines.appendJournal(machine, fmt.Sprintf(`{"ts":"2026-11-15T12:00:00Z","event":"round-%d","machine":%d}`, round, machine))
		}
		for machine := range stores {
			machines.sync(machine, fmt.Sprintf("round %d machine %d", round, machine))
		}
	}

	for machine := range stores {
		if tracked := strings.TrimSpace(machines.git(machines.dirs[machine], "ls-files", "--", "connectors")); tracked != "" {
			t.Fatalf("machine %d publishes its raw zone:\n%s", machine, tracked)
		}
		if status := strings.TrimSpace(machines.git(machines.dirs[machine], "status", "--porcelain")); status != "" {
			t.Fatalf("machine %d is not clean after the syncs:\n%s", machine, status)
		}
		if commits := strings.TrimSpace(machines.git(machines.dirs[machine], "rev-list", "--count", "HEAD")); commits == "" || commits == "1" {
			t.Fatalf("machine %d has no sync commits: %q", machine, commits)
		}
		last := rawZoneItem(rounds-1, time.Time{})
		stored, err := stores[machine].Items().Get(ctx, last.Ref)
		if err != nil || stored.Title != last.Title {
			t.Fatalf("machine %d lost item %s: %+v, %v", machine, last.Ref.ExternalID, stored, err)
		}
	}
	if tree := machines.git(machines.remote, "ls-tree", "-r", "--name-only", "HEAD"); strings.Contains(tree, "connectors/") {
		t.Fatalf("remote tree contains the raw zone:\n%s", tree)
	}
}

// TestSharedRawZonePublishesItemsWithoutConflicts: an explicit `publish: shared`
// connection publishes items and nothing else. Two machines that read the same
// item write the same bytes, so the shared file never conflicts.
func TestSharedRawZonePublishesItemsWithoutConflicts(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	stores := []*fsstore.Store{machines.store(0), machines.store(1)}
	base := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	for machine, store := range stores {
		if err := store.Items().SetPublishPolicy(ctx, "gh-acme", domain.PublishShared); err != nil {
			t.Fatalf("SetPublishPolicy: %v", err)
		}
		item := rawZoneItem(1, base.Add(time.Duration(machine)*time.Hour))
		if _, err := store.Items().Upsert(ctx, item); err != nil {
			t.Fatalf("machine %d: Upsert: %v", machine, err)
		}
		checkpoint := domain.Checkpoint{Stream: "issues", Version: 1, Data: []byte(fmt.Sprintf("cursor-%d", machine))}
		if err := store.Checkpoints().Put(ctx, "gh-acme", checkpoint); err != nil {
			t.Fatalf("machine %d: Put checkpoint: %v", machine, err)
		}
		machines.appendJournal(machine, fmt.Sprintf(`{"ts":"2026-11-15T12:00:00Z","event":"shared-%d"}`, machine))
	}
	for machine := range stores {
		machines.sync(machine, fmt.Sprintf("shared machine %d", machine))
	}

	for machine, store := range stores {
		tracked := strings.TrimSpace(machines.git(machines.dirs[machine], "ls-files", "--", "connectors/gh-acme"))
		if !strings.Contains(tracked, "connectors/gh-acme/.gitignore") {
			t.Fatalf("machine %d does not publish its shared marker:\n%s", machine, tracked)
		}
		if !strings.Contains(tracked, "connectors/gh-acme/items/") || strings.Contains(tracked, "connectors/gh-acme/checkpoints/") {
			t.Fatalf("machine %d tracked raw files:\n%s", machine, tracked)
		}
		item, err := store.Items().Get(ctx, domain.ItemRef{ConnectionID: "gh-acme", ExternalID: "issue-1"})
		if err != nil || !item.ObservedAt.IsZero() {
			t.Fatalf("machine %d item = %+v, %v", machine, item, err)
		}
	}
	first := readRawZoneItemFile(t, machines.dirs[0])
	second := readRawZoneItemFile(t, machines.dirs[1])
	if first != second {
		t.Fatalf("shared item differs between machines:\n--- a ---\n%s\n--- b ---\n%s", first, second)
	}
}

func readRawZoneItemFile(t *testing.T, dataDir string) string {
	t.Helper()
	dir := filepath.Join(dataDir, "connectors", "gh-acme", "items")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read items dir: %v", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("read item: %v", err)
		}
		return string(raw)
	}
	t.Fatalf("no item file in %s", dir)
	return ""
}

// TestDoctorWarnsAboutTrackedRawZone: doctor reports raw-zone files that the
// shared tree tracks, names them, and changes nothing.
func TestDoctorWarnsAboutTrackedRawZone(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	dataDir := machines.dirs[0]
	store := machines.store(0)
	item := rawZoneItem(1, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// A connection with no explicit publish setting is local-only: this is the
	// upgrade case where an earlier engine published the raw zone.
	connections, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatalf("NewConnectionStore: %v", err)
	}
	if err := connections.Create(ctx, domain.Connection{ID: "gh-acme", Provider: "github", Settings: map[string]string{}}); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	provider, err := gitstore.Open(dataDir, gitstore.Options{})
	if err != nil {
		t.Fatalf("gitstore.Open: %v", err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = dataDir
	cfg.SchemaVersion = config.CurrentSchemaVersion
	doctor := app.New(cfg, app.Dependencies{Store: store, Sync: provider, Connections: connections})

	clean, err := doctor.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if check := findDoctorCheck(t, clean, "raw-zone"); check.Status != "ok" {
		t.Fatalf("clean raw zone = %+v", check)
	}

	itemPath := filepath.Join(dataDir, "connectors", "gh-acme", "items", itemFileHexForTest(t, item)+".md")
	machines.git(dataDir, "add", "-f", itemPath)
	machines.git(dataDir, "commit", "-q", "-m", "tracked raw zone")

	report, err := doctor.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := findDoctorCheck(t, report, "raw-zone")
	if check.Status != "warn" || check.Code != "raw-zone-tracked" ||
		!strings.Contains(check.Message, "connectors/gh-acme/items/") || !strings.Contains(check.Message, "git rm --cached") {
		t.Fatalf("tracked raw zone = %+v", check)
	}
	if _, err := os.Stat(itemPath); err != nil {
		t.Fatalf("doctor changed the working tree: %v", err)
	}
}

// TestDoctorIgnoresTrackedSharedItems: a connection that explicitly publishes
// its items is not a finding, while its checkpoints are (cursors never leave
// the machine that read the stream).
func TestDoctorIgnoresTrackedSharedItems(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	dataDir := machines.dirs[0]
	store := machines.store(0)
	connections, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatalf("NewConnectionStore: %v", err)
	}
	connection := domain.Connection{ID: "gh-acme", Provider: "github", Publish: domain.PublishShared, Settings: map[string]string{}}
	if err := connections.Create(ctx, connection); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	if err := store.Items().SetPublishPolicy(ctx, "gh-acme", domain.PublishShared); err != nil {
		t.Fatalf("SetPublishPolicy: %v", err)
	}
	item := rawZoneItem(1, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	checkpoint := domain.Checkpoint{Stream: "issues", Version: 1, Data: []byte("cursor")}
	if err := store.Checkpoints().Put(ctx, "gh-acme", checkpoint); err != nil {
		t.Fatalf("Put checkpoint: %v", err)
	}
	itemPath := filepath.Join(dataDir, "connectors", "gh-acme", "items", itemFileHexForTest(t, item)+".md")
	machines.git(dataDir, "add", "-f", itemPath)
	machines.git(dataDir, "commit", "-q", "-m", "publish shared items")

	provider, err := gitstore.Open(dataDir, gitstore.Options{})
	if err != nil {
		t.Fatalf("gitstore.Open: %v", err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = dataDir
	cfg.SchemaVersion = config.CurrentSchemaVersion
	doctor := app.New(cfg, app.Dependencies{Store: store, Sync: provider, Connections: connections})
	report, err := doctor.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if check := findDoctorCheck(t, report, "raw-zone"); check.Status != "ok" {
		t.Fatalf("shared items reported: %+v", check)
	}

	checkpointDir := filepath.Join(dataDir, "connectors", "gh-acme", "checkpoints")
	entries, err := os.ReadDir(checkpointDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("checkpoint files: %v", err)
	}
	checkpointPath := filepath.Join(checkpointDir, entries[0].Name())
	machines.git(dataDir, "add", "-f", checkpointPath)
	machines.git(dataDir, "commit", "-q", "-m", "tracked checkpoints")

	report, err = doctor.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := findDoctorCheck(t, report, "raw-zone")
	if check.Status != "warn" || !strings.Contains(check.Message, "checkpoints/") {
		t.Fatalf("tracked checkpoint = %+v", check)
	}
}

func findDoctorCheck(t *testing.T, report app.DoctorReport, name string) app.DoctorCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("doctor has no %q check: %+v", name, report.Checks)
	return app.DoctorCheck{}
}

func itemFileHexForTest(t *testing.T, item domain.Item) string {
	t.Helper()
	_, hexPart, ok := strings.Cut(item.Key(), "/")
	if !ok {
		t.Fatalf("unexpected key %q", item.Key())
	}
	return hexPart
}
