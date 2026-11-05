package bootstrap_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestBuildLocalRuntime(t *testing.T) {
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
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = runtime.Close() }()
	if _, err := runtime.App.Status(ctx); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := runtime.App.Note(ctx, app.NoteInput{ProjectID: "demo", Body: "hello"}); err != nil {
		t.Fatalf("Note: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "index", "brain.db")); err != nil {
		t.Fatalf("index file missing: %v", err)
	}
}

func TestBuildMissingDataDirKeepsDiagnosticsUsable(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = filepath.Join(t.TempDir(), "missing")
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = runtime.Close() }()
	_, err = runtime.App.Note(context.Background(), app.NoteInput{ProjectID: "demo", Body: "x"})
	if !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("Note error = %v, want ErrUnsupported", err)
	}
	report, err := runtime.App.Doctor(context.Background(), app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	failed := false
	for _, check := range report.Checks {
		if check.Status == "fail" {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("doctor = %+v, want failing checks", report.Checks)
	}
}

type fakeSecretStore struct {
	values map[string]string
	gets   int
}

func (f *fakeSecretStore) Get(_ context.Context, key string) (string, error) {
	f.gets++
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

func newServerStub(t *testing.T, expectedToken string) (*httptest.Server, *string) {
	t.Helper()
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if expectedToken != "" && gotAuth != "Bearer "+expectedToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"authentication required"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(server.Close)
	return server, &gotAuth
}

func assertNoDoctorFailures(t *testing.T, report app.DoctorReport) {
	t.Helper()
	for _, check := range report.Checks {
		if check.Status == "fail" {
			t.Fatalf("doctor check failed: %+v", check)
		}
	}
}

func TestServerModeUsesBrainToken(t *testing.T) {
	ctx := context.Background()
	server, gotAuth := newServerStub(t, "env-token")
	t.Setenv("BRAIN_TOKEN", "env-token")
	fake := &fakeSecretStore{values: map[string]string{"server-token": "secret-token"}}
	cfg := config.Default()
	cfg.Mode = config.ModeServer
	cfg.ServerURL = server.URL
	runtime, err := bootstrap.BuildWithOptions(cfg, bootstrap.Options{Secrets: fake})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	status, err := runtime.App.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Mode != config.ModeServer || status.ServerURL != server.URL {
		t.Fatalf("status = %+v", status)
	}
	report, err := runtime.App.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	assertNoDoctorFailures(t, report)
	if *gotAuth != "Bearer env-token" {
		t.Fatalf("Authorization = %q", *gotAuth)
	}
	if fake.gets != 0 {
		t.Fatalf("secret store Get calls = %d, want 0 when BRAIN_TOKEN is set", fake.gets)
	}
}

func TestServerModeUsesSecretStoreToken(t *testing.T) {
	ctx := context.Background()
	server, gotAuth := newServerStub(t, "secret-token")
	t.Setenv("BRAIN_TOKEN", "")
	fake := &fakeSecretStore{values: map[string]string{"server-token": "secret-token"}}
	cfg := config.Default()
	cfg.Mode = config.ModeServer
	cfg.ServerURL = server.URL
	runtime, err := bootstrap.BuildWithOptions(cfg, bootstrap.Options{Secrets: fake})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	report, err := runtime.App.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	assertNoDoctorFailures(t, report)
	if *gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q", *gotAuth)
	}
	if fake.gets != 1 {
		t.Fatalf("secret store Get calls = %d, want 1", fake.gets)
	}
}

func TestServerModeDoctorReportsMissingToken(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"authentication required"}}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("BRAIN_TOKEN", "")
	fake := &fakeSecretStore{values: map[string]string{}}
	cfg := config.Default()
	cfg.Mode = config.ModeServer
	cfg.ServerURL = server.URL
	runtime, err := bootstrap.BuildWithOptions(cfg, bootstrap.Options{Secrets: fake})
	if err != nil {
		t.Fatalf("BuildWithOptions: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	report, err := runtime.App.Doctor(ctx, app.DoctorOptions{})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	found := false
	for _, check := range report.Checks {
		if check.Name == "server" && check.Status == "fail" {
			found = true
		}
	}
	if !found {
		t.Fatalf("doctor checks = %+v, want a failing server check", report.Checks)
	}
}

var _ ports.SecretStore = (*fakeSecretStore)(nil)

func TestBuildRejectsUnknownMode(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = "weird"
	if _, err := bootstrap.Build(cfg); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Build error = %v, want ErrValidation", err)
	}
}

func TestGitSyncWithTempRepository(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	runGitAt(t, filepath.Dir(remote), "init", "--quiet", "--bare", remote)
	runGitAt(t, remote, "--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitAt(t, filepath.Dir(seed), "init", "--quiet", seed)
	runGitAt(t, seed, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitAt(t, seed, "config", "user.name", "Brain Test")
	runGitAt(t, seed, "config", "user.email", "brain@example.test")
	writeFile(t, filepath.Join(seed, "README.md"), "seed\n")
	runGitAt(t, seed, "add", "--all")
	runGitAt(t, seed, "commit", "--quiet", "-m", "seed")
	runGitAt(t, seed, "remote", "add", "origin", remote)
	runGitAt(t, seed, "push", "--quiet", "-u", "origin", "main")

	clone := filepath.Join(root, "clone")
	runGitAt(t, root, "clone", "--quiet", remote, clone)
	runGitAt(t, clone, "config", "user.name", "Brain Test")
	runGitAt(t, clone, "config", "user.email", "brain@example.test")

	cfgPath := filepath.Join(clone, "brain.yaml")
	initCfg := config.Default()
	initCfg.Mode = config.ModeGit
	initCfg.DataDir = clone
	if _, err := app.New(initCfg, app.Dependencies{}).Init(ctx, app.InitOptions{
		ConfigPath: cfgPath, Mode: config.ModeGit, DataDir: clone,
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = runtime.Close() }()

	report, err := runtime.App.Doctor(ctx, app.DoctorOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	for _, check := range report.Checks {
		if check.Status == "fail" {
			t.Fatalf("doctor check failed: %+v", check)
		}
	}

	if _, err := runtime.App.Note(ctx, app.NoteInput{ProjectID: "demo", Title: "Git note", Body: "sync me"}); err != nil {
		t.Fatalf("Note: %v", err)
	}
	result, err := runtime.App.Sync(ctx, app.SyncOptions{Push: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Changed == 0 {
		t.Fatalf("Sync result = %+v", result)
	}

	status, err := runtime.App.Sync(ctx, app.SyncOptions{StatusOnly: true})
	if err != nil {
		t.Fatalf("Sync status: %v", err)
	}
	if status.Ahead != 0 || status.Behind != 0 {
		t.Fatalf("status after push = %+v", status)
	}

	logOut := runGitAt(t, root, "--git-dir="+remote, "log", "--oneline", "main")
	if !strings.Contains(logOut, "brain sync:") {
		t.Fatalf("remote log = %q", logOut)
	}
	tree := runGitAt(t, root, "--git-dir="+remote, "ls-tree", "-r", "--name-only", "main")
	for _, want := range []string{"brain.yaml", "registry.json", "projects/demo/state.md", "projects/demo/notes/"} {
		if !strings.Contains(tree, want) {
			t.Fatalf("remote tree missing %q:\n%s", want, tree)
		}
	}
	if strings.Contains(tree, "index/") {
		t.Fatalf("remote tree contains the derived index:\n%s", tree)
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not available")
	}
	if out, err := exec.Command(path, "--version").CombinedOutput(); err != nil {
		t.Skipf("git is not usable: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

func runGitAt(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
