package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

func TestLocalWorkflowEndToEnd(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")

	initCfg := config.Default()
	initCfg.Mode = config.ModeLocal
	initCfg.DataDir = dataDir
	initApp := app.New(initCfg, app.Dependencies{})
	result, err := initApp.Init(ctx, app.InitOptions{
		ConfigPath: cfgPath,
		Mode:       config.ModeLocal,
		DataDir:    dataDir,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if result.ConfigPath != cfgPath || result.DataDir != dataDir || result.Mode != config.ModeLocal {
		t.Fatalf("Init result = %+v", result)
	}

	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	project := domain.Project{ID: "demo", Name: "Demo Project", Kind: domain.ProjectKindApp}
	if err := store.PutProject(ctx, project); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	if _, err := store.PutState(ctx, domain.State{
		ProjectID:     project.ID,
		Goal:          "Ship WP7",
		Active:        []string{"CLI wiring"},
		NextAction:    "run tests",
		OpenQuestions: []string{"should sync push by default?"},
		FreshUntil:    time.Now().UTC().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("PutState: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		t.Fatalf("bootstrap.Build: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	noteResult, err := runtime.App.Note(ctx, app.NoteInput{
		ProjectID: project.ID,
		Title:     "First note",
		Body:      "hello world",
		Tags:      []string{"alpha"},
	})
	if err != nil {
		t.Fatalf("Note: %v", err)
	}
	if noteResult.Note.ID == "" || noteResult.Revision.Value == "" {
		t.Fatalf("note result = %+v", noteResult)
	}
	otherNote, err := runtime.App.Note(ctx, app.NoteInput{
		ProjectID: "other",
		Title:     "Other note",
		Body:      "hello other",
	})
	if err != nil {
		t.Fatalf("Note(other): %v", err)
	}

	decisionResult, err := runtime.App.Decision(ctx, app.DecisionInput{
		ProjectID:    project.ID,
		Title:        "Use SQLite",
		Status:       domain.DecisionAccepted,
		Context:      "local index",
		Alternatives: []string{"files only"},
		Consequences: []string{"derived index"},
		Evidence:     []string{"docs/design.md"},
	})
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	if decisionResult.Decision.ID == "" {
		t.Fatal("decision ID is empty")
	}

	attemptResult, err := runtime.App.Attempt(ctx, app.AttemptInput{
		ProjectID:  project.ID,
		Title:      "Cache warmup",
		Hypothesis: "warm cache helps",
		Result:     "no change",
		Reason:     "already warm",
	})
	if err != nil {
		t.Fatalf("Attempt: %v", err)
	}
	if attemptResult.Attempt.ID == "" {
		t.Fatal("attempt ID is empty")
	}

	handoffResult, err := runtime.App.Handoff(ctx, app.HandoffInput{
		ProjectID:  project.ID,
		Summary:    "wired the CLI",
		NextAction: "write tests",
	})
	if err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	if handoffResult.Handoff.ID == "" {
		t.Fatal("handoff ID is empty")
	}

	openResult, err := runtime.App.Open(ctx, app.OpenOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if openResult.Project.Name != project.Name || openResult.State.Goal != "Ship WP7" {
		t.Fatalf("Open result = %+v", openResult)
	}

	bootstrapResult, err := runtime.App.Bootstrap(ctx, app.BootstrapOptions{ProjectID: project.ID})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, want := range []string{
		"# Demo Project — bootstrap",
		"goal: Ship WP7",
		"freshness: fresh until",
		"Use SQLite",
		"Cache warmup",
		"wired the CLI",
	} {
		if !strings.Contains(bootstrapResult.Summary, want) {
			t.Fatalf("bootstrap summary missing %q:\n%s", want, bootstrapResult.Summary)
		}
	}

	reviewResult, err := runtime.App.Review(ctx, app.ReviewOptions{ProjectID: project.ID, Since: "7d"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	for _, want := range []string{
		"# Review — Demo Project",
		"state_age: ",
		"should sync push by default?",
		"docs/design.md",
		"already warm",
	} {
		if !strings.Contains(reviewResult.Summary, want) {
			t.Fatalf("review summary missing %q:\n%s", want, reviewResult.Summary)
		}
	}

	doctorReport, err := runtime.App.Doctor(ctx, app.DoctorOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	for _, check := range doctorReport.Checks {
		if check.Status == "fail" {
			t.Fatalf("doctor check failed: %+v", check)
		}
	}

	indexResult, err := runtime.App.Reindex(ctx, app.IndexOptions{})
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if indexResult.Projects != 2 || indexResult.States != 2 || indexResult.Notes != 2 ||
		indexResult.Decisions != 1 || indexResult.Attempts != 1 || indexResult.Handoffs != 1 ||
		indexResult.Documents != 7 {
		t.Fatalf("index result = %+v", indexResult)
	}
	searchHits, err := runtime.App.Search(ctx, "SQLite", project.ID, 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(searchHits) == 0 || searchHits[0].ID != decisionResult.Decision.ID {
		t.Fatalf("search hits = %+v", searchHits)
	}
	if _, err := runtime.App.Search(ctx, "  ", "", 5); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty search error = %v, want ErrValidation", err)
	}

	if err := runtime.Close(); err != nil {
		t.Fatalf("runtime.Close: %v", err)
	}
	runtime = nil

	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer func() { _ = index.Close() }()
	hits, err := index.Search(ctx, ports.Query{Text: "SQLite", ProjectID: project.ID})
	if err != nil {
		t.Fatalf("index search: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != decisionResult.Decision.ID {
		t.Fatalf("index hits = %+v", hits)
	}
	noteHits, err := index.Search(ctx, ports.Query{Text: "hello", ProjectID: project.ID})
	if err != nil {
		t.Fatalf("note index search: %v", err)
	}
	if len(noteHits) == 0 || noteHits[0].ID != noteResult.Note.ID {
		t.Fatalf("note index hits = %+v", noteHits)
	}
	allNoteHits, err := index.Search(ctx, ports.Query{Text: "hello", Limit: 10})
	if err != nil {
		t.Fatalf("cross-project index search: %v", err)
	}
	found := map[string]bool{}
	for _, hit := range allNoteHits {
		found[hit.ID] = true
	}
	if !found[noteResult.Note.ID] || !found[otherNote.Note.ID] {
		t.Fatalf("cross-project note hits = %+v", allNoteHits)
	}
}

func TestNoteAutoCreatesProject(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	initCfg := config.Default()
	initCfg.Mode = config.ModeLocal
	initCfg.DataDir = dataDir
	if _, err := app.New(initCfg, app.Dependencies{}).Init(ctx, app.InitOptions{
		ConfigPath: cfgPath, Mode: config.ModeLocal, DataDir: dataDir,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		t.Fatalf("bootstrap.Build: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	if _, err := runtime.App.Note(ctx, app.NoteInput{ProjectID: "auto", Body: "created implicitly"}); err != nil {
		t.Fatalf("Note: %v", err)
	}
	openResult, err := runtime.App.Open(ctx, app.OpenOptions{ProjectID: "auto"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if openResult.Project.ID != "auto" || openResult.Project.Name != "auto" {
		t.Fatalf("project = %+v", openResult.Project)
	}
	if openResult.State.ProjectID != "auto" {
		t.Fatalf("state = %+v", openResult.State)
	}
}

func TestBootstrapBudgetTruncates(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	initCfg := config.Default()
	initCfg.Mode = config.ModeLocal
	initCfg.DataDir = dataDir
	if _, err := app.New(initCfg, app.Dependencies{}).Init(ctx, app.InitOptions{
		ConfigPath: cfgPath, Mode: config.ModeLocal, DataDir: dataDir,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		t.Fatalf("bootstrap.Build: %v", err)
	}
	defer func() { _ = runtime.Close() }()
	for i := 0; i < 8; i++ {
		if _, err := runtime.App.Decision(ctx, app.DecisionInput{
			ProjectID: "demo",
			Title:     "Decision with a deliberately long title for budget testing",
			Context:   strings.Repeat("long context ", 20),
		}); err != nil {
			t.Fatalf("Decision %d: %v", i, err)
		}
	}
	result, err := runtime.App.Bootstrap(ctx, app.BootstrapOptions{ProjectID: "demo", Budget: 120})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if !strings.Contains(result.Summary, "(truncated)") {
		t.Fatalf("summary was not truncated:\n%s", result.Summary)
	}
	if len([]rune(result.Summary)) > 120 {
		t.Fatalf("summary rune length = %d, want <= 120", len([]rune(result.Summary)))
	}
}

func TestServerModeInitAndConfig(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "test-token")
	ctx := context.Background()
	cfgPath := filepath.Join(t.TempDir(), "brain.yaml")
	initCfg := config.Default()
	initCfg.Mode = config.ModeServer
	initCfg.ServerURL = "https://brain.example.test"
	result, err := app.New(initCfg, app.Dependencies{}).Init(ctx, app.InitOptions{
		ConfigPath: cfgPath,
		Mode:       config.ModeServer,
		ServerURL:  "https://brain.example.test",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if result.DataDir != "" || result.Mode != config.ModeServer {
		t.Fatalf("Init result = %+v", result)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.ServerURL != "https://brain.example.test" || cfg.DataDir != "" {
		t.Fatalf("config = %+v", cfg)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		t.Fatalf("bootstrap.Build: %v", err)
	}
	defer func() { _ = runtime.Close() }()
	status, err := runtime.App.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Mode != config.ModeServer || status.ServerURL != "https://brain.example.test" {
		t.Fatalf("status = %+v", status)
	}
	if runtime.Secrets == nil {
		t.Fatal("server runtime has no secret store")
	}
	if _, err := runtime.App.Sync(ctx, app.SyncOptions{}); !errors.Is(err, app.ErrNotImplemented) {
		t.Fatalf("Sync error = %v, want ErrNotImplemented", err)
	}
}

type fakeSecretStore struct {
	values map[string]string
}

func (f *fakeSecretStore) Get(_ context.Context, key string) (string, error) {
	value, ok := f.values[key]
	if !ok {
		return "", domain.ErrNotFound
	}
	return value, nil
}

func (f *fakeSecretStore) Set(_ context.Context, key, value string) error {
	if f.values == nil {
		f.values = make(map[string]string)
	}
	f.values[key] = value
	return nil
}

func (f *fakeSecretStore) Delete(_ context.Context, key string) error {
	delete(f.values, key)
	return nil
}

func (f *fakeSecretStore) Names(context.Context) ([]string, error) {
	names := make([]string, 0, len(f.values))
	for name := range f.values {
		names = append(names, name)
	}
	return names, nil
}

func TestSecretUseCases(t *testing.T) {
	ctx := context.Background()
	store := &fakeSecretStore{values: map[string]string{}}
	useCase := app.New(config.Default(), app.Dependencies{Secrets: store})

	name, err := useCase.SecretSet(ctx, "github/token", "secret-value")
	if err != nil {
		t.Fatalf("SecretSet: %v", err)
	}
	if name != "github/token" || store.values["github/token"] != "secret-value" {
		t.Fatalf("secret = %q, values = %+v", name, store.values)
	}
	names, err := useCase.SecretList(ctx)
	if err != nil {
		t.Fatalf("SecretList: %v", err)
	}
	if len(names) != 1 || names[0] != "github/token" {
		t.Fatalf("names = %v", names)
	}
	if err := useCase.SecretRemove(ctx, "github/token"); err != nil {
		t.Fatalf("SecretRemove: %v", err)
	}
	if len(store.values) != 0 {
		t.Fatalf("values after remove = %+v", store.values)
	}
	if _, err := app.New(config.Default(), app.Dependencies{}).SecretList(ctx); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("SecretList without store error = %v, want ErrUnsupported", err)
	}
}

func TestLocalSyncIsUnsupported(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = t.TempDir()
	_, err := app.New(cfg, app.Dependencies{}).Sync(context.Background(), app.SyncOptions{})
	if !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("Sync error = %v, want ErrUnsupported", err)
	}
}

type fakeUpdater struct {
	release     ports.ReleaseInfo
	checks      int
	downloads   int
	applies     int
	rollbacks   int
	rollbackErr error
	channel     string
}

func (f *fakeUpdater) Check(_ context.Context, channel string) (ports.ReleaseInfo, error) {
	f.checks++
	f.channel = channel
	return f.release, nil
}

func (f *fakeUpdater) Download(context.Context, ports.ReleaseInfo) (string, error) {
	f.downloads++
	return "staged-artifact", nil
}

func (f *fakeUpdater) Apply(context.Context, string) error {
	f.applies++
	return nil
}

func (f *fakeUpdater) Rollback(context.Context) error {
	f.rollbacks++
	return f.rollbackErr
}

func TestUpdateUseCase(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.UpdateChannel = config.ChannelStable

	t.Run("check only", func(t *testing.T) {
		updater := &fakeUpdater{release: ports.ReleaseInfo{Version: "9.9.9", Channel: "stable"}}
		useCase := app.New(cfg, app.Dependencies{Updater: updater})
		result, err := useCase.Update(ctx, app.UpdateOptions{Check: true})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updater.checks != 1 || updater.downloads != 0 || updater.applies != 0 {
			t.Fatalf("calls = %+v", updater)
		}
		if result.Applied || result.Latest.Version != "9.9.9" || result.Current.Version != version.Version {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("yes applies newer release", func(t *testing.T) {
		updater := &fakeUpdater{release: ports.ReleaseInfo{Version: "9.9.9", Channel: "beta"}}
		useCase := app.New(cfg, app.Dependencies{Updater: updater})
		result, err := useCase.Update(ctx, app.UpdateOptions{Yes: true, Channel: "beta"})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updater.channel != "beta" || updater.downloads != 1 || updater.applies != 1 {
			t.Fatalf("calls = %+v", updater)
		}
		if !result.Applied || result.Latest.Channel != "beta" {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("yes without newer release does nothing", func(t *testing.T) {
		updater := &fakeUpdater{release: ports.ReleaseInfo{Version: version.Version, Channel: "stable"}}
		useCase := app.New(cfg, app.Dependencies{Updater: updater})
		result, err := useCase.Update(ctx, app.UpdateOptions{Yes: true})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updater.downloads != 0 || updater.applies != 0 || result.Applied {
			t.Fatalf("calls = %+v, result = %+v", updater, result)
		}
	})

	t.Run("newer release without yes is informational", func(t *testing.T) {
		updater := &fakeUpdater{release: ports.ReleaseInfo{Version: "9.9.9", Channel: "stable"}}
		useCase := app.New(cfg, app.Dependencies{Updater: updater})
		result, err := useCase.Update(ctx, app.UpdateOptions{})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updater.downloads != 0 || updater.applies != 0 || result.Applied {
			t.Fatalf("calls = %+v, result = %+v", updater, result)
		}
	})

	t.Run("rollback restores previous binary without network", func(t *testing.T) {
		updater := &fakeUpdater{}
		useCase := app.New(cfg, app.Dependencies{Updater: updater})
		result, err := useCase.Update(ctx, app.UpdateOptions{Rollback: true})
		if err != nil {
			t.Fatalf("Update rollback: %v", err)
		}
		if updater.rollbacks != 1 || updater.checks != 0 || updater.downloads != 0 || updater.applies != 0 {
			t.Fatalf("calls = %+v", updater)
		}
		if !result.RolledBack || result.Applied {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("rollback rejects check and yes", func(t *testing.T) {
		updater := &fakeUpdater{}
		useCase := app.New(cfg, app.Dependencies{Updater: updater})
		for _, opts := range []app.UpdateOptions{
			{Rollback: true, Check: true},
			{Rollback: true, Yes: true},
		} {
			if _, err := useCase.Update(ctx, opts); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Update(%+v) error = %v, want validation", opts, err)
			}
		}
		if updater.rollbacks != 0 || updater.checks != 0 {
			t.Fatalf("calls = %+v", updater)
		}
	})

	t.Run("rollback error is reported", func(t *testing.T) {
		boom := errors.New("no previous binary")
		updater := &fakeUpdater{rollbackErr: boom}
		useCase := app.New(cfg, app.Dependencies{Updater: updater})
		if _, err := useCase.Update(ctx, app.UpdateOptions{Rollback: true}); !errors.Is(err, boom) {
			t.Fatalf("Update rollback error = %v, want %v", err, boom)
		}
	})

	t.Run("rollback without updater is not implemented", func(t *testing.T) {
		useCase := app.New(cfg, app.Dependencies{})
		_, err := useCase.Update(ctx, app.UpdateOptions{Rollback: true})
		if !errors.Is(err, app.ErrNotImplemented) {
			t.Fatalf("Update error = %v, want ErrNotImplemented", err)
		}
	})
}

type fakeSync struct {
	status    ports.SyncStatus
	pull      ports.SyncResult
	push      ports.SyncResult
	pullErr   error
	calls     []string
	commitMsg string
}

func (f *fakeSync) Status(context.Context) (ports.SyncStatus, error) {
	f.calls = append(f.calls, "status")
	return f.status, nil
}

func (f *fakeSync) Pull(context.Context, ports.SyncOptions) (ports.SyncResult, error) {
	f.calls = append(f.calls, "pull")
	return f.pull, f.pullErr
}

func (f *fakeSync) Commit(_ context.Context, message string) (domain.SyncRevision, error) {
	f.calls = append(f.calls, "commit")
	f.commitMsg = message
	return domain.SyncRevision{Kind: domain.SyncKindGit, Value: "commit"}, nil
}

func (f *fakeSync) Push(context.Context) (ports.SyncResult, error) {
	f.calls = append(f.calls, "push")
	return f.push, nil
}

func TestSyncUseCase(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Mode = config.ModeGit
	cfg.DataDir = t.TempDir()

	statusOnly := &fakeSync{status: ports.SyncStatus{Provider: "git", Branch: "main", Ahead: 1, Behind: 2, Dirty: true}}
	useCase := app.New(cfg, app.Dependencies{Sync: statusOnly})
	result, err := useCase.Sync(ctx, app.SyncOptions{StatusOnly: true})
	if err != nil {
		t.Fatalf("Sync status: %v", err)
	}
	if len(statusOnly.calls) != 1 || statusOnly.calls[0] != "status" {
		t.Fatalf("calls = %v", statusOnly.calls)
	}
	if result.Ahead != 1 || result.Behind != 2 {
		t.Fatalf("result = %+v", result)
	}

	sync := &fakeSync{
		status: ports.SyncStatus{Provider: "git", Branch: "main", Dirty: true},
		pull:   ports.SyncResult{Changed: 1},
		push:   ports.SyncResult{Changed: 2},
	}
	useCase = app.New(cfg, app.Dependencies{Sync: sync})
	result, err = useCase.Sync(ctx, app.SyncOptions{Push: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	wantCalls := []string{"status", "commit", "pull", "push", "status"}
	if strings.Join(sync.calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %v, want %v", sync.calls, wantCalls)
	}
	if !strings.HasPrefix(sync.commitMsg, "brain sync: ") {
		t.Fatalf("commit message = %q", sync.commitMsg)
	}
	if result.Changed != 2 || result.Provider != "git" {
		t.Fatalf("result = %+v", result)
	}

	conflict := &fakeSync{
		status:  ports.SyncStatus{Provider: "git", Branch: "main"},
		pull:    ports.SyncResult{Conflicts: []string{"projects/demo/state.md"}},
		pullErr: domain.ErrConflict,
	}
	useCase = app.New(cfg, app.Dependencies{Sync: conflict})
	result, err = useCase.Sync(ctx, app.SyncOptions{})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Sync error = %v, want ErrConflict", err)
	}
	if len(result.Conflicts) != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestDoctorReportsMissingInstance(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	report, err := app.New(cfg, app.Dependencies{}).Doctor(context.Background(), app.DoctorOptions{ConfigPath: filepath.Join(root, "missing.yaml")})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	failed := 0
	for _, check := range report.Checks {
		if check.Status == "fail" {
			failed++
		}
	}
	if failed == 0 {
		t.Fatalf("expected failing checks: %+v", report.Checks)
	}
}
