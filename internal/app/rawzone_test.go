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

	"github.com/levapveeskela/lv24-brain/internal/adapters/connectionlock"
	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
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
	return newRawZoneMachinesN(t, 2)
}

// newRawZoneMachinesN creates one bare remote and n independent clones, each
// initialized as a brain data directory. Existing tests use two; the
// three-machine concurrency test uses three.
func newRawZoneMachinesN(t *testing.T, count int) *rawZoneMachines {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if count < 2 {
		t.Fatalf("newRawZoneMachinesN: count = %d, want at least 2", count)
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
	machines.dirs = []string{first}

	for i := 1; i < count; i++ {
		dir := filepath.Join(root, fmt.Sprintf("machine-%d", i))
		machines.git(root, "clone", "-q", machines.remote, dir)
		machines.dirs = append(machines.dirs, dir)
	}
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

// catalogue opens the connection catalogue of one machine.
func (m *rawZoneMachines) catalogue(machine int) *fsstore.ConnectionStore {
	m.t.Helper()
	catalogue, err := fsstore.NewConnectionStore(m.dirs[machine])
	if err != nil {
		m.t.Fatalf("NewConnectionStore: %v", err)
	}
	return catalogue
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

// legacyMarker is the connectors/<id>/.gitignore an engine before 0.15 wrote
// for a shared connection: lock and temporary files only.
const legacyMarker = "# lv24-brain: shared connection; only lock and temporary files are ignored.\n*.lock\n.tmp-*\n"

// rawZoneSource is a minimal ports.Source returning its batches once.
type rawZoneSource struct {
	descriptor domain.SourceDescriptor
	items      []domain.Item
	reads      int
}

func (s *rawZoneSource) Descriptor() domain.SourceDescriptor { return s.descriptor }
func (s *rawZoneSource) Check(context.Context) error         { return nil }
func (s *rawZoneSource) Read(context.Context, ports.ReadRequest) (ports.ReadBatch, error) {
	s.reads++
	if s.reads > 1 {
		return ports.ReadBatch{}, nil
	}
	return ports.ReadBatch{Items: s.items}, nil
}

// rawZoneHost wires the real ingestion host (fsstore raw zone, connection
// catalogue, git publish hooks) for one machine.
func newRawZoneHost(t *testing.T, dataDir string, source *rawZoneSource) *app.Ingestion {
	t.Helper()
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	catalogue, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatalf("NewConnectionStore: %v", err)
	}
	locker, err := connectionlock.New(dataDir)
	if err != nil {
		t.Fatalf("connectionlock.New: %v", err)
	}
	provider, err := gitstore.Open(dataDir, gitstore.Options{})
	if err != nil {
		t.Fatalf("gitstore.Open: %v", err)
	}
	registry := connectorbuild.NewRegistry()
	if err := registry.Register(source.Descriptor(), func(domain.Connection, connectorbuild.Deps) (ports.Source, error) {
		return source, nil
	}); err != nil {
		t.Fatalf("register source: %v", err)
	}
	return &app.Ingestion{
		Connections: catalogue, Registry: registry,
		Items: store.Items(), Checkpoints: store.Checkpoints(), Locker: locker, Runs: store,
		PreparePublish: provider.PrepareConnectionPublish,
		RawZoneTracked: provider.RawZoneTracked,
		Now:            func() time.Time { return time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC) },
	}
}

// rawZoneDescriptor is the provider descriptor these fixtures register.
func rawZoneDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider: "sample", DisplayName: "Sample",
		Capabilities:   domain.Capabilities{AuthModes: []domain.AuthMode{domain.AuthNone}},
		DefaultPublish: domain.PublishShared,
		Streams:        []string{"inbox"},
	}
}

// seedLegacyRawZone writes the raw zone an engine before 0.15 published: the
// item keeps observed_at and both the item and the legacy marker are committed.
func seedLegacyRawZone(t *testing.T, machines *rawZoneMachines, machine int, connectionID string, item domain.Item) (string, string) {
	t.Helper()
	ctx := context.Background()
	store := machines.store(machine)
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}
	dataDir := machines.dirs[machine]
	itemPath := filepath.Join(dataDir, "connectors", connectionID, "items", itemFileHex(t, item)+".md")
	markerPath := filepath.Join(dataDir, "connectors", connectionID, ".gitignore")
	if err := os.WriteFile(markerPath, []byte(legacyMarker), 0o644); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}
	machines.git(dataDir, "add", "-f", itemPath, markerPath)
	machines.git(dataDir, "commit", "-q", "-m", "legacy raw zone")
	return itemPath, markerPath
}

// TestLegacyTrackedRawZoneStaysSharedAcrossRun: a connection without an
// explicit policy whose raw zone is already in git keeps everything exactly as
// it is (marker, index and item bytes) and only reports a recommendation.
func TestLegacyTrackedRawZoneStaysSharedAcrossRun(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	if err := machines.catalogue(0).Create(ctx, domain.Connection{ID: "gh-acme", Provider: "sample", Settings: map[string]string{}}); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	item := rawZoneItem(1, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC))
	itemPath, markerPath := seedLegacyRawZone(t, machines, 0, "gh-acme", item)
	itemBefore := readRawZoneFile(t, itemPath)
	trackedBefore := strings.TrimSpace(machines.git(machines.dirs[0], "ls-files", "--", "connectors/gh-acme"))

	source := &rawZoneSource{descriptor: rawZoneDescriptor(), items: []domain.Item{item}}
	host := newRawZoneHost(t, machines.dirs[0], source)
	for attempt := 1; attempt <= 2; attempt++ {
		result, err := host.RunConnection(ctx, app.RunConnectionOptions{ConnectionID: "gh-acme"})
		if err != nil {
			t.Fatalf("run %d: %v", attempt, err)
		}
		if result.Publish != domain.PublishShared || result.PublishChanged {
			t.Fatalf("run %d: publish=%q changed=%v", attempt, result.Publish, result.PublishChanged)
		}
		if !strings.Contains(result.PublishNotice, "grandfathered") || !strings.Contains(result.PublishNotice, "brain connection publish gh-acme") {
			t.Fatalf("run %d: notice = %q", attempt, result.PublishNotice)
		}
	}
	if trackedAfter := strings.TrimSpace(machines.git(machines.dirs[0], "ls-files", "--", "connectors/gh-acme")); trackedAfter != trackedBefore {
		t.Fatalf("grandfathered zone was untracked:\nbefore:\n%s\nafter:\n%s", trackedBefore, trackedAfter)
	}
	if marker := readRawZoneFile(t, markerPath); marker != legacyMarker {
		t.Fatalf("marker was rewritten:\n%s", marker)
	}
	if itemAfter := readRawZoneFile(t, itemPath); itemAfter != itemBefore {
		t.Fatalf("item file was rewritten:\n%s", itemAfter)
	}
}

// TestNewConnectionRawZoneDefaultsLocalOnly: a connection with no tracked raw
// zone and no explicit policy publishes nothing and writes the local marker.
func TestNewConnectionRawZoneDefaultsLocalOnly(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	if err := machines.catalogue(0).Create(ctx, domain.Connection{ID: "gh-acme", Provider: "sample", Settings: map[string]string{}}); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	source := &rawZoneSource{descriptor: rawZoneDescriptor(), items: []domain.Item{rawZoneItem(1, time.Now().UTC())}}
	host := newRawZoneHost(t, machines.dirs[0], source)
	result, err := host.RunConnection(ctx, app.RunConnectionOptions{ConnectionID: "gh-acme"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Publish != domain.PublishLocalOnly || result.PublishNotice != "" {
		t.Fatalf("result = %+v", result)
	}
	marker := readRawZoneFile(t, filepath.Join(machines.dirs[0], "connectors", "gh-acme", ".gitignore"))
	if !strings.Contains(marker, "\n*\n") {
		t.Fatalf("marker = %q, want the local-only marker", marker)
	}
	if tracked := strings.TrimSpace(machines.git(machines.dirs[0], "ls-files", "--", "connectors")); tracked != "" {
		t.Fatalf("new connection published files:\n%s", tracked)
	}
}

// TestExplicitLocalOnlyUntracksTrackedRawZone: the explicit switch is the only
// path that untracks files; they stay on disk.
func TestExplicitLocalOnlyUntracksTrackedRawZone(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	connection := domain.Connection{ID: "gh-acme", Provider: "sample", Publish: domain.PublishLocalOnly, Settings: map[string]string{}}
	if err := machines.catalogue(0).Create(ctx, connection); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	item := rawZoneItem(1, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC))
	itemPath, _ := seedLegacyRawZone(t, machines, 0, "gh-acme", item)

	source := &rawZoneSource{descriptor: rawZoneDescriptor(), items: []domain.Item{item}}
	host := newRawZoneHost(t, machines.dirs[0], source)
	result, err := host.RunConnection(ctx, app.RunConnectionOptions{ConnectionID: "gh-acme"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Publish != domain.PublishLocalOnly || !result.PublishChanged || result.PublishNotice != "" {
		t.Fatalf("result = %+v", result)
	}
	if tracked := strings.TrimSpace(machines.git(machines.dirs[0], "ls-files", "--", "connectors/gh-acme")); tracked != "" {
		t.Fatalf("explicit local-only still tracks:\n%s", tracked)
	}
	if _, err := os.Stat(itemPath); err != nil {
		t.Fatalf("untrack removed the file: %v", err)
	}
	marker := readRawZoneFile(t, filepath.Join(machines.dirs[0], "connectors", "gh-acme", ".gitignore"))
	if !strings.Contains(marker, "\n*\n") {
		t.Fatalf("marker = %q, want the local-only marker", marker)
	}
}

// TestExplicitSharedKeepsItemsAndUntracksCheckpoints: publishing items never
// publishes cursors.
func TestExplicitSharedKeepsItemsAndUntracksCheckpoints(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	connection := domain.Connection{ID: "gh-acme", Provider: "sample", Publish: domain.PublishShared, Settings: map[string]string{}}
	if err := machines.catalogue(0).Create(ctx, connection); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	item := rawZoneItem(1, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC))
	itemPath, _ := seedLegacyRawZone(t, machines, 0, "gh-acme", item)
	store := machines.store(0)
	if err := store.Checkpoints().Put(ctx, "gh-acme", domain.Checkpoint{Stream: "inbox", Version: 1, Data: []byte("cursor")}); err != nil {
		t.Fatalf("Put checkpoint: %v", err)
	}
	checkpointDir := filepath.Join(machines.dirs[0], "connectors", "gh-acme", "checkpoints")
	entries, err := os.ReadDir(checkpointDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("checkpoint files: %v", err)
	}
	checkpointPath := filepath.Join(checkpointDir, entries[0].Name())
	machines.git(machines.dirs[0], "add", "-f", checkpointPath)
	machines.git(machines.dirs[0], "commit", "-q", "-m", "legacy checkpoint")

	source := &rawZoneSource{descriptor: rawZoneDescriptor(), items: []domain.Item{item}}
	host := newRawZoneHost(t, machines.dirs[0], source)
	result, err := host.RunConnection(ctx, app.RunConnectionOptions{ConnectionID: "gh-acme"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Publish != domain.PublishShared || result.PublishNotice != "" {
		t.Fatalf("result = %+v", result)
	}
	tracked := machines.git(machines.dirs[0], "ls-files", "--", "connectors/gh-acme")
	if !strings.Contains(tracked, "connectors/gh-acme/items/") || strings.Contains(tracked, "connectors/gh-acme/checkpoints/") {
		t.Fatalf("tracked raw files:\n%s", tracked)
	}
	for _, path := range []string{itemPath, checkpointPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("untrack removed %s: %v", path, err)
		}
	}
	marker := readRawZoneFile(t, filepath.Join(machines.dirs[0], "connectors", "gh-acme", ".gitignore"))
	if !strings.Contains(marker, "checkpoints/") {
		t.Fatalf("marker = %q, want a shared marker that ignores checkpoints", marker)
	}
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
		connection := domain.Connection{ID: "gh-acme", Provider: "sample", Publish: domain.PublishShared, Settings: map[string]string{}}
		if err := machines.catalogue(machine).Create(ctx, connection); err != nil {
			t.Fatalf("machine %d: Create connection: %v", machine, err)
		}
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

// doctorFor opens doctor over one machine's git repository.
func doctorFor(t *testing.T, machines *rawZoneMachines, machine int) *app.App {
	t.Helper()
	dataDir := machines.dirs[machine]
	store := machines.store(machine)
	provider, err := gitstore.Open(dataDir, gitstore.Options{})
	if err != nil {
		t.Fatalf("gitstore.Open: %v", err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = dataDir
	cfg.SchemaVersion = config.CurrentSchemaVersion
	return app.New(cfg, app.Dependencies{Store: store, Sync: provider, Connections: machines.catalogue(machine)})
}

// TestDoctorWarnsAboutGrandfatheredRawZone: a tracked zone without an explicit
// policy is reported as compatibility mode with the command that fixes it, and
// nothing is changed.
func TestDoctorWarnsAboutGrandfatheredRawZone(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	if err := machines.catalogue(0).Create(ctx, domain.Connection{ID: "gh-acme", Provider: "sample", Settings: map[string]string{}}); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	item := rawZoneItem(1, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC))
	itemPath, _ := seedLegacyRawZone(t, machines, 0, "gh-acme", item)

	report, err := doctorFor(t, machines, 0).Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := findDoctorCheck(t, report, "raw-zone-policy")
	if check.Status != "warn" || check.Code != "raw-zone-grandfathered" || !strings.Contains(check.Message, "brain connection publish") {
		t.Fatalf("grandfathered check = %+v", check)
	}
	if check := findDoctorCheck(t, report, "raw-zone"); check.Status != "ok" {
		t.Fatalf("leftovers check = %+v", check)
	}
	if _, err := os.Stat(itemPath); err != nil {
		t.Fatalf("doctor changed the working tree: %v", err)
	}
}

// TestDoctorWarnsAboutLocalOnlyTrackedRawZone: the explicit local-only case is
// a leftovers finding that recommends untracking.
func TestDoctorWarnsAboutLocalOnlyTrackedRawZone(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	connection := domain.Connection{ID: "gh-acme", Provider: "sample", Publish: domain.PublishLocalOnly, Settings: map[string]string{}}
	if err := machines.catalogue(0).Create(ctx, connection); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	item := rawZoneItem(1, time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC))
	itemPath, _ := seedLegacyRawZone(t, machines, 0, "gh-acme", item)
	doctor := doctorFor(t, machines, 0)

	report, err := doctor.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := findDoctorCheck(t, report, "raw-zone")
	if check.Status != "warn" || check.Code != "raw-zone-tracked" || !strings.Contains(check.Message, "git rm --cached") {
		t.Fatalf("leftovers check = %+v", check)
	}
	if check := findDoctorCheck(t, report, "raw-zone-policy"); check.Status != "ok" {
		t.Fatalf("policy check = %+v", check)
	}
	machines.git(machines.dirs[0], "rm", "--cached", "-r", "-q", "--", "connectors/gh-acme")
	report, err = doctor.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor after untrack: %v", err)
	}
	for _, name := range []string{"raw-zone", "raw-zone-policy"} {
		if check := findDoctorCheck(t, report, name); check.Status != "ok" {
			t.Fatalf("%s after untrack = %+v", name, check)
		}
	}
	if _, err := os.Stat(itemPath); err != nil {
		t.Fatalf("untrack removed the file: %v", err)
	}
}

// TestDoctorIgnoresTrackedSharedItems: an explicit shared connection may publish
// items, but its checkpoints are still a finding.
func TestDoctorIgnoresTrackedSharedItems(t *testing.T) {
	machines := newRawZoneMachines(t)
	ctx := context.Background()
	connection := domain.Connection{ID: "gh-acme", Provider: "sample", Publish: domain.PublishShared, Settings: map[string]string{}}
	if err := machines.catalogue(0).Create(ctx, connection); err != nil {
		t.Fatalf("Create connection: %v", err)
	}
	store := machines.store(0)
	if err := store.Items().SetPublishPolicy(ctx, "gh-acme", domain.PublishShared); err != nil {
		t.Fatalf("SetPublishPolicy: %v", err)
	}
	item := rawZoneItem(1, time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	itemPath := filepath.Join(machines.dirs[0], "connectors", "gh-acme", "items", itemFileHex(t, item)+".md")
	machines.git(machines.dirs[0], "add", "-f", itemPath)
	machines.git(machines.dirs[0], "commit", "-q", "-m", "publish shared items")
	doctor := doctorFor(t, machines, 0)

	report, err := doctor.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	for _, name := range []string{"raw-zone", "raw-zone-policy"} {
		if check := findDoctorCheck(t, report, name); check.Status != "ok" {
			t.Fatalf("%s with shared items = %+v", name, check)
		}
	}

	if err := store.Checkpoints().Put(ctx, "gh-acme", domain.Checkpoint{Stream: "inbox", Version: 1, Data: []byte("cursor")}); err != nil {
		t.Fatalf("Put checkpoint: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(machines.dirs[0], "connectors", "gh-acme", "checkpoints"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("checkpoint files: %v", err)
	}
	machines.git(machines.dirs[0], "add", "-f", filepath.Join(machines.dirs[0], "connectors", "gh-acme", "checkpoints", entries[0].Name()))
	machines.git(machines.dirs[0], "commit", "-q", "-m", "tracked checkpoints")

	report, err = doctor.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	check := findDoctorCheck(t, report, "raw-zone")
	if check.Status != "warn" || !strings.Contains(check.Message, "checkpoints") {
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

func readRawZoneFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func itemFileHex(t *testing.T, item domain.Item) string {
	t.Helper()
	_, hexPart, ok := strings.Cut(item.Key(), "/")
	if !ok {
		t.Fatalf("unexpected key %q", item.Key())
	}
	return hexPart
}
