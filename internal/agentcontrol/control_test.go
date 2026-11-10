package agentcontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/daemon"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type fakeSecrets struct {
	mu     sync.Mutex
	values map[string]string
}

func (f *fakeSecrets) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.values[key]
	if !ok {
		return "", domain.ErrNotFound
	}
	return value, nil
}

func (f *fakeSecrets) Set(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.values == nil {
		f.values = map[string]string{}
	}
	f.values[key] = value
	return nil
}

func (f *fakeSecrets) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values, key)
	return nil
}

func (f *fakeSecrets) Names(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.values))
	for name := range f.values {
		names = append(names, name)
	}
	return names, nil
}

type fakeProjects struct{ projects []domain.Project }

func (f fakeProjects) ListProjects(context.Context) ([]domain.Project, error) {
	return f.projects, nil
}

type fakeUpdater struct{}

func (fakeUpdater) Check(context.Context, string) (ports.ReleaseInfo, error) {
	return ports.ReleaseInfo{}, domain.ErrNotFound
}
func (fakeUpdater) Download(context.Context, ports.ReleaseInfo) (string, error) { return "", nil }
func (fakeUpdater) Apply(context.Context, string) error                         { return nil }
func (fakeUpdater) Rollback(context.Context) error                              { return nil }
func (fakeUpdater) CurrentVersion() string                                      { return "9.9.9" }

type fixtureSource struct{}

func (fixtureSource) Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:    "fixture",
		DisplayName: "Fixture",
		Capabilities: domain.Capabilities{
			Backfill: true, Incremental: true,
			AuthModes: []domain.AuthMode{domain.AuthNone},
		},
		DefaultPublish: domain.PublishShared,
	}
}
func (fixtureSource) Check(context.Context) error { return nil }
func (fixtureSource) Read(context.Context, ports.ReadRequest) (ports.ReadBatch, error) {
	return ports.ReadBatch{}, nil
}

type recordingRunner struct {
	mu   sync.Mutex
	runs int
}

func (r *recordingRunner) RunConnection(ctx context.Context, opts app.RunConnectionOptions) (app.RunConnectionResult, error) {
	// A real ingestion run outlives the HTTP request that queued it; fail if
	// the job context is canceled when the request ends.
	select {
	case <-ctx.Done():
		return app.RunConnectionResult{}, ctx.Err()
	case <-time.After(20 * time.Millisecond):
	}
	r.mu.Lock()
	r.runs++
	id := fmt.Sprintf("engine-run-%d", r.runs)
	r.mu.Unlock()
	started := time.Now().UTC().Add(-time.Second)
	finished := time.Now().UTC()
	return app.RunConnectionResult{Run: domain.ConnectorRun{
		ID: id, Source: "fixture", ConnectionID: opts.ConnectionID,
		StartedAt: started, FinishedAt: &finished, Status: domain.ConnectorSuccess,
		Stats: &domain.RunStats{Seen: 3, Upserted: 2, Unchanged: 1},
	}}, nil
}

// waitRuntime reads the owner-only runtime record written by the agent.
func waitRuntime(t *testing.T, dataDir string, done <-chan error) daemon.Runtime {
	t.Helper()
	path := filepath.Join(dataDir, "agent", "runtime.json")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(path)
		if err == nil {
			var runtime daemon.Runtime
			if err := json.Unmarshal(raw, &runtime); err == nil && runtime.Port > 0 && runtime.Token != "" {
				return runtime
			}
		}
		select {
		case err := <-done:
			t.Fatalf("agent exited before writing the runtime record: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("runtime record was not written under %s", path)
	return daemon.Runtime{}
}

type apiClient struct {
	t     *testing.T
	base  string
	token string
}

func (c apiClient) do(method, path string, body string, headers map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return resp, raw
}

// blockingConnectionStore forces the old Get -> Update interleaving by
// pausing the next Get after it has taken its snapshot.
type blockingConnectionStore struct {
	ports.ConnectionStore
	mu           sync.Mutex
	blockNextGet bool
	getStarted   chan struct{}
	getRelease   chan struct{}
	patchStarted chan struct{}
	patchOnce    sync.Once
}

func (s *blockingConnectionStore) Get(ctx context.Context, id string) (domain.Connection, error) {
	connection, err := s.ConnectionStore.Get(ctx, id)
	s.mu.Lock()
	block := s.blockNextGet
	if block {
		s.blockNextGet = false
	}
	s.mu.Unlock()
	if block {
		close(s.getStarted)
		<-s.getRelease
	}
	return connection, err
}

func (s *blockingConnectionStore) Patch(ctx context.Context, id string, mutate func(*domain.Connection) error) (domain.Connection, error) {
	s.patchOnce.Do(func() { close(s.patchStarted) })
	return s.ConnectionStore.Patch(ctx, id, mutate)
}

func TestConcurrentPatchAndScheduleSetPreserveFields(t *testing.T) {
	ctx := context.Background()
	store, err := fsstore.NewConnectionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, domain.Connection{ID: "work", Provider: "fixture", Account: "old"}); err != nil {
		t.Fatal(err)
	}
	blocking := &blockingConnectionStore{
		ConnectionStore: store,
		blockNextGet:    true,
		getStarted:      make(chan struct{}),
		getRelease:      make(chan struct{}),
		patchStarted:    make(chan struct{}),
	}
	schedules := scheduleStore{connections: blocking}
	done := make(chan error, 1)
	go func() { done <- schedules.SetSchedule(ctx, "work", "@hourly") }()
	select {
	case <-blocking.getStarted:
	case <-blocking.patchStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("SetSchedule did not reach the connection store")
	}
	if _, err := store.Patch(ctx, "work", func(connection *domain.Connection) error {
		connection.Settings = map[string]string{"repos": "owner/other"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(blocking.getRelease)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Settings["repos"] != "owner/other" || stored.Schedule != "@hourly" {
		t.Fatalf("lost update: %+v", stored)
	}
}

func TestControlEndToEnd(t *testing.T) {
	dir := t.TempDir()
	connections, err := fsstore.NewConnectionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := connections.Create(ctx, domain.Connection{ID: "work", Provider: "fixture"}); err != nil {
		t.Fatal(err)
	}
	secrets := &fakeSecrets{}
	registry := connectorbuild.NewRegistry()
	if err := registry.Register(fixtureSource{}.Descriptor(), func(domain.Connection, connectorbuild.Deps) (ports.Source, error) {
		return fixtureSource{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	control, err := New(Options{
		DataDir: dir, Connections: connections, Secrets: secrets, Registry: registry,
		Deps:     connectorbuild.Deps{Credentials: connectorbuild.SecretCredentials{Store: secrets}},
		Projects: fakeProjects{projects: []domain.Project{{ID: "demo", Name: "Demo"}}},
		Updater:  fakeUpdater{}, Version: "9.9.9",
	})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := daemon.New(daemon.Options{
		DataDir: dir, Version: "9.9.9", Connections: connections, Runner: runner,
		Control: control,
	})
	if err != nil {
		t.Fatal(err)
	}
	control.BindAgent(engine)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- engine.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	})
	runtime := waitRuntime(t, dir, done)
	base := fmt.Sprintf("http://127.0.0.1:%d", runtime.Port)
	api := apiClient{t: t, base: base, token: runtime.Token}

	// Authentication and Host checks.
	if resp, _ := (apiClient{t: t, base: base}).do(http.MethodGet, "/v1/health", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated health = %d", resp.StatusCode)
	}
	if resp, _ := api.do(http.MethodGet, "/v1/health", "", map[string]string{"Origin": "https://evil.example"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin health = %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/health", nil)
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer "+runtime.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host health = %d", resp.StatusCode)
	}

	// Health with the runtime token.
	resp, body := api.do(http.MethodGet, "/v1/health", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"9.9.9"`)) {
		t.Fatalf("health = %d %s", resp.StatusCode, body)
	}

	// Connections, projects and test probe.
	resp, body = api.do(http.MethodGet, "/v1/connections", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"work"`)) {
		t.Fatalf("connections = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodPost, "/v1/connections/work/test", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"ok":true`)) {
		t.Fatalf("test = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodGet, "/v1/projects", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"demo"`)) {
		t.Fatalf("projects = %d %s", resp.StatusCode, body)
	}

	// Settings round-trip through agent/settings.json.
	resp, body = api.do(http.MethodPut, "/v1/settings", `{"theme":"dark"}`, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"theme":"dark"`)) {
		t.Fatalf("put settings = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodGet, "/v1/settings", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"dark"`)) {
		t.Fatalf("get settings = %d %s", resp.StatusCode, body)
	}

	// Schedule PUT updates the connection catalogue.
	resp, body = api.do(http.MethodPut, "/v1/schedules/work", `{"schedule":"@hourly"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put schedule = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodGet, "/v1/schedules", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"@hourly"`)) {
		t.Fatalf("schedules = %d %s", resp.StatusCode, body)
	}

	// Queue a run; its job ID is immediately usable as a run ID.
	resp, body = api.do(http.MethodPost, "/v1/connections/work/run", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run = %d %s", resp.StatusCode, body)
	}
	var accepted struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil || accepted.JobID == "" {
		t.Fatalf("run response = %s (%v)", body, err)
	}
	var getResp *http.Response
	var getBody []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getResp, getBody = api.do(http.MethodGet, "/v1/runs/"+accepted.JobID, "", nil)
		if getResp.StatusCode == http.StatusOK && bytes.Contains(getBody, []byte(`"status":"success"`)) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if getResp.StatusCode != http.StatusOK || !bytes.Contains(getBody, []byte(accepted.JobID)) {
		t.Fatalf("run %s = %d %s", accepted.JobID, getResp.StatusCode, getBody)
	}
	if !bytes.Contains(getBody, []byte(`"status":"success"`)) {
		t.Fatalf("run did not finish successfully: %s", getBody)
	}
	resp, body = api.do(http.MethodGet, "/v1/runs", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(accepted.JobID)) {
		t.Fatalf("runs = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodGet, "/v1/runs/"+accepted.JobID+"/log", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("status=")) {
		t.Fatalf("run log = %d %s", resp.StatusCode, body)
	}
	resp, body = api.do(http.MethodPost, "/v1/engine/update", `{}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("update without confirmation = %d %s", resp.StatusCode, body)
	}

	// The shutdown endpoint stops the engine gracefully.
	resp, body = api.do(http.MethodPost, "/v1/shutdown", "", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown = %d %s", resp.StatusCode, body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("engine.Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine did not stop after /v1/shutdown")
	}
	done <- nil // keep the cleanup from waiting on a drained channel
}

func TestSettingsStoreRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	store := &settingsStore{path: filepath.Join(dir, "settings.json")}
	if err := os.WriteFile(store.path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSettings(context.Background()); err == nil {
		t.Fatal("corrupt settings file was accepted")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.10.0", "0.9.0", 1},
		{"v0.10.0", "0.10.0", 0},
		{"0.10.0", "0.10.1", -1},
		{"1.0.0-pre.1", "1.0.0", -1},
		{"1.0.0", "1.0.0-pre.1", 1},
		{"bad", "0.1.0", -1},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

type fakeJobTable struct {
	jobs     map[string]daemon.JobDetail
	canceled []string
}

func (f *fakeJobTable) JobDetails(id string) (daemon.JobDetail, bool) {
	detail, ok := f.jobs[id]
	return detail, ok
}

func (f *fakeJobTable) CancelJob(id string) bool {
	f.canceled = append(f.canceled, id)
	_, ok := f.jobs[id]
	return ok
}

func TestJobCancelerAcceptsOnlyActiveJobs(t *testing.T) {
	table := &fakeJobTable{jobs: map[string]daemon.JobDetail{
		"queued":  {View: daemon.JobView{ID: "queued", Status: "queued"}},
		"running": {View: daemon.JobView{ID: "running", Status: "running"}},
		"done":    {View: daemon.JobView{ID: "done", Status: "success"}},
		"failed":  {View: daemon.JobView{ID: "failed", Status: "failed"}},
	}}
	canceler := jobCanceler{jobs: table}
	for _, tc := range []struct{ id, want string }{
		{"queued", "queued"},
		{"running", "running"},
	} {
		got, err := canceler.CancelJob(context.Background(), tc.id)
		if err != nil || got != tc.want {
			t.Fatalf("CancelJob(%q) = %q, %v; want %q", tc.id, got, err, tc.want)
		}
	}
	if _, err := canceler.CancelJob(context.Background(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown job error = %v, want ErrNotFound", err)
	}
	for _, id := range []string{"done", "failed"} {
		if _, err := canceler.CancelJob(context.Background(), id); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("finished job %q error = %v, want ErrConflict", id, err)
		}
	}
	if strings.Join(table.canceled, ",") != "queued,running" {
		t.Fatalf("cancelled jobs = %v, want only active jobs", table.canceled)
	}
	cancelledContext, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := canceler.CancelJob(cancelledContext, "queued"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v", err)
	}
}

func TestRunFromJobReportsCanceledBeforeStartAsPartialCanceled(t *testing.T) {
	finished := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	run, ok := runFromJob(daemon.JobDetail{View: daemon.JobView{
		ID: "job-1", ConnectionID: "work", Status: "canceled",
		StartedAt: finished.Add(-time.Second), FinishedAt: finished,
	}})
	if !ok {
		t.Fatal("canceled job was hidden from the run list")
	}
	if run.Status != domain.ConnectorPartial {
		t.Fatalf("status = %q, want %q", run.Status, domain.ConnectorPartial)
	}
	if run.Stats == nil || run.Stats.Truncated != domain.TruncatedCanceled {
		t.Fatalf("stats = %+v, want truncated=canceled", run.Stats)
	}
	if run.FinishedAt == nil || !run.FinishedAt.Equal(finished) {
		t.Fatalf("finishedAt = %v, want %v", run.FinishedAt, finished)
	}
}

func TestRunSummaryExcludesSecrets(t *testing.T) {
	// The run record is sanitized by the ingestion host; this checks the
	// adapter never adds credential fields of its own.
	run := domain.ConnectorRun{
		ID: "run-1", Source: "fixture", ConnectionID: "work",
		StartedAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
		Status:    domain.ConnectorFailed,
		Error:     "request failed",
		Stats:     &domain.RunStats{Seen: 1, Upserted: 1},
	}
	lines := strings.Join(runSummary(run), "\n")
	for _, forbidden := range []string{"token", "password", "secret", "Authorization"} {
		if strings.Contains(strings.ToLower(lines), strings.ToLower(forbidden)+"=") {
			t.Fatalf("run summary leaks %q: %s", forbidden, lines)
		}
	}
}
