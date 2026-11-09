package connectorbuild

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type fakeSecrets map[string]string

func (f fakeSecrets) Get(_ context.Context, key string) (string, error) {
	value, ok := f[key]
	if !ok {
		return "", domain.ErrNotFound
	}
	return value, nil
}

func (f fakeSecrets) Set(context.Context, string, string) error { return nil }
func (f fakeSecrets) Delete(context.Context, string) error      { return nil }
func (f fakeSecrets) Names(context.Context) ([]string, error)   { return nil, nil }

func TestBuildRequiresSecretsAndSink(t *testing.T) {
	_, err := Build(Options{Name: "github", Sink: connectors.DiscardSink{}})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "secrets") {
		t.Fatalf("missing secrets error = %v", err)
	}
	_, err = Build(Options{Name: "github", Secrets: fakeSecrets{}})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "sink") {
		t.Fatalf("missing sink error = %v", err)
	}
}

func TestBuildGitHubUsesEnvironmentRepos(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	connector, err := Build(Options{
		Name: "github", Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if connector.Name() != "github" {
		t.Fatalf("name = %q", connector.Name())
	}
}

func TestBuildGitHubFallsBackToProjectFilter(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "")
	connector, err := Build(Options{
		Name: "github", Projects: []string{"acme/widgets"},
		Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if connector.Name() != "github" {
		t.Fatalf("name = %q", connector.Name())
	}
}

func TestBuildIMAPFailsClosedWithoutEnvironment(t *testing.T) {
	for _, key := range []string{"BRAIN_IMAP_ACCOUNT", "BRAIN_IMAP_ADDRESS", "BRAIN_IMAP_USERNAME"} {
		t.Setenv(key, "")
	}
	_, err := Build(Options{Name: "imap", Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{}})
	if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "BRAIN_IMAP_ACCOUNT") {
		t.Fatalf("Build error = %v", err)
	}
}

func TestBuildRejectsUnknownName(t *testing.T) {
	_, err := Build(Options{Name: "unknown", Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{}})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Build error = %v, want ErrValidation", err)
	}
	if !strings.Contains(err.Error(), "github, gitlab, or imap") {
		t.Fatalf("Build error = %v, want the registry providers", err)
	}
}

func TestBuildAcceptsEverySnapshotProvider(t *testing.T) {
	for _, provider := range SnapshotProviders() {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
			t.Setenv("BRAIN_GITLAB_PROJECTS", "acme/api")
			t.Setenv("BRAIN_IMAP_ACCOUNT", "work")
			t.Setenv("BRAIN_IMAP_ADDRESS", "imap.example.test:993")
			t.Setenv("BRAIN_IMAP_USERNAME", "me@example.test")
			connector, err := Build(Options{Name: provider, Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{}})
			if err != nil {
				t.Fatalf("Build(%q): %v", provider, err)
			}
			if connector.Name() != provider {
				t.Fatalf("Name() = %q, want %q", connector.Name(), provider)
			}
		})
	}
}

func TestBuildRejectsNativeOnlyProviderForSnapshotCommand(t *testing.T) {
	for _, provider := range []string{"folders", "jira", "confluence", "linear"} {
		t.Run(provider, func(t *testing.T) {
			_, err := Build(Options{Name: provider, Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{}})
			if !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "brain connection run") {
				t.Fatalf("Build(%q) error = %v, want native-only guidance", provider, err)
			}
		})
	}
}

// The snapshot builders are selected by registry name; these tests lock the
// legacy target precedence that the native path does not share.
func TestBuildGitHubTargetPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		projects []string
		repos    []string
		want     string
	}{
		{name: "environment wins", env: "env/repo", projects: []string{"project/repo"}, repos: []string{"derived/repo"}, want: "/repos/env/repo"},
		{name: "project filter beats derived repos", projects: []string{"project/repo"}, repos: []string{"derived/repo"}, want: "/repos/project/repo"},
		{name: "derived repos are the last fallback", repos: []string{"derived/repo"}, want: "/repos/derived/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := &recordedRequests{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.add(r.URL.Path)
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls"), strings.HasSuffix(r.URL.Path, "/issues"), strings.HasSuffix(r.URL.Path, "/commits"):
					writeFixtureJSON(w, `[]`)
				case strings.HasPrefix(r.URL.Path, "/repos/"):
					writeFixtureJSON(w, `{"full_name":"acme/widgets","default_branch":"main","open_issues_count":0,"updated_at":"2026-11-05T10:00:00Z"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv("BRAIN_GITHUB_REPOS", tc.env)
			t.Setenv("BRAIN_GITHUB_BASE_URL", server.URL)

			connector, err := Build(Options{Name: "github", Projects: tc.projects, Repos: tc.repos, Secrets: fakeSecrets{"github/token": "token"}, Sink: connectors.DiscardSink{}})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if _, err := connector.Run(context.Background(), ports.ConnectorOptions{Limit: 5}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !requests.has(tc.want) {
				t.Fatalf("requests = %v, want %s", requests.all(), tc.want)
			}
			for _, path := range requests.all() {
				for _, unexpected := range []string{"env/repo", "project/repo", "derived/repo"} {
					if path == "/repos/"+unexpected && path != tc.want {
						t.Fatalf("requested %s, want only %s", path, tc.want)
					}
				}
			}
		})
	}
}

func TestBuildGitLabTargetPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		projects []string
		derived  []string
		want     string
	}{
		{name: "environment wins", env: "env/api", projects: []string{"project/api"}, derived: []string{"derived/api"}, want: "/projects/env%2Fapi"},
		{name: "project filter beats derived projects", projects: []string{"project/api"}, derived: []string{"derived/api"}, want: "/projects/project%2Fapi"},
		{name: "derived projects are the last fallback", derived: []string{"derived/api"}, want: "/projects/derived%2Fapi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests := &recordedRequests{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.add(r.URL.EscapedPath())
				switch {
				case strings.HasPrefix(r.URL.Path, "/projects/1/"):
					writeFixtureJSON(w, `[]`)
				case strings.HasPrefix(r.URL.Path, "/projects/"):
					writeFixtureJSON(w, `{"id":1,"path_with_namespace":"acme/api","web_url":"https://gitlab.example/acme/api"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv("BRAIN_GITLAB_PROJECTS", tc.env)
			t.Setenv("BRAIN_GITLAB_BASE_URL", server.URL)

			connector, err := Build(Options{Name: "gitlab", Projects: tc.projects, GitLabProjects: tc.derived, Secrets: fakeSecrets{"gitlab/token": "token"}, Sink: connectors.DiscardSink{}})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if _, err := connector.Run(context.Background(), ports.ConnectorOptions{Limit: 5}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !requests.has(tc.want) {
				t.Fatalf("requests = %v, want %s", requests.all(), tc.want)
			}
			var roots []string
			for _, path := range requests.all() {
				if strings.HasPrefix(path, "/projects/") && !strings.HasPrefix(path, "/projects/1/") {
					roots = append(roots, path)
				}
			}
			if len(roots) != 1 || roots[0] != tc.want {
				t.Fatalf("project roots = %v, want only %s", roots, tc.want)
			}
		})
	}
}

func TestBuildIMAPUsesMailAccountWhenEnvironmentIsEmpty(t *testing.T) {
	for _, key := range []string{"BRAIN_IMAP_ACCOUNT", "BRAIN_IMAP_ADDRESS", "BRAIN_IMAP_USERNAME", "BRAIN_IMAP_FOLDERS", "BRAIN_IMAP_STATE"} {
		t.Setenv(key, "")
	}
	connector, err := Build(Options{
		Name: "imap", Secrets: fakeSecrets{}, Sink: connectors.DiscardSink{}, StatePath: "state.json",
		IMAP: &IMAPAccount{Account: "work", Address: "imap.example.test:993", Username: "me@example.test", Folders: []string{"INBOX", "Archive"}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if connector.Name() != "imap" {
		t.Fatalf("Name() = %q", connector.Name())
	}
}

type recordedRequests struct {
	mu    sync.Mutex
	paths []string
}

func (r *recordedRequests) add(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
}

func (r *recordedRequests) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func (r *recordedRequests) has(path string) bool {
	for _, got := range r.all() {
		if got == path {
			return true
		}
	}
	return false
}

func writeFixtureJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}
