package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/connectionlock"
	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
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

// rawTestStore is the test durable backend: the in-memory HTTP store for
// projects and run records, plus filesystem raw-zone item/checkpoint stores
// and a cross-process connection lock. It proves the same contract as the
// PostgreSQL serverstore without a database.
type rawTestStore struct {
	*httpapi.MemoryStore
	items       ports.ItemStore
	checkpoints ports.CheckpointStore
	locker      ports.ConnectionLocker
}

var _ rawStore = (*rawTestStore)(nil)

func newRawTestStore(t *testing.T) *rawTestStore {
	t.Helper()
	fs, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	locker, err := connectionlock.New(fs.Root())
	if err != nil {
		t.Fatalf("connectionlock.New: %v", err)
	}
	return &rawTestStore{
		MemoryStore: httpapi.NewMemoryStore(),
		items:       fs.Items(),
		checkpoints: fs.Checkpoints(),
		locker:      locker,
	}
}

func (s *rawTestStore) ItemZone() ports.ItemStore             { return s.items }
func (s *rawTestStore) CheckpointZone() ports.CheckpointStore { return s.checkpoints }

func (s *rawTestStore) TryLock(ctx context.Context, id string) (func() error, error) {
	return s.locker.TryLock(ctx, id)
}

// stubSource is a deterministic in-memory provider for tests that must not
// touch the network. The registry entry reuses the GitHub descriptor so the
// ephemeral connection built by buildServerConnection validates.
type stubSource struct {
	descriptor domain.SourceDescriptor
	read       func(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error)
}

func (s *stubSource) Descriptor() domain.SourceDescriptor { return s.descriptor }
func (s *stubSource) Check(context.Context) error         { return nil }

func (s *stubSource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	return s.read(ctx, req)
}

func stubRegistry(t *testing.T, read func(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error)) *connectorbuild.Registry {
	t.Helper()
	descriptor := connectorbuild.GitHubDescriptor()
	registry := connectorbuild.NewRegistry()
	if err := registry.Register(descriptor, func(domain.Connection, connectorbuild.Deps) (ports.Source, error) {
		return &stubSource{descriptor: descriptor, read: read}, nil
	}); err != nil {
		t.Fatalf("register stub source: %v", err)
	}
	return registry
}

// stubItem is one minimal valid item for the stub source.
func stubItem(connectionID, externalID string, at time.Time) domain.Item {
	return domain.Item{
		Ref:       domain.ItemRef{ConnectionID: connectionID, ExternalID: externalID},
		Kind:      domain.ItemIssue,
		Title:     "stub " + externalID,
		Body:      "stub body",
		CreatedAt: at,
		UpdatedAt: at,
	}
}

func stubCheckpoint() domain.Checkpoint {
	return domain.Checkpoint{Stream: "github", Version: 1, Data: []byte(`{"page":1}`)}
}

func successStub(_ context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	at := time.Now().UTC()
	return ports.ReadBatch{
		Items:       []domain.Item{stubItem(req.Connection.ID, "stub-1", at)},
		Checkpoints: []domain.Checkpoint{stubCheckpoint()},
	}, nil
}

// failingStub returns a source error carrying a recognizable provider token.
// The host must redact it before it reaches the run record or the caller.
func failingStub(token string) func(context.Context, ports.ReadRequest) (ports.ReadBatch, error) {
	return func(_ context.Context, _ ports.ReadRequest) (ports.ReadBatch, error) {
		return ports.ReadBatch{}, errors.New("upstream exploded token=" + token)
	}
}

func mustCreateProject(t *testing.T, store *rawTestStore, id string) {
	t.Helper()
	_, _, err := store.CreateProject(t.Context(), domain.Project{ID: id, Name: id, Kind: domain.ProjectKindOther})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
}

// githubTestServer serves one repository with a single issue: repo metadata,
// one issue, and empty pulls/commits/comments pages.
func githubTestServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets":
			_, _ = w.Write([]byte(`{"full_name":"acme/widgets","description":"Widgets","html_url":"https://github.com/acme/widgets","default_branch":"main","private":false,"archived":false,"fork":false,"stargazers_count":1,"forks_count":1,"open_issues_count":1,"updated_at":"2026-11-05T10:00:00Z"}`))
		case "/repos/acme/widgets/issues":
			_, _ = w.Write([]byte(`[{"number":7,"title":"Widget wobbles","body":"It wobbles.","state":"open","html_url":"https://github.com/acme/widgets/issues/7","user":{"login":"octo"},"comments":0,"created_at":"2026-11-05T09:00:00Z","updated_at":"2026-11-05T10:00:00Z","labels":[]}]`))
		case "/repos/acme/widgets/issues/7/comments":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/acme/widgets/pulls":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/acme/widgets/commits":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestConnectorRunnerGitHubPersistsRun(t *testing.T) {
	const token = "ghp_server_connector_test"
	githubServer := githubTestServer(t, token)

	t.Setenv("BRAIN_GITHUB_BASE_URL", githubServer.URL)
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")

	store := newRawTestStore(t)
	mustCreateProject(t, store, "acme")
	runner := newConnectorRunner(store, staticSecrets{"github/token": token}, 30*time.Second, nil)
	run, err := runner.Run(context.Background(), httpapi.ConnectorRunRequest{
		Name:      "github",
		ProjectID: "acme",
		Projects:  []string{"acme/widgets"},
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != domain.ConnectorSuccess {
		t.Fatalf("run = %+v", run)
	}
	if run.Stats == nil || run.Stats.Seen == 0 {
		t.Fatalf("run stats = %+v", run.Stats)
	}
	if run.ConnectionID == "" {
		t.Fatalf("run has no connection id: %+v", run)
	}

	// Raw items are stored and mapped to the validated project.
	page, err := store.ItemZone().List(context.Background(), run.ConnectionID, ports.ItemListOptions{Limit: 100})
	if err != nil {
		t.Fatalf("list raw items: %v", err)
	}
	if len(page.Items) == 0 {
		t.Fatalf("no raw items stored for %q", run.ConnectionID)
	}
	for _, item := range page.Items {
		if item.ProjectID != "acme" {
			t.Fatalf("item %q project = %q, want acme", item.Ref.ExternalID, item.ProjectID)
		}
	}

	// The resumable cursor is checkpointed.
	if _, err := store.CheckpointZone().Get(context.Background(), run.ConnectionID, "github"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	records, err := store.ListConnectorRuns(context.Background(), httpapi.ConnectorRunFilter{Source: "github", Limit: 10})
	if err != nil {
		t.Fatalf("ListConnectorRuns: %v", err)
	}
	if len(records) != 1 || records[0].ID != run.ID || records[0].Status != domain.ConnectorSuccess {
		t.Fatalf("records = %+v", records)
	}
	if strings.Contains(records[0].Error, token) {
		t.Fatalf("persisted run leaks the token: %q", records[0].Error)
	}
}

func TestConnectorRunnerRejectsUnknownName(t *testing.T) {
	runner := newConnectorRunner(newRawTestStore(t), staticSecrets{}, time.Second, nil)
	_, err := runner.Run(context.Background(), httpapi.ConnectorRunRequest{Name: "unknown"})
	if err == nil || !strings.Contains(err.Error(), "connector.name") {
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

func TestConnectorRunnerRequiresSecrets(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	runner := newConnectorRunner(newRawTestStore(t), nil, time.Second, nil)
	_, err := runner.Run(context.Background(), httpapi.ConnectorRunRequest{Name: "github"})
	if err == nil || !strings.Contains(err.Error(), "secret store") {
		t.Fatalf("error = %v", err)
	}
}

func TestConnectorRunnerMemoryStoreIsUnsupported(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	runner := newConnectorRunner(httpapi.NewMemoryStore(), staticSecrets{}, time.Second, nil)
	_, err := runner.Run(context.Background(), httpapi.ConnectorRunRequest{Name: "github"})
	if !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("error = %v, want %v", err, domain.ErrUnsupported)
	}
}

func TestConnectorRunnerWrappedMemoryStoreIsUnsupported(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	runner := newConnectorRunner(memoryOnlyStore{httpapi.NewMemoryStore()}, staticSecrets{}, time.Second, nil)
	_, err := runner.Run(context.Background(), httpapi.ConnectorRunRequest{Name: "github"})
	if !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("error = %v, want %v", err, domain.ErrUnsupported)
	}
}

// memoryOnlyStore hides the concrete backend behind the HTTP interface; it
// proves a decorator that does not expose the raw zone fails closed.
type memoryOnlyStore struct{ httpapi.Store }

func TestConnectorRunnerWorksWithRawZoneWrapper(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := newRawTestStore(t)
	runner := newConnectorRunner(contextCheckingRunStore{store}, staticSecrets{}, time.Second, nil)
	runner.registry = stubRegistry(t, successStub)
	run, err := runner.Run(t.Context(), httpapi.ConnectorRunRequest{Name: "github"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != domain.ConnectorSuccess || run.ID == "" {
		t.Fatalf("run = %+v", run)
	}
	if _, err := store.GetConnectorRun(t.Context(), run.ID); err != nil {
		t.Fatalf("persisted run: %v", err)
	}
}

// The real PostgreSQL adapter rejects canceled contexts. Keep that behavior
// here so this test cannot pass just because MemoryStore ignores ctx.
// Embedding *rawTestStore promotes the raw-zone methods, so the wrapper still
// satisfies rawStore while overriding run persistence.
type contextCheckingRunStore struct{ *rawTestStore }

func (s contextCheckingRunStore) CreateConnectorRun(ctx context.Context, run domain.ConnectorRun) (httpapi.ConnectorRunRecord, domain.SyncRevision, error) {
	if err := ctx.Err(); err != nil {
		return httpapi.ConnectorRunRecord{}, domain.SyncRevision{}, err
	}
	return s.rawTestStore.CreateConnectorRun(ctx, run)
}

type deadlineSecrets struct{ staticSecrets }

func (s deadlineSecrets) Get(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func TestConnectorRunnerPersistsTimeoutWithLiveContext(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := newRawTestStore(t)
	runner := newConnectorRunner(contextCheckingRunStore{store}, deadlineSecrets{}, 5*time.Millisecond, nil)
	run, err := runner.Run(t.Context(), httpapi.ConnectorRunRequest{Name: "github"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != domain.ConnectorSuccess {
		t.Fatalf("timeout run = %+v", run)
	}
	if run.Stats == nil || run.Stats.Truncated != domain.TruncatedDeadline {
		t.Fatalf("timeout run stats = %+v", run.Stats)
	}
	record, err := store.GetConnectorRun(t.Context(), run.ID)
	if err != nil || record.Status != run.Status || record.FinishedAt == nil {
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
	store := newRawTestStore(t)
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
	if strings.Contains(record.Error, token) {
		t.Fatalf("persisted run leaks the token: %q", record.Error)
	}
}

func TestConnectorRunnerPersistsPanicAsFailedRun(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := newRawTestStore(t)
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

func TestConnectorRunnerPanicRedactsSecrets(t *testing.T) {
	const token = "ghp_0123456789abcdef0123456789abcdef01"
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := newRawTestStore(t)
	runner := newConnectorRunner(store, staticSecrets{}, time.Second, nil)
	runner.registry = stubRegistry(t, func(context.Context, ports.ReadRequest) (ports.ReadBatch, error) {
		panic("boom " + token)
	})
	run, err := runner.Run(t.Context(), httpapi.ConnectorRunRequest{Name: "github"})
	if err == nil || !strings.Contains(err.Error(), "panicked") || strings.Contains(err.Error(), token) {
		t.Fatalf("Run error = %v", err)
	}
	if run.ID == "" || run.Status != domain.ConnectorFailed || strings.Contains(run.Error, token) {
		t.Fatalf("panic run = %+v", run)
	}
	record, err := store.GetConnectorRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("persisted panic record: %v", err)
	}
	if record.Status != domain.ConnectorFailed || strings.Contains(record.Error, token) {
		t.Fatalf("persisted panic record = %+v", record)
	}
}

func TestConnectorRunnerKeepsSecretsOutOfRuns(t *testing.T) {
	const token = "ghp_0123456789abcdef0123456789abcdef01"
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := newRawTestStore(t)
	runner := newConnectorRunner(store, staticSecrets{}, time.Second, nil)
	runner.registry = stubRegistry(t, failingStub(token))
	run, err := runner.Run(t.Context(), httpapi.ConnectorRunRequest{Name: "github"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.ID == "" || run.Status != domain.ConnectorFailed || strings.Contains(run.Error, token) {
		t.Fatalf("failure run = %+v", run)
	}
	record, err := store.GetConnectorRun(t.Context(), run.ID)
	if err != nil {
		t.Fatalf("persisted failure: %v", err)
	}
	if record.Status != domain.ConnectorFailed || strings.Contains(record.Error, token) {
		t.Fatalf("persisted failure = %+v", record)
	}
}

type failingRunStore struct{ *rawTestStore }

func (s failingRunStore) CreateConnectorRun(context.Context, domain.ConnectorRun) (httpapi.ConnectorRunRecord, domain.SyncRevision, error) {
	return httpapi.ConnectorRunRecord{}, domain.SyncRevision{}, errors.New("persist unavailable")
}

func TestConnectorRunnerReportsPersistenceFailure(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := newRawTestStore(t)
	runner := newConnectorRunner(failingRunStore{store}, staticSecrets{}, time.Second, nil)
	runner.registry = stubRegistry(t, successStub)
	run, err := runner.Run(t.Context(), httpapi.ConnectorRunRequest{Name: "github"})
	if err == nil || !strings.Contains(err.Error(), "record connector run") {
		t.Fatalf("Run error = %v", err)
	}
	if run.ID == "" {
		t.Fatalf("run = %+v", run)
	}
}

func TestConnectorRunnerRejectsUnknownProject(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := newRawTestStore(t)
	runner := newConnectorRunner(store, staticSecrets{}, time.Second, nil)
	runner.registry = stubRegistry(t, successStub)
	_, err := runner.Run(t.Context(), httpapi.ConnectorRunRequest{Name: "github", ProjectID: "missing-project"})
	if err == nil || !errors.Is(err, domain.ErrNotFound) || !strings.Contains(err.Error(), "unknown project") {
		t.Fatalf("error = %v", err)
	}
	records, listErr := store.ListConnectorRuns(t.Context(), httpapi.ConnectorRunFilter{})
	if listErr != nil {
		t.Fatalf("ListConnectorRuns: %v", listErr)
	}
	if len(records) != 0 {
		t.Fatalf("rejected run persisted %d records", len(records))
	}
}

func TestConnectorRunnerBusyLockMapsToConflict(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	store := newRawTestStore(t)
	runner := newConnectorRunner(store, staticSecrets{}, time.Second, nil)
	runner.registry = stubRegistry(t, successStub)

	req := httpapi.ConnectorRunRequest{Name: "github"}
	conn, err := buildServerConnection(req)
	if err != nil {
		t.Fatalf("buildServerConnection: %v", err)
	}
	release, err := store.TryLock(t.Context(), conn.ID)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	defer func() { _ = release() }()

	_, err = runner.Run(t.Context(), req)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("error = %v, want %v", err, domain.ErrConflict)
	}
}

func TestBuildServerConnectionStableIDs(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_BASE_URL", "https://github.example.com")
	t.Setenv("BRAIN_GITHUB_REPOS", "")

	first, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "github", Projects: []string{"acme/widgets", "acme/api"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	second, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "github", Projects: []string{"acme/api", "acme/widgets"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("same target set hashes differently: %q vs %q", first.ID, second.ID)
	}

	other, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "github", Projects: []string{"acme/widgets"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if other.ID == first.ID {
		t.Fatalf("different target sets share cursor %q", first.ID)
	}

	// Explicit environment lists win over request targets.
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	envConn, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "github", Projects: []string{"acme/other"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if envConn.Settings["repos"] != "acme/widgets" {
		t.Fatalf("repos = %q, want env list", envConn.Settings["repos"])
	}

	// Project mapping is part of the identity: the same provider targets mapped
	// to different brain projects must never share items or cursors.
	mappedOne, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "github", ProjectID: "project-one"})
	if err != nil {
		t.Fatalf("build mapped one: %v", err)
	}
	mappedTwo, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "github", ProjectID: "project-two"})
	if err != nil {
		t.Fatalf("build mapped two: %v", err)
	}
	if mappedOne.ID == mappedTwo.ID || mappedOne.ID == envConn.ID {
		t.Fatalf("project mappings share a connection id: one=%q two=%q unmapped=%q", mappedOne.ID, mappedTwo.ID, envConn.ID)
	}
	if len(mappedOne.Projects) != 1 || mappedOne.Projects[0] != "project-one" {
		t.Fatalf("mapped projects = %v", mappedOne.Projects)
	}

	// Different hosts never share a cursor.
	t.Setenv("BRAIN_GITHUB_BASE_URL", "https://other.example.com")
	hostConn, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "github", Projects: []string{"acme/widgets"}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if hostConn.ID == other.ID {
		t.Fatalf("different hosts share cursor %q", hostConn.ID)
	}
}

func TestServerCredentialsMapping(t *testing.T) {
	ctx := context.Background()

	t.Run("github token", func(t *testing.T) {
		t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
		conn, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "github"})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		creds := newServerCredentials(staticSecrets{"github/token": "secret"}, "github", conn.ID, "")
		got, err := creds.Get(ctx, conn.ID, "token")
		if err != nil || got != "secret" {
			t.Fatalf("token = %q, %v", got, err)
		}
		if _, err := creds.Get(ctx, conn.ID, "password"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("field error = %v", err)
		}
		if _, err := creds.Get(ctx, "other", "token"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("connection error = %v", err)
		}
	})

	t.Run("gitlab token", func(t *testing.T) {
		t.Setenv("BRAIN_GITLAB_PROJECTS", "group/project")
		conn, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "gitlab"})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		creds := newServerCredentials(staticSecrets{"gitlab/token": "secret"}, "gitlab", conn.ID, "")
		got, err := creds.Get(ctx, conn.ID, "token")
		if err != nil || got != "secret" {
			t.Fatalf("token = %q, %v", got, err)
		}
	})

	t.Run("imap password key precedence", func(t *testing.T) {
		t.Setenv("BRAIN_IMAP_ACCOUNT", "Team Inbox")
		t.Setenv("BRAIN_IMAP_ADDRESS", "imap.example.com:993")
		t.Setenv("BRAIN_IMAP_USERNAME", "bot")
		t.Setenv("BRAIN_IMAP_PASSWORD_KEY", "custom/imap/key")
		conn, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "imap"})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		creds := newServerCredentials(staticSecrets{"custom/imap/key": "secret"}, "imap", conn.ID, conn.Account)
		got, err := creds.Get(ctx, conn.ID, "password")
		if err != nil || got != "secret" {
			t.Fatalf("password = %q, %v", got, err)
		}

		// Without the override the slug of the account names the key.
		t.Setenv("BRAIN_IMAP_PASSWORD_KEY", "")
		creds = newServerCredentials(staticSecrets{"imap/team-inbox/password": "secret"}, "imap", conn.ID, conn.Account)
		got, err = creds.Get(ctx, conn.ID, "password")
		if err != nil || got != "secret" {
			t.Fatalf("password = %q, %v", got, err)
		}
		if _, err := creds.Get(ctx, conn.ID, "token"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("field error = %v", err)
		}
	})

	t.Run("unknown provider", func(t *testing.T) {
		creds := newServerCredentials(staticSecrets{}, "slack", "slack-conn", "")
		if _, err := creds.Get(ctx, "slack-conn", "token"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("missing secret store", func(t *testing.T) {
		creds := newServerCredentials(nil, "github", "github-conn", "")
		if _, err := creds.Get(ctx, "github-conn", "token"); err == nil {
			t.Fatal("expected an error without a secret store")
		}
	})
}

func TestServerConnectionStoreIsReadOnly(t *testing.T) {
	t.Setenv("BRAIN_GITHUB_REPOS", "acme/widgets")
	conn, err := buildServerConnection(httpapi.ConnectorRunRequest{Name: "github"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	store := &serverConnectionStore{conn: conn}
	ctx := context.Background()

	listed, err := store.List(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != conn.ID {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	// The ingestion host reads the connection twice (before and under the
	// lock); both reads must agree for the ephemeral store.
	for range 2 {
		got, err := store.Get(ctx, conn.ID)
		if err != nil || !reflect.DeepEqual(got, conn) {
			t.Fatalf("get = %+v, %v", got, err)
		}
	}
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("get error = %v", err)
	}
	if err := store.Create(ctx, conn); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("create error = %v", err)
	}
	if err := store.Update(ctx, conn); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("update error = %v", err)
	}
	if err := store.Delete(ctx, conn.ID); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("delete error = %v", err)
	}
}
