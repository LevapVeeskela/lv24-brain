package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	compatGitHubToken = "ghp_compat_github_token"
	compatGitLabToken = "compat_gitlab_token"
)

// connectorFixtureServer serves the legacy GitHub and GitLab endpoints used by
// `connector run --all` when the connection catalogue is empty.
func connectorFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/widgets":
			writeConnectorFixtureJSON(w, `{
				"full_name":"acme/widgets","description":"Widgets",
				"html_url":"https://github.com/acme/widgets","default_branch":"main",
				"private":false,"archived":false,"fork":false,
				"stargazers_count":1,"forks_count":1,"open_issues_count":0,
				"updated_at":"2026-11-05T10:00:00Z"
			}`)
		case strings.HasSuffix(r.URL.Path, "/pulls"), strings.HasSuffix(r.URL.Path, "/issues"), strings.HasSuffix(r.URL.Path, "/commits"):
			writeConnectorFixtureJSON(w, `[]`)
		case strings.HasPrefix(r.URL.Path, "/projects/") && strings.HasSuffix(r.URL.Path, "/merge_requests"):
			writeConnectorFixtureJSON(w, `[]`)
		case strings.HasPrefix(r.URL.Path, "/projects/") && strings.HasSuffix(r.URL.Path, "/issues"):
			writeConnectorFixtureJSON(w, `[]`)
		case strings.Contains(r.URL.Path, "/repository/commits"):
			writeConnectorFixtureJSON(w, `[]`)
		case strings.HasPrefix(r.URL.Path, "/projects/"):
			writeConnectorFixtureJSON(w, `{"id":1,"path_with_namespace":"acme/widgets","web_url":"https://gitlab.example/acme/widgets"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func writeConnectorFixtureJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestConnectorRunAllFallsBackToLegacyTargetsWhenCatalogueEmpty(t *testing.T) {
	server := connectorFixtureServer(t)
	t.Setenv("BRAIN_SECRET_GITHUB_TOKEN", compatGitHubToken)
	t.Setenv("BRAIN_SECRET_GITLAB_TOKEN", compatGitLabToken)
	t.Setenv("BRAIN_GITHUB_BASE_URL", server.URL)
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	t.Setenv("BRAIN_GITLAB_BASE_URL", server.URL)
	t.Setenv("BRAIN_GITLAB_PROJECTS", "acme/widgets")

	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	code, stdout, stderr := runForTest(t, "connector", "run", "--all", "--since", "2026-06-01T00:00:00Z", "--limit", "10", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("connector --all exit code = %d (stderr: %s)", code, stderr)
	}
	for _, label := range []string{"== github", "== gitlab"} {
		if got := strings.Count(stdout, label+"\n"); got != 1 {
			t.Fatalf("%s ran %d times:\n%s", label, got, stdout)
		}
	}
	for _, dump := range []string{
		filepath.Join(dataDir, "github", "acme", "widgets.md"),
		filepath.Join(dataDir, "gitlab", "acme", "widgets.md"),
	} {
		if _, err := os.Stat(dump); err != nil {
			t.Fatalf("legacy dump %s: %v", dump, err)
		}
	}
	if strings.Contains(stdout, "== work") {
		t.Fatalf("empty catalogue still ran a configured connection: %s", stdout)
	}
}

func TestConnectorRunAllProjectFilterSelectsCatalogueConnections(t *testing.T) {
	path, dataDir := connectionConfig(t, config.ModeLocal)
	store, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, connection := range []domain.Connection{
		{ID: "alpha", Provider: "fixture", Account: "a", Projects: []string{"project-a"}},
		{ID: "beta", Provider: "fixture", Account: "b", Projects: []string{"project-b"}},
	} {
		if err := store.Create(context.Background(), connection); err != nil {
			t.Fatal(err)
		}
	}
	previous := buildIngestion
	buildIngestion = func(cfg config.BrainConfig, opts bootstrap.Options) (*bootstrap.IngestionRuntime, error) {
		opts.Secrets = newCLISecretStore()
		runtime, err := bootstrap.BuildIngestion(cfg, opts)
		if err != nil {
			return nil, err
		}
		if err := runtime.Ingestion.Registry.Register(cliItemSource{}.Descriptor(), func(domain.Connection, connectorbuild.Deps) (ports.Source, error) {
			return cliItemSource{}, nil
		}); err != nil {
			runtime.Close()
			return nil, err
		}
		return runtime, nil
	}
	t.Cleanup(func() { buildIngestion = previous })

	code, stdout, stderr := runForTest(t, "connector", "run", "--all", "--project", "project-a", "--config", path)
	if code != 0 {
		t.Fatalf("connector --all --project exit code = %d (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "== alpha") || strings.Contains(stdout, "== beta") {
		t.Fatalf("project filter ran the wrong connections:\n%s", stdout)
	}
	raw, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := raw.ListConnectorRuns(context.Background(), "fixture", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %+v, %v; want one run", runs, err)
	}
}

func TestConnectorRunNameKeepsLegacySnapshotWithNonEmptyCatalogue(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "brain")
	cfgPath := filepath.Join(dataDir, "brain.yaml")
	if code, _, stderr := runForTest(t, "init", "--mode", "local", "--data-dir", dataDir); code != 0 {
		t.Fatalf("init exit code = %d (stderr: %s)", code, stderr)
	}
	store, err := fsstore.NewConnectionStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), domain.Connection{ID: "work", Provider: "fixture"}); err != nil {
		t.Fatal(err)
	}
	server := connectorFixtureServer(t)
	t.Setenv("BRAIN_SECRET_GITHUB_TOKEN", compatGitHubToken)
	t.Setenv("BRAIN_GITHUB_BASE_URL", server.URL)
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")

	code, stdout, stderr := runForTest(t, "connector", "run", "--name", "github", "--project", "acme/widgets", "--since", "2026-06-01T00:00:00Z", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("connector --name exit code = %d (stderr: %s)", code, stderr)
	}
	if strings.Contains(stdout, "== work") {
		t.Fatalf("--name ran the catalogue: %s", stdout)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "github", "acme", "widgets.md")); err != nil {
		t.Fatalf("legacy dump missing: %v", err)
	}
}
