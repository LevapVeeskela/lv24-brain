package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

func TestMain(m *testing.M) {
	// CLI tests must never reach the release endpoint or write the user's
	// update state. Tests that need the daily check can clear this variable.
	_ = os.Setenv("BRAIN_NO_UPDATE", "1")
	// Never resolve the developer's real default config (`brain config use`)
	// or BRAIN_CONFIG: a flagless command in a test would write to live data.
	isolated, err := os.MkdirTemp("", "brain-cli-test-home-")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"APPDATA", "XDG_CONFIG_HOME", "HOME"} {
		_ = os.Setenv(name, isolated)
	}
	_ = os.Unsetenv("BRAIN_CONFIG")
	code := m.Run()
	_ = os.RemoveAll(isolated)
	os.Exit(code)
}

func runForTest(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func runWithInputForTest(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runWithInput(args, strings.NewReader(stdin), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

const (
	testAccessToken  = "cli-access-token"
	testRefreshToken = "cli-refresh-token"
)

type cliSecretStore struct {
	mu     sync.Mutex
	values map[string]string
}

func newCLISecretStore() *cliSecretStore {
	return &cliSecretStore{values: map[string]string{}}
}

func (s *cliSecretStore) Get(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	if !ok {
		return "", domain.ErrNotFound
	}
	return value, nil
}

func (s *cliSecretStore) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	return nil
}

func (s *cliSecretStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	return nil
}

func (s *cliSecretStore) Names(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.values))
	for name := range s.values {
		names = append(names, name)
	}
	return names, nil
}

func (s *cliSecretStore) lookup(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	return value, ok
}

func (s *cliSecretStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values = map[string]string{}
}

func useSecretStore(t *testing.T, store ports.SecretStore) {
	t.Helper()
	previous := buildRuntime
	buildRuntime = func(cfg config.BrainConfig) (*bootstrap.Runtime, error) {
		return bootstrap.BuildWithOptions(cfg, bootstrap.Options{Secrets: store})
	}
	previousOS := osSecrets
	osSecrets = func() ports.SecretStore { return store }
	t.Cleanup(func() {
		buildRuntime = previous
		osSecrets = previousOS
	})
}

// storedSession mirrors the CLI-facing part of the httpclient session bundle.
type storedSession struct {
	ServerURL    string `json:"serverUrl"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
}

func storeSession(t *testing.T, store *cliSecretStore, serverURL, access, refresh string) {
	t.Helper()
	raw, err := json.Marshal(storedSession{ServerURL: serverURL, AccessToken: access, RefreshToken: refresh})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.values[httpclient.SecretSession] = string(raw)
}

func lookupSession(t *testing.T, store *cliSecretStore) (storedSession, bool) {
	t.Helper()
	raw, ok := store.lookup(httpclient.SecretSession)
	if !ok {
		return storedSession{}, false
	}
	var session storedSession
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		t.Fatalf("stored session is not JSON: %v", err)
	}
	return session, true
}

func writeServerConfig(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := config.Default()
	cfg.Mode = config.ModeServer
	cfg.ServerURL = serverURL
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return path
}

func newAuthServer(t *testing.T, wantLogin, wantPassword string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/login":
			var body struct {
				Login    string `json:"login"`
				Password string `json:"password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Login != wantLogin || body.Password != wantPassword {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_credentials","message":"invalid credentials"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accessToken":  testAccessToken,
				"refreshToken": testRefreshToken,
				"expiresAt":    time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
				"user":         map[string]any{"id": "user_1", "login": wantLogin, "role": "member"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/auth/me":
			if r.Header.Get("Authorization") != "Bearer "+testAccessToken {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"authentication required"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user":       map[string]any{"id": "user_1", "login": wantLogin, "role": "member"},
				"authSource": "jwt",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/logout":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

var _ ports.SecretStore = (*cliSecretStore)(nil)

func TestLoginCommand(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "")
	t.Setenv("BRAIN_LOGIN", "")
	t.Setenv("BRAIN_PASSWORD", "")
	server := newAuthServer(t, "alice", "s3cret")
	cfgPath := writeServerConfig(t, server.URL)
	store := newCLISecretStore()
	useSecretStore(t, store)

	code, stdout, stderr := runWithInputForTest(t, "s3cret\n", "login", "--config", cfgPath, "--login", "alice", "--password-stdin")
	if code != 0 {
		t.Fatalf("login exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"logged in as alice", "member", "expires 2026-11-06T10:00:00Z"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("login stdout missing %q:\n%s", want, stdout)
		}
	}
	session, ok := lookupSession(t, store)
	if !ok {
		t.Fatal("login did not store a session bundle")
	}
	if session.ServerURL != server.URL || session.AccessToken != testAccessToken || session.RefreshToken != testRefreshToken {
		t.Fatalf("stored session = %+v", session)
	}
	if _, ok := store.lookup(httpclient.SecretAccessToken); ok {
		t.Fatal("login stored the legacy access key")
	}
	if _, ok := store.lookup(httpclient.SecretRefreshToken); ok {
		t.Fatal("login stored the legacy refresh key")
	}
	if combined := stdout + stderr; strings.Contains(combined, "s3cret") ||
		strings.Contains(combined, testAccessToken) || strings.Contains(combined, testRefreshToken) {
		t.Fatalf("login output leaked a credential: %q", combined)
	}

	store.reset()
	code, _, stderr = runWithInputForTest(t, "wrong\n", "login", "--config", cfgPath, "--login", "alice", "--password-stdin")
	if code == 0 {
		t.Fatal("login with a wrong password succeeded")
	}
	if !strings.Contains(stderr, "brain login:") {
		t.Fatalf("wrong password stderr = %q", stderr)
	}
	if _, ok := store.lookup(httpclient.SecretSession); ok {
		t.Fatal("failed login stored a session bundle")
	}

	code, stdout, stderr = runWithInputForTest(t, "s3cret\n", "login", "--config", cfgPath, "--login", "alice", "--password-stdin")
	if code != 0 || !strings.Contains(stdout, "logged in as alice") {
		t.Fatalf("password-stdin login exit code = %d, stdout = %q (stderr: %s)", code, stdout, stderr)
	}

}

func TestLoginRejectsPasswordFlag(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "")
	t.Setenv("BRAIN_SECRET_SERVER_TOKEN", "")
	t.Setenv("BRAIN_LOGIN", "")
	t.Setenv("BRAIN_PASSWORD", "")
	cfgPath := writeServerConfig(t, "http://127.0.0.1:1")

	code, _, stderr := runForTest(t, "login", "--config", cfgPath, "--login", "alice", "--password", "x")
	if code != 2 || !strings.Contains(stderr, "-password") {
		t.Fatalf("--password exit code = %d, stderr = %q; want exit 2 with unknown-flag error", code, stderr)
	}
}

func TestLoginUsageErrorsExitTwo(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "")
	t.Setenv("BRAIN_SECRET_SERVER_TOKEN", "")
	t.Setenv("BRAIN_LOGIN", "")
	t.Setenv("BRAIN_PASSWORD", "")
	cfgPath := writeServerConfig(t, "http://127.0.0.1:1")

	code, _, stderr := runWithInputForTest(t, "x\n", "login", "--config", cfgPath, "--password-stdin")
	if code != 2 || !strings.Contains(stderr, "--login is required") {
		t.Fatalf("missing login exit code = %d, stderr = %q", code, stderr)
	}
	code, _, stderr = runForTest(t, "login", "--config", cfgPath, "--login", "alice", "--password-stdin", "extra")
	if code != 2 || !strings.Contains(stderr, "unexpected arguments") {
		t.Fatalf("positional arguments exit code = %d, stderr = %q", code, stderr)
	}
	code, _, stderr = runForTest(t, "login", "--config", cfgPath, "--login", "alice")
	if code != 2 || !strings.Contains(stderr, "password is required") {
		t.Fatalf("missing password exit code = %d, stderr = %q", code, stderr)
	}
}

func TestLogoutStaticTokenWarnsAndClearsLocalTokens(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "")
	t.Setenv("BRAIN_SECRET_SERVER_TOKEN", "")
	var logoutCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/auth/me":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user":       map[string]any{"id": "user_1", "login": "alice", "role": "member"},
				"authSource": "api_token",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/logout":
			logoutCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cfgPath := writeServerConfig(t, server.URL)
	store := newCLISecretStore()
	store.values[httpclient.SecretAccessToken] = "static-api-token"
	store.values[httpclient.SecretRefreshToken] = "static-refresh"
	useSecretStore(t, store)

	code, _, stderr := runForTest(t, "logout", "--config", cfgPath)
	if code != 1 {
		t.Fatalf("logout exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "static API token") || !strings.Contains(stderr, "local tokens were cleared") {
		t.Fatalf("logout stderr = %q", stderr)
	}
	if logoutCalls != 0 {
		t.Fatalf("logout endpoint called %d times for a static token", logoutCalls)
	}
	if _, ok := store.lookup(httpclient.SecretAccessToken); ok {
		t.Fatal("logout kept the access token")
	}
	if _, ok := store.lookup(httpclient.SecretRefreshToken); ok {
		t.Fatal("logout kept the refresh token")
	}
	if _, ok := store.lookup(httpclient.SecretSession); ok {
		t.Fatal("logout kept the session bundle")
	}
}

func TestWarnPlainHTTPLogin(t *testing.T) {
	var warning bytes.Buffer
	warnPlainHTTPLogin(&warning, "http://brain.example.test")
	if !strings.Contains(warning.String(), "plain HTTP") {
		t.Fatalf("warning = %q", warning.String())
	}
	for _, safe := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080"} {
		warning.Reset()
		warnPlainHTTPLogin(&warning, safe)
		if warning.Len() != 0 {
			t.Fatalf("%s produced a warning: %q", safe, warning.String())
		}
	}
}

func TestLoginRefusesSecretEnvToken(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "")
	t.Setenv("BRAIN_SECRET_SERVER_TOKEN", "env-secret-token")
	t.Setenv("BRAIN_LOGIN", "")
	t.Setenv("BRAIN_PASSWORD", "")
	cfgPath := writeServerConfig(t, "http://127.0.0.1:1")

	code, _, stderr := runWithInputForTest(t, "x\n", "login", "--config", cfgPath, "--login", "alice", "--password-stdin")
	if code != 1 || !strings.Contains(stderr, "BRAIN_SECRET_SERVER_TOKEN") {
		t.Fatalf("login exit code = %d, stderr = %q", code, stderr)
	}
}

func TestLogoutEnvTokenWarnsWhenStoredTokensExist(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "")
	t.Setenv("BRAIN_SECRET_SERVER_TOKEN", "env-secret-token")
	cfgPath := writeServerConfig(t, "http://127.0.0.1:1")
	store := newCLISecretStore()
	storeSession(t, store, "http://127.0.0.1:1", testAccessToken, testRefreshToken)
	useSecretStore(t, store)

	code, stdout, stderr := runForTest(t, "logout", "--config", cfgPath)
	if code != 1 {
		t.Fatalf("logout exit code = %d, want 1 (stdout: %q)", code, stdout)
	}
	if !strings.Contains(stderr, "left unchanged") || !strings.Contains(stderr, "Unset it and rerun") {
		t.Fatalf("logout stderr = %q", stderr)
	}
	if strings.Contains(stdout, "logged out") {
		t.Fatalf("env-token logout claimed success: %q", stdout)
	}
	if _, ok := store.lookup(httpclient.SecretSession); !ok {
		t.Fatal("logout cleared the session bundle despite an env-provided static token")
	}
}

func TestLogoutEnvTokenWithoutStoredTokensSucceeds(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "env-token")
	t.Setenv("BRAIN_SECRET_SERVER_TOKEN", "")
	cfgPath := writeServerConfig(t, "http://127.0.0.1:1")
	useSecretStore(t, newCLISecretStore())

	code, stdout, stderr := runForTest(t, "logout", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("logout exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "static server token") {
		t.Fatalf("logout stdout = %q", stdout)
	}
}

func TestLogoutCommandClearsStoredTokens(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "")
	server := newAuthServer(t, "alice", "s3cret")
	cfgPath := writeServerConfig(t, server.URL)
	store := newCLISecretStore()
	storeSession(t, store, server.URL, testAccessToken, testRefreshToken)
	useSecretStore(t, store)

	code, stdout, stderr := runForTest(t, "logout", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("logout exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "logged out") {
		t.Fatalf("logout stdout = %q", stdout)
	}
	if _, ok := store.lookup(httpclient.SecretSession); ok {
		t.Fatal("logout kept the session bundle")
	}
	if _, ok := store.lookup(httpclient.SecretAccessToken); ok {
		t.Fatal("logout kept the legacy access token")
	}

	code, stdout, stderr = runForTest(t, "logout", "--config", cfgPath)
	if code != 0 || !strings.Contains(stdout, "logged out") {
		t.Fatalf("second logout exit code = %d, stdout = %q (stderr: %s)", code, stdout, stderr)
	}
}

func TestUpdateRollbackValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "brain")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"update", "--rollback", "--yes", "--config", path},
		{"update", "--rollback", "--check", "--config", path},
	} {
		code, _, stderr := runForTest(t, args...)
		if code != 2 {
			t.Fatalf("%v exit code = %d, want 2", args, code)
		}
		if !strings.Contains(stderr, "rollback cannot be combined") {
			t.Fatalf("%v stderr = %q", args, stderr)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	code, stdout, stderr := runForTest(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "brain "+version.Version) {
		t.Fatalf("stdout = %q, want version %q", stdout, version.Version)
	}
}

func TestHelpCommand(t *testing.T) {
	code, stdout, _ := runForTest(t, "help")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, command := range []string{"init", "doctor", "note", "sync", "index", "search", "connector", "migrate", "secret", "login", "logout", "serve", "update"} {
		if !strings.Contains(stdout, command) {
			t.Fatalf("help output missing command %q", command)
		}
	}
	if !strings.Contains(stdout, "--rollback") {
		t.Fatalf("help output missing --rollback:\n%s", stdout)
	}
}

func TestNoArgumentsIsUsageError(t *testing.T) {
	code, _, stderr := runForTest(t)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr = %q, want usage", stderr)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, stderr := runForTest(t, "frobnicate")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Fatalf("stderr = %q, want unknown command", stderr)
	}
}

func TestStatusReadsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := config.Default()
	cfg.Mode = config.ModeServer
	cfg.ServerURL = "https://brain.example.com"
	cfg.User = "alex"
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runForTest(t, "status", "--config", path)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"mode: server", "server_url: https://brain.example.com", "user: alex", "engine_version: " + version.Version} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestInvalidFlagExitsTwo(t *testing.T) {
	code, _, _ := runForTest(t, "status", "--nope")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestInitWritesConfigIntoDataDir(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	code, stdout, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "initialized local brain") {
		t.Fatalf("stdout = %q", stdout)
	}
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	if cfg.Mode != config.ModeLocal || cfg.DataDir != dataDir {
		t.Fatalf("config = %+v", cfg)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "projects")); err != nil {
		t.Fatalf("projects directory missing: %v", err)
	}
}

func TestLocalWorkflowCLI(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")

	code, stdout, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir)
	if code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "initialized local brain") {
		t.Fatalf("init stdout = %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "note", "--create", "--config", cfgPath, "--project", "demo", "--title", "First note", "--body", "hello world", "--tags", "alpha, beta")
	if code != 0 {
		t.Fatalf("note exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.HasPrefix(stdout, "note note-") {
		t.Fatalf("note stdout = %q", stdout)
	}

	code, _, stderr = runForTest(t, "decision", "--config", cfgPath, "--project", "demo",
		"--title", "Use SQLite", "--status", "accepted", "--context", "local index",
		"--alternatives", "files only", "--consequences", "derived index", "--evidence", "docs/design.md")
	if code != 0 {
		t.Fatalf("decision exit code = %d (stderr: %s)", code, stderr)
	}

	code, _, stderr = runForTest(t, "attempt", "--config", cfgPath, "--project", "demo",
		"--title", "Cache warmup", "--hypothesis", "warm cache helps", "--result", "no change",
		"--reason", "already warm")
	if code != 0 {
		t.Fatalf("attempt exit code = %d (stderr: %s)", code, stderr)
	}

	code, _, stderr = runForTest(t, "handoff", "--config", cfgPath, "--project", "demo",
		"--summary", "wired the CLI", "--next", "write tests")
	if code != 0 {
		t.Fatalf("handoff exit code = %d (stderr: %s)", code, stderr)
	}

	code, stdout, stderr = runForTest(t, "open", "--config", cfgPath, "--project", "demo")
	if code != 0 {
		t.Fatalf("open exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"# demo — bootstrap", "Use SQLite", "## Git", "(not a repository)", "## Graphs", "(none)"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("open stdout missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = runForTest(t, "open", "--brief", "--config", cfgPath, "demo")
	if code != 0 {
		t.Fatalf("open --brief exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"project: demo (demo)", "goal: not set", "state_updated: "} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("open stdout missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = runForTest(t, "bootstrap", "--config", cfgPath, "--project", "demo")
	if code != 0 {
		t.Fatalf("bootstrap exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"# demo — bootstrap", "Use SQLite", "Cache warmup", "wired the CLI"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("bootstrap stdout missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = runForTest(t, "review", "--config", cfgPath, "--project", "demo", "--since", "7d")
	if code != 0 {
		t.Fatalf("review exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"# Review — demo", "docs/design.md", "already warm"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("review stdout missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = runForTest(t, "doctor", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("doctor exit code = %d (stderr: %s)\n%s", code, stderr, stdout)
	}
	for _, want := range []string{"ok: config", "ok: data_dir", "ok: schema", "ok: index", "ok: git"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("doctor stdout missing %q:\n%s", want, stdout)
		}
	}

	code, _, stderr = runForTest(t, "sync", "--config", cfgPath, "--status")
	if code != 1 || !strings.Contains(stderr, "local mode has no remote") {
		t.Fatalf("sync exit code = %d, stderr = %q", code, stderr)
	}

	for _, rel := range []string{
		"projects/demo/state.md",
		"projects/demo/notes",
		"projects/demo/decisions",
		"projects/demo/attempts",
		"projects/demo/handoffs",
		"index/brain.db",
	} {
		if _, err := os.Stat(filepath.Join(dataDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("expected %s: %v", rel, err)
		}
	}
}

func TestSecretCommandEnvironmentFallback(t *testing.T) {
	t.Setenv("BRAIN_SECRET_GITHUB_TOKEN", "from-environment")
	code, stdout, stderr := runWithInputForTest(t, "new-value\n", "secret", "set", "github/token")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want no secret value/name", stdout)
	}
	if !strings.Contains(stderr, "provided by the environment") {
		t.Fatalf("stderr = %q", stderr)
	}
	if strings.Contains(stderr, "new-value") || strings.Contains(stderr, "from-environment") {
		t.Fatalf("stderr leaked a secret value: %q", stderr)
	}

	code, _, stderr = runForTest(t, "secret", "remove", "github/token")
	if code != 2 || !strings.Contains(stderr, "pass --yes") {
		t.Fatalf("unconfirmed remove exit code = %d, stderr = %q", code, stderr)
	}
	code, _, stderr = runForTest(t, "secret", "remove", "github/token", "--yes")
	if code != 1 || !strings.Contains(stderr, "cannot be deleted") {
		t.Fatalf("remove exit code = %d, stderr = %q", code, stderr)
	}

	code, _, _ = runWithInputForTest(t, "\n", "secret", "set", "github/token")
	if code != 1 {
		t.Fatalf("empty value exit code = %d, want 1", code)
	}
	code, _, _ = runForTest(t, "secret")
	if code != 2 {
		t.Fatalf("missing subcommand exit code = %d, want 2", code)
	}
	code, _, _ = runForTest(t, "secret", "list", "extra")
	if code != 2 {
		t.Fatalf("extra argument exit code = %d, want 2", code)
	}
}

func TestIndexAndSearchCLI(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	if code, _, stderr := runForTest(t, "note", "--create", "--config", cfgPath, "--project", "demo", "--title", "Search note", "--body", "searchable needle"); code != 0 {
		t.Fatalf("note exit code = %d (stderr: %s)", code, stderr)
	}
	if code, _, stderr := runForTest(t, "decision", "--config", cfgPath, "--project", "demo", "--title", "Search decision", "--evidence", "needle evidence"); code != 0 {
		t.Fatalf("decision exit code = %d (stderr: %s)", code, stderr)
	}

	code, stdout, stderr := runForTest(t, "index", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("index exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"projects: 1", "states: 1", "notes: 1", "decisions: 1", "documents: 3", "rebuilt: false"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("index stdout missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = runForTest(t, "search", "--config", cfgPath, "--project", "demo", "--limit", "1", "searchable")
	if code != 0 {
		t.Fatalf("search exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "note/demo/") || !strings.Contains(stdout, "searchable") {
		t.Fatalf("search stdout = %q", stdout)
	}
	code, stdout, stderr = runForTest(t, "search", "searchable", "--config", cfgPath, "--project", "demo")
	if code != 0 || !strings.Contains(stdout, "note/demo/") {
		t.Fatalf("query-first search stdout = %q, code = %d (stderr: %s)", stdout, code, stderr)
	}

	code, _, _ = runForTest(t, "search", "--config", cfgPath)
	if code != 2 {
		t.Fatalf("empty search exit code = %d, want 2", code)
	}
	code, _, _ = runForTest(t, "search", "--config", cfgPath, "   ")
	if code != 1 {
		t.Fatalf("whitespace search exit code = %d, want 1", code)
	}

	index, err := sqliteindex.Open(filepath.Join(dataDir, "index", "brain.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	if err := index.Upsert(context.Background(), ports.Document{
		ID:        "stale:demo",
		ProjectID: "demo",
		Kind:      "note",
		Title:     "Stale note",
		Body:      "staleword",
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert stale document: %v", err)
	}
	if err := index.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}
	code, stdout, _ = runForTest(t, "search", "--config", cfgPath, "staleword")
	if code != 0 || !strings.Contains(stdout, "Stale note") {
		t.Fatalf("stale search stdout = %q, code = %d", stdout, code)
	}

	code, stdout, stderr = runForTest(t, "index", "--config", cfgPath, "--rebuild")
	if code != 0 {
		t.Fatalf("rebuild exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "rebuilt: true") {
		t.Fatalf("rebuild stdout = %q", stdout)
	}
	code, stdout, _ = runForTest(t, "search", "--config", cfgPath, "staleword")
	if code != 0 || stdout != "" {
		t.Fatalf("stale search after rebuild = %q, code = %d", stdout, code)
	}
	code, stdout, _ = runForTest(t, "search", "--config", cfgPath, "searchable")
	if code != 0 || !strings.Contains(stdout, "note/demo/") {
		t.Fatalf("search after rebuild = %q, code = %d", stdout, code)
	}

	code, stdout, stderr = runForTest(t, "search", "--config", cfgPath, "--kind", "decision", "needle")
	if code != 0 || !strings.Contains(stdout, "decision/demo/") || strings.Contains(stdout, "note/demo/") {
		t.Fatalf("--kind decision stdout = %q, code = %d (stderr: %s)", stdout, code, stderr)
	}
	code, stdout, stderr = runForTest(t, "search", "--config", cfgPath, "--kind", "bogus", "needle")
	if code != 0 || stdout != "" {
		t.Fatalf("--kind bogus stdout = %q, code = %d (stderr: %s)", stdout, code, stderr)
	}
	code, stdout, stderr = runForTest(t, "search", "--config", cfgPath, "--dumps", "--kind", "note,decision", "needle")
	if code != 0 || !strings.Contains(stdout, "note/demo/") || !strings.Contains(stdout, "decision/demo/") {
		t.Fatalf("--dumps --kind stdout = %q, code = %d (stderr: %s)", stdout, code, stderr)
	}
	code, stdout, stderr = runForTest(t, "search", "--config", cfgPath, "--dumps", "needle")
	if code != 0 || !strings.Contains(stdout, "note/demo/") {
		t.Fatalf("--dumps stdout = %q, code = %d (stderr: %s)", stdout, code, stderr)
	}
}

func TestConnectorRunCLI(t *testing.T) {
	const token = "ghp_cli_connector_test_token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets":
			_, _ = w.Write([]byte(`{
				"full_name":"acme/widgets","description":"Widgets",
				"html_url":"https://github.com/acme/widgets","default_branch":"main",
				"private":false,"archived":false,"fork":false,
				"stargazers_count":42,"forks_count":7,"open_issues_count":3,
				"updated_at":"2026-11-05T10:00:00Z"
			}`))
		case "/repos/acme/widgets/pulls":
			_, _ = w.Write([]byte(`[
				{"number":2,"title":"Second PR","state":"open","html_url":"https://github.com/acme/widgets/pull/2",
				 "user":{"login":"alice"},"draft":false,"created_at":"2026-11-01T10:00:00Z",
				 "updated_at":"2026-11-04T10:00:00Z","labels":[{"name":"ready"}],
				 "head":{"ref":"feature"},"base":{"ref":"main"}}
			]`))
		case "/repos/acme/widgets/issues":
			_, _ = w.Write([]byte(`[
				{"number":5,"title":"Bug report","state":"open","html_url":"https://github.com/acme/widgets/issues/5",
				 "user":{"login":"carol"},"comments":2,"created_at":"2026-11-02T10:00:00Z",
				 "updated_at":"2026-11-05T09:00:00Z","labels":[{"name":"bug"}]}
			]`))
		case "/repos/acme/widgets/commits":
			_, _ = w.Write([]byte(`[
				{"sha":"abcdef1234567890","html_url":"https://github.com/acme/widgets/commit/abcdef1",
				 "commit":{"author":{"name":"Alice","email":"alice@example.test","date":"2026-11-05T08:00:00Z"},
				 "message":"Fix widgets"},"author":{"login":"alice"}}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	t.Setenv("BRAIN_SECRET_GITHUB_TOKEN", token)
	t.Setenv("BRAIN_GITHUB_BASE_URL", server.URL)
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	code, stdout, stderr := runForTest(t, "connector", "run", "--name", "github", "--project", "acme/widgets",
		"--since", "2026-06-01T00:00:00Z", "--limit", "10", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("connector exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"source: github", "status: success", "count repos: 1"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("connector stdout missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout+stderr, token) {
		t.Fatalf("connector output leaked the token")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "github", "acme", "widgets.md")); err != nil {
		t.Fatalf("connector dump missing: %v", err)
	}
	runs, err := os.ReadDir(filepath.Join(dataDir, "connectors", "runs"))
	if err != nil || len(runs) == 0 {
		t.Fatalf("connector run record missing: %v", err)
	}

	code, _, _ = runForTest(t, "connector")
	if code != 2 {
		t.Fatalf("connector without subcommand exit code = %d, want 2", code)
	}
	code, _, _ = runForTest(t, "connector", "run", "--name", "bogus", "--config", cfgPath)
	if code != 2 {
		t.Fatalf("connector invalid name exit code = %d, want 2", code)
	}
	code, _, _ = runForTest(t, "connector", "run", "--name", "github", "--since", "not-a-time", "--config", cfgPath)
	if code != 2 {
		t.Fatalf("connector invalid since exit code = %d, want 2", code)
	}
}

func TestConnectorRunServerModeCLI(t *testing.T) {
	const token = "brain_server_connector_test"
	var got struct {
		Name     string   `json:"name"`
		Limit    int      `json:"limit"`
		Projects []string `json:"projects"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/connectors/run" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if gotToken := r.Header.Get("Authorization"); gotToken != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(domain.ConnectorRun{
			ID:        "run-server-1",
			Source:    "github",
			Status:    domain.ConnectorSuccess,
			StartedAt: time.Now().UTC(),
			Counts:    map[string]int{"repos": 2},
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv("BRAIN_TOKEN", token)

	root := t.TempDir()
	cfgPath := filepath.Join(root, "brain-server.yaml")
	code, _, stderr := runForTest(t, "init", "--mode", "server", "--server-url", server.URL, "--config", cfgPath)
	if code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	code, stdout, stderr := runForTest(t, "connector", "run", "--name", "github", "--limit", "5", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("connector exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"source: github", "status: success", "count repos: 2"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("connector stdout missing %q:\n%s", want, stdout)
		}
	}
	if got.Name != "github" || got.Limit != 5 {
		t.Fatalf("request = %+v", got)
	}
}

func TestConnectorIMAPFailsClosedCLI(t *testing.T) {
	for _, key := range []string{"BRAIN_IMAP_ACCOUNT", "BRAIN_IMAP_ADDRESS", "BRAIN_IMAP_USERNAME"} {
		t.Setenv(key, "")
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	code, _, stderr := runForTest(t, "connector", "run", "--name", "imap", "--config", cfgPath)
	if code != 1 {
		t.Fatalf("connector imap exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "BRAIN_IMAP_ACCOUNT") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestMigrateCLI(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "legacy")
	target := filepath.Join(root, "data")
	projectDir := filepath.Join(source, "projects", "demo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := `---
project_id: demo
kind: state
---

# Demo — State

## Goal

legacy goal
`
	if err := os.WriteFile(filepath.Join(projectDir, "state.md"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := map[string]any{
		"version": 1,
		"projects": []any{map[string]any{
			"id": "demo", "path": filepath.Join(root, "demo-repo"), "kind": "monorepo",
			"repo": "https://example.test/demo.git", "branch": "main",
		}},
	}
	rawRegistry, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "registry.json"), rawRegistry, 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runForTest(t, "migrate", "--from", source, "--data-dir", target, "--dry-run")
	if code != 0 {
		t.Fatalf("dry-run exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "created   1") || !strings.Contains(stdout, "conflicts 0") {
		t.Fatalf("dry-run stdout = %q", stdout)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("dry run created target directory: %v", err)
	}

	code, stdout, stderr = runForTest(t, "migrate", "--from", source, "--data-dir", target)
	if code != 0 {
		t.Fatalf("migrate exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "created   1") {
		t.Fatalf("migrate stdout = %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(target, "projects", "demo", "state.md")); err != nil {
		t.Fatalf("migrated state missing: %v", err)
	}
	assertMigratedRegistryMetadata(t, target, filepath.Join(root, "demo-repo"))
	cfg, err := config.Load(filepath.Join(target, "brain.yaml"))
	if err != nil {
		t.Fatalf("load migrated config: %v", err)
	}
	if cfg.SchemaVersion != 1 || cfg.Mode != config.ModeLocal || cfg.DataDir != target {
		t.Fatalf("migrated config = %+v", cfg)
	}
	code, stdout, stderr = runForTest(t, "open", "--brief", "--project", "demo", "--config", filepath.Join(target, "brain.yaml"))
	if code != 0 {
		t.Fatalf("open after migrate exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "project: demo (demo)") {
		t.Fatalf("open after migrate stdout = %q", stdout)
	}
	code, stdout, stderr = runForTest(t, "bootstrap", "--project", "demo", "--config", filepath.Join(target, "brain.yaml"))
	if code != 0 {
		t.Fatalf("bootstrap after migrate exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "legacy goal") {
		t.Fatalf("bootstrap after migrate stdout = %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "migrate", "--from", source, "--data-dir", target)
	if code != 0 {
		t.Fatalf("second migrate exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "created   0") || !strings.Contains(stdout, "unchanged 1") {
		t.Fatalf("second migrate stdout = %q", stdout)
	}

	if err := os.WriteFile(filepath.Join(target, "projects", "demo", "state.md"), []byte("different\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runForTest(t, "migrate", "--from", source, "--data-dir", target)
	if code != 1 {
		t.Fatalf("conflict exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout, "conflicts 1") {
		t.Fatalf("conflict stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "conflict") {
		t.Fatalf("conflict stderr = %q", stderr)
	}

	code, _, _ = runForTest(t, "migrate", "--nope")
	if code != 2 {
		t.Fatalf("unknown flag exit code = %d, want 2", code)
	}
	code, _, stderr = runForTest(t, "migrate", "--data-dir", target)
	if code != 2 || !strings.Contains(stderr, "--from is required") {
		t.Fatalf("missing --from exit code = %d, stderr = %q", code, stderr)
	}
}

func assertMigratedRegistryMetadata(t *testing.T, targetDir, wantPath string) {
	t.Helper()
	var registry struct {
		Projects []struct {
			ID            string   `json:"id"`
			Paths         []string `json:"paths"`
			Kind          string   `json:"kind"`
			RepoURL       string   `json:"repo_url"`
			DefaultBranch string   `json:"default_branch"`
		} `json:"projects"`
	}
	raw, err := os.ReadFile(filepath.Join(targetDir, "registry.json"))
	if err != nil {
		t.Fatalf("read migrated registry: %v", err)
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		t.Fatalf("parse migrated registry: %v", err)
	}
	for _, project := range registry.Projects {
		if project.ID != "demo" {
			continue
		}
		if len(project.Paths) != 1 || project.Paths[0] != wantPath {
			t.Fatalf("migrated paths = %v, want %q", project.Paths, wantPath)
		}
		if project.Kind != string(domain.ProjectKindRepository) {
			t.Fatalf("migrated kind = %q, want repository", project.Kind)
		}
		if project.RepoURL != "https://example.test/demo.git" || project.DefaultBranch != "main" {
			t.Fatalf("migrated repo metadata = %+v", project)
		}
		return
	}
	t.Fatal("migrated registry has no demo project")
}

func TestMigratePreservesRelativeDataDir(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "legacy")
	target := filepath.Join(root, "data")
	projectDir := filepath.Join(source, "projects", "demo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := "---\nproject_id: demo\nkind: state\n---\n\n# Demo\n"
	if err := os.WriteFile(filepath.Join(projectDir, "state.md"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(target, "brain.yaml")
	configRaw := "mode: local\ndata_dir: \".\"\nuser: tester\nschema_version: 1\nupdate_channel: stable\n"
	if err := os.WriteFile(configPath, []byte(configRaw), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runForTest(t, "migrate", "--from", source, "--config", configPath)
	if code != 0 {
		t.Fatalf("migrate exit code = %d (stderr: %s)", code, stderr)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(raw), `data_dir: "."`) && !strings.Contains(string(raw), "data_dir: .") {
		t.Fatalf("relative data_dir was not preserved:\n%s", raw)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.DataDir != target {
		t.Fatalf("resolved data_dir = %q, want %q", cfg.DataDir, target)
	}
}

func TestMigrateKeepsCuratedProjectMetadata(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "legacy")
	target := filepath.Join(root, "data")
	projectDir := filepath.Join(source, "projects", "demo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "state.md"), []byte("---\nproject_id: demo\nkind: state\n---\n\n# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRegistry := map[string]any{
		"version": 1,
		"projects": []any{map[string]any{
			"id": "demo", "path": "C:\\legacy\\demo", "kind": "monorepo",
			"repo": "https://legacy.example/demo.git", "branch": "legacy",
		}},
	}
	rawSource, _ := json.Marshal(sourceRegistry)
	if err := os.WriteFile(filepath.Join(source, "registry.json"), rawSource, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	curated := map[string]any{
		"version": 1,
		"projects": []any{map[string]any{
			"id": "demo", "name": "Curated", "paths": []string{"C:\\curated\\demo"},
			"kind": "app", "repo_url": "https://curated.example/demo.git",
			"default_branch": "trunk", "created_at": "2026-01-01T00:00:00Z",
			"updated_at": "2026-01-01T00:00:00Z",
		}},
	}
	rawCurated, _ := json.Marshal(curated)
	if err := os.WriteFile(filepath.Join(target, "registry.json"), rawCurated, 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := runForTest(t, "migrate", "--from", source, "--data-dir", target)
	if code != 0 {
		t.Fatalf("migrate exit code = %d (stderr: %s)", code, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(target, "registry.json"))
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	var registry struct {
		Projects []struct {
			ID            string   `json:"id"`
			Name          string   `json:"name"`
			Paths         []string `json:"paths"`
			Kind          string   `json:"kind"`
			RepoURL       string   `json:"repo_url"`
			DefaultBranch string   `json:"default_branch"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(raw, &registry); err != nil {
		t.Fatalf("parse registry: %v", err)
	}
	if len(registry.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(registry.Projects))
	}
	project := registry.Projects[0]
	if project.Name != "Curated" || len(project.Paths) != 1 || project.Paths[0] != "C:\\curated\\demo" {
		t.Fatalf("curated metadata overwritten: %+v", project)
	}
	if project.Kind != "app" || project.RepoURL != "https://curated.example/demo.git" || project.DefaultBranch != "trunk" {
		t.Fatalf("curated metadata overwritten: %+v", project)
	}
}

type fakeUpdater struct {
	release ports.ReleaseInfo
	err     error
	checked string
}

func (f *fakeUpdater) Check(_ context.Context, channel string) (ports.ReleaseInfo, error) {
	f.checked = channel
	return f.release, f.err
}

func (f *fakeUpdater) Download(context.Context, ports.ReleaseInfo) (string, error) { return "", nil }

func (f *fakeUpdater) Apply(context.Context, string) error { return nil }

func (f *fakeUpdater) Rollback(context.Context) error { return nil }

func TestUpdateUsesRuntimeSeamAndValidatesArgs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := config.Default()
	cfg.DataDir = filepath.Join(t.TempDir(), "brain")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	updater := &fakeUpdater{release: ports.ReleaseInfo{Version: "9.9.9", Channel: config.ChannelStable}}
	previous := buildRuntime
	buildRuntime = func(cfg config.BrainConfig) (*bootstrap.Runtime, error) {
		return &bootstrap.Runtime{App: app.New(cfg, app.Dependencies{Updater: updater})}, nil
	}
	t.Cleanup(func() { buildRuntime = previous })

	code, stdout, stderr := runForTest(t, "update", "--check", "--config", path)
	if code != 0 {
		t.Fatalf("update --check exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "latest: 9.9.9") || !strings.Contains(stdout, "applied: false") {
		t.Fatalf("update stdout = %q", stdout)
	}
	if updater.checked != config.ChannelStable {
		t.Fatalf("checked channel = %q", updater.checked)
	}

	code, _, stderr = runForTest(t, "update", "extra", "--config", path)
	if code != 2 || !strings.Contains(stderr, "unexpected arguments") {
		t.Fatalf("positional update exit code = %d, stderr = %q", code, stderr)
	}
	code, _, stderr = runForTest(t, "update", "--rollback", "--channel", "beta", "--config", path)
	if code != 2 || !strings.Contains(stderr, "--channel cannot be combined") {
		t.Fatalf("channel+rollback exit code = %d, stderr = %q", code, stderr)
	}
}

type cliDeleteFailStore struct {
	*cliSecretStore
	failKey string
}

func (s *cliDeleteFailStore) Delete(ctx context.Context, key string) error {
	if key == s.failKey {
		return errors.New("keychain locked")
	}
	return s.cliSecretStore.Delete(ctx, key)
}

func TestSearchServerModeCLI(t *testing.T) {
	const token = "brain_server_search_test"
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/search" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if gotToken := r.Header.Get("Authorization"); gotToken != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotQuery = r.URL.Query().Get("q") + "|" + r.URL.Query().Get("projectId")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"ID":"note-1","ProjectID":"demo","Kind":"note","Title":"Release","Snippet":"ship it","Score":1}]}`)
	}))
	t.Cleanup(server.Close)
	t.Setenv("BRAIN_TOKEN", token)

	cfgPath := writeServerConfig(t, server.URL)
	code, stdout, stderr := runForTest(t, "search", "release", "--project", "demo", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("search exit code = %d (stderr: %s)", code, stderr)
	}
	if gotQuery != "release|demo" {
		t.Fatalf("server query = %q, want release|demo", gotQuery)
	}
	if !strings.Contains(stdout, "note/demo/Release") || !strings.Contains(stdout, "ship it") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func newStaticTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/auth/me" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user":       map[string]any{"id": "user_1", "login": "alice", "role": "member"},
				"authSource": "api_token",
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestLogoutStaticTokenCleanupFailureIsReported(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "")
	t.Setenv("BRAIN_SECRET_SERVER_TOKEN", "")
	cfgPath := writeServerConfig(t, newStaticTokenServer(t).URL)
	store := &cliDeleteFailStore{cliSecretStore: newCLISecretStore(), failKey: httpclient.SecretAccessToken}
	store.values[httpclient.SecretAccessToken] = "static-api-token"
	useSecretStore(t, store)

	code, _, stderr := runForTest(t, "logout", "--config", cfgPath)
	if code != 1 {
		t.Fatalf("logout exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "not fully cleared") {
		t.Fatalf("logout stderr = %q, want a cleanup failure", stderr)
	}
	if strings.Contains(stderr, "local tokens were cleared") {
		t.Fatalf("logout claimed the failed cleanup succeeded: %q", stderr)
	}
}

func TestLogoutWrongServerLeavesTokens(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "")
	t.Setenv("BRAIN_SECRET_SERVER_TOKEN", "")
	cfgPath := writeServerConfig(t, "https://server-b.example.test")
	store := newCLISecretStore()
	storeSession(t, store, "https://server-a.example.test", testAccessToken, testRefreshToken)
	useSecretStore(t, store)

	code, _, stderr := runForTest(t, "logout", "--config", cfgPath)
	if code != 1 {
		t.Fatalf("logout exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "different server") || !strings.Contains(stderr, "server-a.example.test") {
		t.Fatalf("logout stderr = %q", stderr)
	}
	if _, ok := lookupSession(t, store); !ok {
		t.Fatal("logout cleared server A's session bundle")
	}
}

func TestLoginInvalidPasswordMessage(t *testing.T) {
	t.Setenv("BRAIN_TOKEN", "")
	t.Setenv("BRAIN_SECRET_SERVER_TOKEN", "")
	t.Setenv("BRAIN_LOGIN", "")
	t.Setenv("BRAIN_PASSWORD", "")
	cfgPath := writeServerConfig(t, newAuthServer(t, "alice", "s3cret").URL)
	useSecretStore(t, newCLISecretStore())

	code, _, stderr := runWithInputForTest(t, "wrong\n", "login", "--config", cfgPath, "--login", "alice", "--password-stdin")
	if code != 1 {
		t.Fatalf("login exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "invalid login or password") {
		t.Fatalf("login stderr = %q", stderr)
	}
	if strings.Contains(stderr, "authentication required") {
		t.Fatalf("login reported a generic auth error: %q", stderr)
	}
}

func TestConfigPathFromArgs(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"note", "--config", "a.yaml"}, "a.yaml"},
		{[]string{"note", "--config=b.yaml"}, "b.yaml"},
		{[]string{"note", "-config", "c.yaml"}, "c.yaml"},
		{[]string{"note", "-config=d.yaml"}, "d.yaml"},
		{[]string{"note"}, config.FileName},
	}
	for _, test := range tests {
		if got := configPathFromArgs(test.args); got != test.want {
			t.Fatalf("configPathFromArgs(%v) = %q, want %q", test.args, got, test.want)
		}
	}
}

type cliRegistryProject struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Paths         []string  `json:"paths"`
	Aliases       []string  `json:"aliases"`
	Kind          string    `json:"kind"`
	RepoURL       string    `json:"repo_url"`
	DefaultBranch string    `json:"default_branch"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type cliRegistry struct {
	Version  int                  `json:"version"`
	Projects []cliRegistryProject `json:"projects"`
}

func readCLIRegistry(t *testing.T, dataDir string) cliRegistry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, "registry.json"))
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	var registry cliRegistry
	if err := json.Unmarshal(raw, &registry); err != nil {
		t.Fatalf("parse registry: %v", err)
	}
	return registry
}

func TestProjectCommandWorkflow(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}

	projectPath := filepath.Join(root, "src", "demo")
	code, stdout, stderr := runForTest(t, "project", "set", "demo",
		"--name", "Demo",
		"--path", projectPath,
		"--kind", "repository",
		"--repo", "https://example.test/demo.git",
		"--branch", "main",
		"--config", cfgPath)
	if code != 0 {
		t.Fatalf("project set exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"id: demo", "name: Demo", "kind: repository", "paths: " + projectPath} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("project set stdout missing %q:\n%s", want, stdout)
		}
	}

	registry := readCLIRegistry(t, dataDir)
	if len(registry.Projects) != 1 {
		t.Fatalf("registry projects = %+v", registry.Projects)
	}
	created := registry.Projects[0]
	if created.ID != "demo" || created.Name != "Demo" || created.Kind != "repository" {
		t.Fatalf("registry project = %+v", created)
	}
	if len(created.Paths) != 1 || created.Paths[0] != projectPath {
		t.Fatalf("registry paths = %+v", created.Paths)
	}
	if created.RepoURL != "https://example.test/demo.git" || created.DefaultBranch != "main" {
		t.Fatalf("registry repo metadata = %+v", created)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("registry timestamps = %+v", created)
	}

	code, stdout, stderr = runForTest(t, "project", "list", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project list exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"demo", "repository", "Demo", projectPath, "https://example.test/demo.git", "main"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("project list stdout missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, stderr = runForTest(t, "project", "show", "demo", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project show exit code = %d (stderr: %s)", code, stderr)
	}
	for _, want := range []string{"id: demo", "name: Demo", "paths: " + projectPath, "repo: https://example.test/demo.git", "branch: main"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("project show stdout missing %q:\n%s", want, stdout)
		}
	}

	code, _, stderr = runForTest(t, "project", "set", "demo", "--branch", "dev", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project set branch exit code = %d (stderr: %s)", code, stderr)
	}
	updated := readCLIRegistry(t, dataDir).Projects[0]
	if updated.Name != "Demo" || updated.RepoURL != "https://example.test/demo.git" || updated.DefaultBranch != "dev" {
		t.Fatalf("partial update lost fields: %+v", updated)
	}
	if len(updated.Paths) != 1 || updated.Paths[0] != projectPath {
		t.Fatalf("partial update lost paths: %+v", updated.Paths)
	}
	if !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("CreatedAt = %s, want %s", updated.CreatedAt, created.CreatedAt)
	}

	code, stdout, stderr = runForTest(t, "project", "list", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project list --json exit code = %d (stderr: %s)", code, stderr)
	}
	var projects []domain.Project
	if err := json.Unmarshal([]byte(stdout), &projects); err != nil {
		t.Fatalf("project list --json is not JSON: %v\n%s", err, stdout)
	}
	if len(projects) != 1 || projects[0].ID != "demo" || projects[0].Kind != domain.ProjectKindRepository {
		t.Fatalf("project list --json = %+v", projects)
	}

	code, stdout, stderr = runForTest(t, "project", "show", "demo", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project show --json exit code = %d (stderr: %s)", code, stderr)
	}
	var shown domain.Project
	if err := json.Unmarshal([]byte(stdout), &shown); err != nil {
		t.Fatalf("project show --json is not JSON: %v\n%s", err, stdout)
	}
	if shown.ID != "demo" || shown.DefaultBranch != "dev" || shown.Name != "Demo" {
		t.Fatalf("project show --json = %+v", shown)
	}

	code, _, stderr = runForTest(t, "project", "show", "missing", "--config", cfgPath)
	if code != 1 || !strings.Contains(stderr, "not found") {
		t.Fatalf("project show missing exit code = %d, stderr = %q", code, stderr)
	}

	code, _, stderr = runForTest(t, "project", "nope")
	if code != 2 || !strings.Contains(stderr, "unknown subcommand") {
		t.Fatalf("project nope exit code = %d, stderr = %q", code, stderr)
	}

	code, _, stderr = runForTest(t, "project", "set", "demo", "--clear-paths", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project set --clear-paths exit code = %d (stderr: %s)", code, stderr)
	}
	cleared := readCLIRegistry(t, dataDir).Projects[0]
	if len(cleared.Paths) != 0 {
		t.Fatalf("paths were not cleared: %+v", cleared.Paths)
	}
	if cleared.Name != "Demo" || cleared.DefaultBranch != "dev" {
		t.Fatalf("--clear-paths lost other fields: %+v", cleared)
	}

	code, _, stderr = runForTest(t, "project", "set", "demo",
		"--path", projectPath, "--clear-paths", "--config", cfgPath)
	if code != 2 || !strings.Contains(stderr, "cannot be combined") {
		t.Fatalf("conflicting flags exit code = %d, stderr = %q", code, stderr)
	}

	code, stdout, stderr = runForTest(t, "help")
	if code != 0 || !strings.Contains(stdout, "project") {
		t.Fatalf("help exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	code, stdout, stderr = runForTest(t, "project", "--help")
	if code != 0 || !strings.Contains(stdout, "brain project set") {
		t.Fatalf("project help exit code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestProjectSetDefaultsNameToID(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}

	code, _, stderr := runForTest(t, "project", "set", "plain", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project set exit code = %d (stderr: %s)", code, stderr)
	}
	project := readCLIRegistry(t, dataDir).Projects[0]
	if project.Name != "plain" || project.Kind != "other" {
		t.Fatalf("project = %+v", project)
	}
}

func TestProjectSetRejectsExplicitEmptyFlags(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{
			name:       "empty path",
			args:       []string{"project", "set", "demo", "--path", "", "--config", cfgPath},
			wantCode:   2,
			wantStderr: "must not be empty",
		},
		{
			name:       "empty path with equals",
			args:       []string{"project", "set", "demo", "--path=", "--config", cfgPath},
			wantCode:   2,
			wantStderr: "must not be empty",
		},
		{
			name:       "blank alias",
			args:       []string{"project", "set", "demo", "--alias", "  ", "--config", cfgPath},
			wantCode:   2,
			wantStderr: "must not be empty",
		},
		{
			name:       "empty kind",
			args:       []string{"project", "set", "demo", "--kind", "", "--config", cfgPath},
			wantCode:   2,
			wantStderr: "--kind must not be empty",
		},
		{
			name:       "empty name on create",
			args:       []string{"project", "set", "demo", "--name", "", "--config", cfgPath},
			wantCode:   1,
			wantStderr: "project.name",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, _, stderr := runForTest(t, test.args...)
			if code != test.wantCode || !strings.Contains(stderr, test.wantStderr) {
				t.Fatalf("exit code = %d, stderr = %q; want %d containing %q", code, stderr, test.wantCode, test.wantStderr)
			}
		})
	}
}

func TestProjectSetDeduplicatesRepeatableFlags(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}

	projectPath := filepath.Join(root, "src", "demo")
	code, stdout, stderr := runForTest(t, "project", "set", "demo",
		"--path", projectPath,
		"--path", "  "+projectPath+"  ",
		"--alias", "d",
		"--alias", "d",
		"--config", cfgPath)
	if code != 0 {
		t.Fatalf("project set exit code = %d (stderr: %s)", code, stderr)
	}
	project := readCLIRegistry(t, dataDir).Projects[0]
	if len(project.Paths) != 1 || project.Paths[0] != projectPath {
		t.Fatalf("paths = %+v, want one deduplicated path", project.Paths)
	}
	if len(project.Aliases) != 1 || project.Aliases[0] != "d" {
		t.Fatalf("aliases = %+v, want one deduplicated alias", project.Aliases)
	}
	if strings.Count(stdout, projectPath) != 1 {
		t.Fatalf("project set output repeated the path:\n%s", stdout)
	}
}

func TestProjectClearServerModeSendsEmptyArrays(t *testing.T) {
	t.Setenv("BRAIN_CONFIG", "")
	t.Setenv("BRAIN_TOKEN", "test-token")
	project := domain.Project{
		ID:      "demo",
		Name:    "Demo",
		Paths:   []string{"/src/demo"},
		Aliases: []string{"d"},
		Kind:    domain.ProjectKindRepository,
	}
	var patchedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/demo":
			_ = json.NewEncoder(w).Encode(project)
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/projects/demo":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read patch body: %v", err)
				http.Error(w, "read failed", http.StatusInternalServerError)
				return
			}
			patchedBody = append([]byte(nil), body...)
			project.Paths = []string{}
			project.Aliases = []string{}
			_ = json.NewEncoder(w).Encode(project)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cfgPath := writeServerConfig(t, server.URL)

	code, stdout, stderr := runForTest(t, "project", "set", "demo",
		"--clear-paths", "--clear-aliases", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project set exit code = %d (stderr: %s)", code, stderr)
	}
	body := string(patchedBody)
	if !strings.Contains(body, `"paths":[]`) {
		t.Fatalf("patch body does not clear paths with []: %s", body)
	}
	if !strings.Contains(body, `"aliases":[]`) {
		t.Fatalf("patch body does not clear aliases with []: %s", body)
	}
	if strings.Contains(body, `"paths":null`) || strings.Contains(body, `"aliases":null`) {
		t.Fatalf("patch body sent null instead of []: %s", body)
	}
	if !strings.Contains(stdout, "paths: -") || !strings.Contains(stdout, "aliases: -") {
		t.Fatalf("project set output does not show cleared lists:\n%s", stdout)
	}
}
