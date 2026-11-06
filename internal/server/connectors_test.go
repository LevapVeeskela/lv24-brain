package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

type staticSecrets map[string]string

func (s staticSecrets) Get(_ context.Context, key string) (string, error) {
	value, ok := s[key]
	if !ok {
		return "", domain.ErrNotFound
	}
	return value, nil
}

func (s staticSecrets) Set(context.Context, string, string) error { return nil }
func (s staticSecrets) Delete(context.Context, string) error      { return nil }
func (s staticSecrets) Names(context.Context) ([]string, error) {
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	return names, nil
}

// panickingSecrets makes a connector panic while resolving its token mid-run,
// exercising the runConnector recover path (the record must still persist).
type panickingSecrets struct{ staticSecrets }

func (panickingSecrets) Get(context.Context, string) (string, error) {
	panic("connector secret boom")
}

func TestConnectorRunnerGitHubPersistsRun(t *testing.T) {
	const token = "ghp_server_connector_test"
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets":
			_, _ = w.Write([]byte(`{"full_name":"acme/widgets","description":"Widgets","html_url":"https://github.com/acme/widgets","default_branch":"main","private":false,"archived":false,"fork":false,"stargazers_count":1,"forks_count":1,"open_issues_count":0,"updated_at":"2026-11-05T10:00:00Z"}`))
		case "/repos/acme/widgets/pulls":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/acme/widgets/issues":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/acme/widgets/commits":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(githubServer.Close)

	t.Setenv("BRAIN_GITHUB_BASE_URL", githubServer.URL)
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")

	store := httpapi.NewMemoryStore()
	runner := newConnectorRunner(store, staticSecrets{"github/token": token}, 30*time.Second, nil)
	run, err := runner.Run(context.Background(), httpapi.ConnectorRunRequest{
		Name:     "github",
		Projects: []string{"acme/widgets"},
		Limit:    10,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != domain.ConnectorSuccess || run.Counts["repos"] != 1 {
		t.Fatalf("run = %+v", run)
	}
	records, err := store.ListConnectorRuns(context.Background(), httpapi.ConnectorRunFilter{Source: "github", Limit: 10})
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(records) != 1 || records[0].ID != run.ID || records[0].Status != domain.ConnectorSuccess {
		t.Fatalf("records = %+v", records)
	}
}

func TestConnectorRunnerRejectsUnknownName(t *testing.T) {
	runner := newConnectorRunner(httpapi.NewMemoryStore(), staticSecrets{}, time.Second, nil)
	_, err := runner.Run(context.Background(), httpapi.ConnectorRunRequest{Name: "unknown"})
	if !strings.Contains(err.Error(), "connector.name") {
		t.Fatalf("error = %v", err)
	}
}

func TestConnectorRunnerRequiresStore(t *testing.T) {
	runner := newConnectorRunner(nil, staticSecrets{}, time.Millisecond, nil)
	_, err := runner.Run(context.Background(), httpapi.ConnectorRunRequest{Name: "github"})
	if err == nil {
		t.Fatal("expected an error without a store")
	}
}

// The real PostgreSQL adapter rejects canceled contexts. Keep that behavior
// here so this test cannot pass just because MemoryStore ignores ctx.
type contextCheckingRunStore struct{ httpapi.Store }

func (s contextCheckingRunStore) CreateConnectorRun(ctx context.Context, run domain.ConnectorRun) (httpapi.ConnectorRunRecord, domain.SyncRevision, error) {
	if err := ctx.Err(); err != nil {
		return httpapi.ConnectorRunRecord{}, domain.SyncRevision{}, err
	}
	return s.Store.CreateConnectorRun(ctx, run)
}

type deadlineSecrets struct{ staticSecrets }

func (s deadlineSecrets) Get(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestConnectorRunnerPersistsTimeoutWithLiveContext(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := httpapi.NewMemoryStore()
	runner := newConnectorRunner(contextCheckingRunStore{store}, deadlineSecrets{}, 5*time.Millisecond, nil)
	run, err := runner.Run(t.Context(), httpapi.ConnectorRunRequest{Name: "github"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != domain.ConnectorFailed || !strings.Contains(run.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("timeout run = %+v", run)
	}
	record, err := store.GetConnectorRun(t.Context(), run.ID)
	if err != nil || record.Status != domain.ConnectorFailed || record.FinishedAt == nil {
		t.Fatalf("persisted timeout = %+v, %v", record, err)
	}
}

func TestConnectorRunnerPersistsSanitizedUpstreamFailure(t *testing.T) {
	const token = "test-server-secret-token"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("denied token=" + token))
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("BRAIN_GITHUB_BASE_URL", upstream.URL)
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := httpapi.NewMemoryStore()
	runner := newConnectorRunner(store, staticSecrets{"github/token": token}, time.Second, nil)
	run, err := runner.Run(t.Context(), httpapi.ConnectorRunRequest{Name: "github"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != domain.ConnectorFailed || !strings.Contains(run.Error, "401") || strings.Contains(run.Error, token) {
		t.Fatalf("failure run = %+v", run)
	}
	record, err := store.GetConnectorRun(t.Context(), run.ID)
	if err != nil || record.Error != run.Error || record.Status != run.Status {
		t.Fatalf("persisted failure = %+v, %v", record, err)
	}
}

func TestConnectorRunnerPersistsPanicAsFailedRun(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := httpapi.NewMemoryStore()
	runner := newConnectorRunner(store, panickingSecrets{}, time.Second, nil)
	run, err := runner.Run(t.Context(), httpapi.ConnectorRunRequest{Name: "github"})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("Run error = %v", err)
	}
	if run.ID == "" || run.Status != domain.ConnectorFailed || !strings.Contains(run.Error, "panicked") {
		t.Fatalf("panic run = %+v", run)
	}
	record, err := store.GetConnectorRun(t.Context(), run.ID)
	if err != nil || record.Status != domain.ConnectorFailed || record.FinishedAt == nil {
		t.Fatalf("persisted panic record = %+v, %v", record, err)
	}
}

type failingRunStore struct{ httpapi.Store }

func (s failingRunStore) CreateConnectorRun(context.Context, domain.ConnectorRun) (httpapi.ConnectorRunRecord, domain.SyncRevision, error) {
	return httpapi.ConnectorRunRecord{}, domain.SyncRevision{}, errors.New("persist unavailable")
}

func TestConnectorRunnerReportsPersistenceFailure(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	runner := newConnectorRunner(failingRunStore{httpapi.NewMemoryStore()}, staticSecrets{}, time.Second, nil)
	run, err := runner.Run(t.Context(), httpapi.ConnectorRunRequest{Name: "github"})
	if err == nil || !strings.Contains(err.Error(), "record connector run") {
		t.Fatalf("Run error = %v", err)
	}
	if run.ID == "" || run.Status != domain.ConnectorFailed {
		t.Fatalf("run = %+v", run)
	}
}
