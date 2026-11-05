package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const testToken = "glpat-test-token-value"

type fakeSecrets struct {
	value string
	err   error
}

func (f fakeSecrets) Get(ctx context.Context, key string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.value, nil
}

func (f fakeSecrets) Set(ctx context.Context, key, value string) error { return nil }
func (f fakeSecrets) Delete(ctx context.Context, key string) error     { return nil }
func (f fakeSecrets) Names(ctx context.Context) ([]string, error)      { return nil, nil }

type memorySink struct {
	mu    sync.Mutex
	files map[string][]byte
}

func (s *memorySink) Write(ctx context.Context, path string, content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files[path] = append([]byte(nil), content...)
	return nil
}

func (s *memorySink) file(path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.files[path]
	return string(content), ok
}

func TestGitLabRunWritesDeterministicMarkdown(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if got := r.Header.Get("PRIVATE-TOKEN"); got != testToken {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != DefaultUserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		switch r.URL.EscapedPath() {
		case "/projects/acme%2Fwidgets":
			writeJSON(t, w, `{
				"id":123,"path_with_namespace":"acme/widgets","name":"Widgets",
				"description":"Widgets for everyone","web_url":"https://gitlab.com/acme/widgets",
				"default_branch":"main","visibility":"private","archived":false,
				"star_count":9,"forks_count":2,"open_issues_count":4,
				"last_activity_at":"2026-11-05T10:00:00Z"
			}`)
		case "/projects/123/merge_requests":
			if got := r.URL.Query().Get("updated_after"); got != "2026-06-01T00:00:00Z" {
				t.Errorf("merge_requests updated_after = %q", got)
			}
			writeJSON(t, w, `[
				{"iid":2,"title":"Second MR","state":"opened","web_url":"https://gitlab.com/acme/widgets/-/merge_requests/2",
				 "author":{"username":"alice"},"draft":false,"created_at":"2026-11-01T10:00:00Z",
				 "updated_at":"2026-11-04T10:00:00Z","labels":["ready"],
				 "source_branch":"feature","target_branch":"main"},
				{"iid":1,"title":"Old MR","state":"merged","web_url":"https://gitlab.com/acme/widgets/-/merge_requests/1",
				 "author":{"username":"bob"},"created_at":"2025-01-01T10:00:00Z",
				 "updated_at":"2025-01-02T10:00:00Z"}
			]`)
		case "/projects/123/issues":
			writeJSON(t, w, `[
				{"iid":5,"title":"Bug report","state":"opened","web_url":"https://gitlab.com/acme/widgets/-/issues/5",
				 "author":{"username":"carol"},"user_notes_count":2,"created_at":"2026-11-02T10:00:00Z",
				 "updated_at":"2026-11-05T09:00:00Z","labels":["bug"]},
				{"iid":7,"title":"Old issue","state":"closed","web_url":"https://gitlab.com/acme/widgets/-/issues/7",
				 "author":{"username":"eve"},"created_at":"2025-01-01T10:00:00Z",
				 "updated_at":"2025-01-02T10:00:00Z"}
			]`)
		case "/projects/123/repository/commits":
			if got := r.URL.Query().Get("since"); got != "2026-06-01T00:00:00Z" {
				t.Errorf("commits since = %q", got)
			}
			writeJSON(t, w, `[
				{"id":"abcdef1234567890","short_id":"abcdef1","title":"Fix widgets",
				 "message":"Fix widgets\n\nLonger body","author_name":"Alice",
				 "author_email":"alice@example.test","authored_date":"2026-11-05T08:00:00Z",
				 "web_url":"https://gitlab.com/acme/widgets/-/commit/abcdef1"}
			]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	sink := &memorySink{}
	fixed := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	connector, err := New(Config{
		BaseURL:    server.URL,
		TokenKey:   "gitlab/token",
		Projects:   []string{"acme/widgets"},
		Secrets:    fakeSecrets{value: testToken},
		Sink:       sink,
		Now:        func() time.Time { return fixed },
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{Since: &since, Limit: 10})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != domain.ConnectorSuccess {
		t.Fatalf("status = %q, error = %q", run.Status, run.Error)
	}
	if run.Counts["projects"] != 1 || run.Counts["merge_requests"] != 1 || run.Counts["issues"] != 1 || run.Counts["commits"] != 1 {
		t.Fatalf("counts = %#v", run.Counts)
	}
	for _, method := range methods {
		if method != http.MethodGet {
			t.Fatalf("non-GET request: %s", method)
		}
	}

	content, ok := sink.file("gitlab/acme/widgets.md")
	if !ok {
		t.Fatalf("dump not written: %#v", sink.files)
	}
	for _, want := range []string{
		"# acme/widgets", "## Merge requests (1)", "Second MR", "## Issues (1)", "Bug report",
		"## Commits (1)", "Fix widgets", "https://gitlab.com/acme/widgets/-/merge_requests/2",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("dump missing %q:\n%s", want, content)
		}
	}
	for _, forbidden := range []string{"Old MR", "Old issue", testToken} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("dump contains %q:\n%s", forbidden, content)
		}
	}

	if _, err := connector.Run(context.Background(), ports.ConnectorOptions{Since: &since, Limit: 10}); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	secondContent, _ := sink.file("gitlab/acme/widgets.md")
	if secondContent != content {
		t.Fatalf("markdown is not deterministic:\n%s\n---\n%s", content, secondContent)
	}
}

func TestGitLabRunFailsClosedWithoutToken(t *testing.T) {
	sink := &memorySink{}
	connector, err := New(Config{
		BaseURL:  "https://gitlab.test/api/v4",
		Projects: []string{"acme/widgets"},
		Secrets:  fakeSecrets{err: domain.ErrNotFound},
		Sink:     sink,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{})
	if err == nil || run.Status != domain.ConnectorFailed {
		t.Fatalf("Run = (%#v, %v)", run, err)
	}
	if len(sink.files) != 0 {
		t.Fatalf("sink wrote files: %#v", sink.files)
	}
}

func TestGitLabRunRejectsUnconfiguredProject(t *testing.T) {
	connector, err := New(Config{
		BaseURL:  "https://gitlab.test/api/v4",
		Projects: []string{"acme/widgets"},
		Secrets:  fakeSecrets{value: testToken},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{Projects: []string{"other/repo"}})
	if err == nil || run.Status != domain.ConnectorFailed {
		t.Fatalf("Run = (%#v, %v)", run, err)
	}
}

func TestGitLabRunDoesNotLeakTokenOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid token "+testToken, http.StatusUnauthorized)
	}))
	defer server.Close()

	connector, err := New(Config{
		BaseURL:    server.URL,
		Projects:   []string{"acme/widgets"},
		Secrets:    fakeSecrets{value: testToken},
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{})
	if err == nil {
		t.Fatal("Run = nil error")
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(run.Error, testToken) {
		t.Fatalf("token leaked: run=%q err=%q", run.Error, err)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

var _ ports.SecretStore = fakeSecrets{}

var _ ports.Connector = (*Connector)(nil)
