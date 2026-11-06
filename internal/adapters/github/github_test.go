package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const testToken = "ghp_test_token_value"

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

func TestGitHubRunWritesDeterministicMarkdown(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != DefaultUserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		switch r.URL.Path {
		case "/repos/acme/widgets":
			writeJSON(t, w, `{
				"full_name":"acme/widgets","description":"Widgets for everyone",
				"html_url":"https://github.com/acme/widgets","default_branch":"main",
				"private":false,"archived":false,"fork":false,
				"stargazers_count":42,"forks_count":7,"open_issues_count":3,
				"updated_at":"2026-11-05T10:00:00Z"
			}`)
		case "/repos/acme/widgets/pulls":
			if got := r.URL.Query().Get("state"); got != "all" {
				t.Errorf("pulls state = %q", got)
			}
			writeJSON(t, w, `[
				{"number":2,"title":"Second PR","state":"open","html_url":"https://github.com/acme/widgets/pull/2",
				 "user":{"login":"alice"},"draft":false,"created_at":"2026-11-01T10:00:00Z",
				 "updated_at":"2026-11-04T10:00:00Z","labels":[{"name":"ready"}],
				 "head":{"ref":"feature"},"base":{"ref":"main"}},
				{"number":1,"title":"Old PR","state":"closed","html_url":"https://github.com/acme/widgets/pull/1",
				 "user":{"login":"bob"},"draft":false,"created_at":"2025-01-01T10:00:00Z",
				 "updated_at":"2025-01-02T10:00:00Z"}
			]`)
		case "/repos/acme/widgets/issues":
			if got := r.URL.Query().Get("since"); got != "2026-06-01T00:00:00Z" {
				t.Errorf("issues since = %q", got)
			}
			writeJSON(t, w, `[
				{"number":5,"title":"Bug report","state":"open","html_url":"https://github.com/acme/widgets/issues/5",
				 "user":{"login":"carol"},"comments":2,"created_at":"2026-11-02T10:00:00Z",
				 "updated_at":"2026-11-05T09:00:00Z","labels":[{"name":"bug"}]},
				{"number":6,"title":"PR shown as issue","state":"open","html_url":"https://github.com/acme/widgets/pull/6",
				 "user":{"login":"dave"},"created_at":"2026-11-02T10:00:00Z",
				 "updated_at":"2026-11-04T09:00:00Z","pull_request":{}},
				{"number":7,"title":"Old issue","state":"closed","html_url":"https://github.com/acme/widgets/issues/7",
				 "user":{"login":"eve"},"created_at":"2025-01-01T10:00:00Z",
				 "updated_at":"2025-01-02T10:00:00Z"}
			]`)
		case "/repos/acme/widgets/commits":
			if got := r.URL.Query().Get("since"); got != "2026-06-01T00:00:00Z" {
				t.Errorf("commits since = %q", got)
			}
			writeJSON(t, w, `[
				{"sha":"abcdef1234567890","html_url":"https://github.com/acme/widgets/commit/abcdef1",
				 "commit":{"author":{"name":"Alice","email":"alice@example.test","date":"2026-11-05T08:00:00Z"},
				 "message":"Fix widgets\n\nLonger body"},"author":{"login":"alice"}}
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
		TokenKey:   "github/token",
		Repos:      []string{"acme/widgets"},
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
	if run.Counts["repos"] != 1 || run.Counts["pull_requests"] != 1 || run.Counts["issues"] != 1 || run.Counts["commits"] != 1 {
		t.Fatalf("counts = %#v", run.Counts)
	}
	for _, method := range methods {
		if method != http.MethodGet {
			t.Fatalf("non-GET request: %s", method)
		}
	}

	content, ok := sink.file("github/acme/widgets.md")
	if !ok {
		t.Fatalf("dump not written: %#v", sink.files)
	}
	for _, want := range []string{
		"# acme/widgets", "## Pull requests (1)", "Second PR", "## Issues (1)", "Bug report",
		"## Commits (1)", "Fix widgets", "https://github.com/acme/widgets/pull/2",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("dump missing %q:\n%s", want, content)
		}
	}
	for _, forbidden := range []string{"Old PR", "Old issue", "PR shown as issue", testToken} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("dump contains %q:\n%s", forbidden, content)
		}
	}

	secondRun, err := connector.Run(context.Background(), ports.ConnectorOptions{Since: &since, Limit: 10})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	secondContent, _ := sink.file("github/acme/widgets.md")
	if secondContent != content {
		t.Fatalf("markdown is not deterministic:\n%s\n---\n%s", content, secondContent)
	}
	if secondRun.Counts["pull_requests"] != 1 {
		t.Fatalf("second counts = %#v", secondRun.Counts)
	}
}

func TestGitHubBaseURLSchemeValidation(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{name: "https public host", baseURL: "https://api.github.com", wantErr: false},
		{name: "https custom host", baseURL: "https://github.example.internal/api/v3", wantErr: false},
		{name: "http ipv4 loopback", baseURL: "http://127.0.0.1:8080", wantErr: false},
		{name: "http localhost", baseURL: "http://localhost:8080", wantErr: false},
		{name: "http ipv6 loopback", baseURL: "http://[::1]:8080", wantErr: false},
		{name: "http non-loopback rejected", baseURL: "http://example.com", wantErr: true},
		{name: "http private address rejected", baseURL: "http://10.0.0.5", wantErr: true},
		{name: "ftp scheme rejected", baseURL: "ftp://api.github.com", wantErr: true},
		{name: "file scheme rejected", baseURL: "file:///tmp/api", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{
				BaseURL: tc.baseURL,
				Repos:   []string{"acme/widgets"},
				Secrets: fakeSecrets{value: testToken},
			})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("New(%q) error = %v", tc.baseURL, err)
				}
				return
			}
			var validationErr *domain.ValidationError
			if !errors.As(err, &validationErr) || validationErr.Field != "github.baseURL" {
				t.Fatalf("New(%q) error = %v, want github.baseURL validation error", tc.baseURL, err)
			}
		})
	}
}

func TestGitHubRunFailsClosedWithoutToken(t *testing.T) {
	sink := &memorySink{}
	connector, err := New(Config{
		BaseURL: "https://api.github.test",
		Repos:   []string{"acme/widgets"},
		Secrets: fakeSecrets{err: domain.ErrNotFound},
		Sink:    sink,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{})
	if err == nil {
		t.Fatal("Run = nil error")
	}
	if run.Status != domain.ConnectorFailed {
		t.Fatalf("status = %q", run.Status)
	}
	if len(sink.files) != 0 {
		t.Fatalf("sink wrote files: %#v", sink.files)
	}
}

func TestGitHubRunRejectsUnconfiguredRepository(t *testing.T) {
	connector, err := New(Config{
		BaseURL: "https://api.github.test",
		Repos:   []string{"acme/widgets"},
		Secrets: fakeSecrets{value: testToken},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	run, err := connector.Run(context.Background(), ports.ConnectorOptions{Projects: []string{"other/repo"}})
	if err == nil || run.Status != domain.ConnectorFailed {
		t.Fatalf("Run = (%#v, %v)", run, err)
	}
}

func TestGitHubRunDoesNotLeakTokenOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid token "+testToken, http.StatusUnauthorized)
	}))
	defer server.Close()

	connector, err := New(Config{
		BaseURL:    server.URL,
		Repos:      []string{"acme/widgets"},
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

func TestGitHubRetriesRateLimit(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/widgets":
			writeJSON(t, w, `{"full_name":"acme/widgets"}`)
		default:
			writeJSON(t, w, `[]`)
		}
	}))
	defer server.Close()

	var slept bool
	connector, err := New(Config{
		BaseURL:    server.URL,
		Repos:      []string{"acme/widgets"},
		Secrets:    fakeSecrets{value: testToken},
		HTTPClient: server.Client(),
		Retry:      connectors.RetryPolicy{MaxAttempts: 2},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	connector.http = connectors.NewHTTP(connectors.HTTPOptions{
		Client: server.Client(),
		Retry:  connectors.RetryPolicy{MaxAttempts: 2},
		Sleep: func(ctx context.Context, wait time.Duration) error {
			slept = true
			return nil
		},
	})
	if _, err := connector.Run(context.Background(), ports.ConnectorOptions{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if attempts < 2 || !slept {
		t.Fatalf("attempts=%d slept=%v", attempts, slept)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

var _ ports.SecretStore = fakeSecrets{}
var _ connectors.MarkdownSink = (*memorySink)(nil)

var _ ports.Connector = (*Connector)(nil)
