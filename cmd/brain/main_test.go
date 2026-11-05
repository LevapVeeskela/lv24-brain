package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

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
	for _, command := range []string{"init", "doctor", "note", "sync", "index", "search", "connector", "migrate", "secret", "serve", "update"} {
		if !strings.Contains(stdout, command) {
			t.Fatalf("help output missing command %q", command)
		}
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

	code, stdout, stderr = runForTest(t, "note", "--config", cfgPath, "--project", "demo", "--title", "First note", "--body", "hello world", "--tags", "alpha, beta")
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
	if code, _, stderr := runForTest(t, "note", "--config", cfgPath, "--project", "demo", "--title", "Search note", "--body", "searchable needle"); code != 0 {
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
	cfg, err := config.Load(filepath.Join(target, "brain.yaml"))
	if err != nil {
		t.Fatalf("load migrated config: %v", err)
	}
	if cfg.SchemaVersion != 1 || cfg.Mode != config.ModeLocal || cfg.DataDir != target {
		t.Fatalf("migrated config = %+v", cfg)
	}
	code, stdout, stderr = runForTest(t, "open", "--project", "demo", "--config", filepath.Join(target, "brain.yaml"))
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
