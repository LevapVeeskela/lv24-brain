package bootstrap_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const connectorTestToken = "ghp_connector_test_token"

func TestBuildConnectorGitHubWritesMarkdown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+connectorTestToken {
			t.Errorf("Authorization = %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets":
			writeTestJSON(w, `{
				"full_name":"acme/widgets","description":"Widgets",
				"html_url":"https://github.com/acme/widgets","default_branch":"main",
				"private":false,"archived":false,"fork":false,
				"stargazers_count":42,"forks_count":7,"open_issues_count":3,
				"updated_at":"2026-11-05T10:00:00Z"
			}`)
		case "/repos/acme/widgets/pulls":
			writeTestJSON(w, `[
				{"number":2,"title":"Second PR","state":"open","html_url":"https://github.com/acme/widgets/pull/2",
				 "user":{"login":"alice"},"draft":false,"created_at":"2026-11-01T10:00:00Z",
				 "updated_at":"2026-11-04T10:00:00Z","labels":[{"name":"ready"}],
				 "head":{"ref":"feature"},"base":{"ref":"main"}}
			]`)
		case "/repos/acme/widgets/issues":
			writeTestJSON(w, `[
				{"number":5,"title":"Bug report","state":"open","html_url":"https://github.com/acme/widgets/issues/5",
				 "user":{"login":"carol"},"comments":2,"created_at":"2026-11-02T10:00:00Z",
				 "updated_at":"2026-11-05T09:00:00Z","labels":[{"name":"bug"}]}
			]`)
		case "/repos/acme/widgets/commits":
			writeTestJSON(w, `[
				{"sha":"abcdef1234567890","html_url":"https://github.com/acme/widgets/commit/abcdef1",
				 "commit":{"author":{"name":"Alice","email":"alice@example.test","date":"2026-11-05T08:00:00Z"},
				 "message":"Fix widgets"},"author":{"login":"alice"}}
			]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	t.Setenv("BRAIN_GITHUB_BASE_URL", server.URL)
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	dataDir := filepath.Join(t.TempDir(), "brain")
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	fake := &fakeSecretStore{values: map[string]string{"github/token": connectorTestToken}}
	connector, err := bootstrap.BuildConnector(cfg, bootstrap.ConnectorOptions{Name: "github", Limit: 10}, bootstrap.Options{Secrets: fake})
	if err != nil {
		t.Fatalf("BuildConnector: %v", err)
	}
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{Since: &since, Limit: 10})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != domain.ConnectorSuccess || run.Counts["repos"] != 1 {
		t.Fatalf("run = %+v", run)
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "github", "acme", "widgets.md"))
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	content := string(raw)
	for _, want := range []string{"# acme/widgets", "Second PR", "Bug report", "Fix widgets"} {
		if !strings.Contains(content, want) {
			t.Fatalf("dump missing %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, connectorTestToken) {
		t.Fatalf("dump contains the token")
	}
}

func TestBuildConnectorIMAPFailsClosedWithoutEnvironment(t *testing.T) {
	for _, key := range []string{"BRAIN_IMAP_ACCOUNT", "BRAIN_IMAP_ADDRESS", "BRAIN_IMAP_USERNAME", "BRAIN_IMAP_PASSWORD_KEY", "BRAIN_IMAP_FOLDERS", "BRAIN_IMAP_STATE"} {
		t.Setenv(key, "")
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = filepath.Join(t.TempDir(), "brain")
	_, err := bootstrap.BuildConnector(cfg, bootstrap.ConnectorOptions{Name: "imap"}, bootstrap.Options{Secrets: &fakeSecretStore{}})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "BRAIN_IMAP_ACCOUNT") {
		t.Fatalf("BuildConnector error = %v", err)
	}
}

func TestBuildConnectorIMAPConfig(t *testing.T) {
	t.Setenv("BRAIN_IMAP_ACCOUNT", "work")
	t.Setenv("BRAIN_IMAP_ADDRESS", "imap.example.test:993")
	t.Setenv("BRAIN_IMAP_USERNAME", "me@example.test")
	t.Setenv("BRAIN_IMAP_FOLDERS", "INBOX,Archive")
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = filepath.Join(t.TempDir(), "brain")
	connector, err := bootstrap.BuildConnector(cfg, bootstrap.ConnectorOptions{Name: "imap", Limit: 5}, bootstrap.Options{Secrets: &fakeSecretStore{}})
	if err != nil {
		t.Fatalf("BuildConnector: %v", err)
	}
	if connector.Name() != "imap" {
		t.Fatalf("connector name = %q", connector.Name())
	}
}

func TestBuildConnectorGitLabConfig(t *testing.T) {
	t.Setenv("BRAIN_GITLAB_PROJECTS", "acme/widgets")
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = filepath.Join(t.TempDir(), "brain")
	connector, err := bootstrap.BuildConnector(cfg, bootstrap.ConnectorOptions{Name: "gitlab"}, bootstrap.Options{Secrets: &fakeSecretStore{}})
	if err != nil {
		t.Fatalf("BuildConnector: %v", err)
	}
	if connector.Name() != "gitlab" {
		t.Fatalf("connector name = %q", connector.Name())
	}
}

func TestBuildConnectorRejectsUnknownName(t *testing.T) {
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = filepath.Join(t.TempDir(), "brain")
	_, err := bootstrap.BuildConnector(cfg, bootstrap.ConnectorOptions{Name: "nope"}, bootstrap.Options{Secrets: &fakeSecretStore{}})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("BuildConnector error = %v, want ErrValidation", err)
	}
}

func TestBuildConnectorGitHubTargetPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		project string
		repos   []string
		want    string
	}{
		{name: "project filter beats derived repos", project: "proj/repo", repos: []string{"derived/repo"}, want: "proj/repo"},
		{name: "derived repos are the last fallback", repos: []string{"derived/repo"}, want: "derived/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls"), strings.HasSuffix(r.URL.Path, "/issues"), strings.HasSuffix(r.URL.Path, "/commits"):
					writeTestJSON(w, `[]`)
				case strings.HasPrefix(r.URL.Path, "/repos/"):
					writeTestJSON(w, `{"full_name":"acme/widgets","default_branch":"main","open_issues_count":0,"updated_at":"2026-11-05T10:00:00Z"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv("BRAIN_GITHUB_BASE_URL", server.URL)
			t.Setenv("BRAIN_GITHUB_REPOS", "")
			dataDir := filepath.Join(t.TempDir(), "brain")
			cfg := config.Default()
			cfg.Mode = config.ModeLocal
			cfg.DataDir = dataDir
			connector, err := bootstrap.BuildConnector(cfg, bootstrap.ConnectorOptions{Name: "github", Project: tc.project, Repos: tc.repos}, bootstrap.Options{Secrets: &fakeSecretStore{values: map[string]string{"github/token": connectorTestToken}}})
			if err != nil {
				t.Fatalf("BuildConnector: %v", err)
			}
			run, err := connector.Run(context.Background(), ports.ConnectorOptions{Limit: 5})
			if err != nil || run.Status != domain.ConnectorSuccess {
				t.Fatalf("run = %+v, %v", run, err)
			}
			if !slices.Contains(recordedPaths(&mu, &paths), "/repos/"+tc.want) {
				t.Fatalf("requests = %v, want /repos/%s", recordedPaths(&mu, &paths), tc.want)
			}
			owner, repo, _ := strings.Cut(tc.want, "/")
			if _, err := os.Stat(filepath.Join(dataDir, "github", owner, repo+".md")); err != nil {
				t.Fatalf("dump for %s: %v", tc.want, err)
			}
		})
	}
}

func TestBuildConnectorIMAPUsesMailAccountFallback(t *testing.T) {
	for _, key := range []string{"BRAIN_IMAP_ACCOUNT", "BRAIN_IMAP_ADDRESS", "BRAIN_IMAP_USERNAME", "BRAIN_IMAP_PASSWORD_KEY", "BRAIN_IMAP_FOLDERS", "BRAIN_IMAP_STATE"} {
		t.Setenv(key, "")
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = filepath.Join(t.TempDir(), "brain")
	connector, err := bootstrap.BuildConnector(cfg, bootstrap.ConnectorOptions{
		Name: "imap",
		IMAP: &connectorbuild.IMAPAccount{Account: "work", Address: "imap.example.test:993", Username: "me@example.test", Folders: []string{"INBOX"}},
	}, bootstrap.Options{Secrets: &fakeSecretStore{}})
	if err != nil {
		t.Fatalf("BuildConnector: %v", err)
	}
	if connector.Name() != "imap" {
		t.Fatalf("connector name = %q", connector.Name())
	}
}

func recordedPaths(mu *sync.Mutex, paths *[]string) []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), (*paths)...)
}

func writeTestJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}
