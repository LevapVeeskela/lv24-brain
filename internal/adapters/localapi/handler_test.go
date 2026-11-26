package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const testToken = "localapi-test-token-never-log"
const testPort = 43123

type fakeConnections struct {
	mu     sync.Mutex
	values map[string]domain.Connection

	// Optional hooks let tests force the old Get -> Update interleaving: the
	// next Get returns its snapshot and blocks until getRelease is closed.
	blockNextGet bool
	getStarted   chan struct{}
	getRelease   chan struct{}
	patchStarted chan struct{}
	patchOnce    sync.Once
}

func newFakeConnections() *fakeConnections {
	return &fakeConnections{values: map[string]domain.Connection{}}
}
func (f *fakeConnections) List(context.Context) ([]domain.Connection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Connection, 0, len(f.values))
	for _, v := range f.values {
		out = append(out, v)
	}
	return out, nil
}
func (f *fakeConnections) Get(_ context.Context, id string) (domain.Connection, error) {
	f.mu.Lock()
	v, ok := f.values[id]
	block := f.blockNextGet
	if block {
		f.blockNextGet = false
	}
	f.mu.Unlock()
	if !ok {
		return domain.Connection{}, domain.ErrNotFound
	}
	if block {
		if f.getStarted != nil {
			close(f.getStarted)
		}
		if f.getRelease != nil {
			<-f.getRelease
		}
	}
	return v, nil
}
func (f *fakeConnections) Create(_ context.Context, c domain.Connection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.values[c.ID]; ok {
		return domain.ErrConflict
	}
	f.values[c.ID] = c
	return nil
}
func (f *fakeConnections) Update(_ context.Context, c domain.Connection) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.values[c.ID]; !ok {
		return domain.ErrNotFound
	}
	f.values[c.ID] = c
	return nil
}
func (f *fakeConnections) Patch(_ context.Context, id string, mutate func(*domain.Connection) error) (domain.Connection, error) {
	if f.patchStarted != nil {
		f.patchOnce.Do(func() { close(f.patchStarted) })
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[id]
	if !ok {
		return domain.Connection{}, domain.ErrNotFound
	}
	if err := mutate(&v); err != nil {
		return domain.Connection{}, err
	}
	f.values[id] = v
	return v, nil
}
func (f *fakeConnections) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.values[id]; !ok {
		return domain.ErrNotFound
	}
	delete(f.values, id)
	return nil
}

type fakeChecker struct{ err error }

func (f fakeChecker) Check(context.Context) error { return f.err }

type fakeRegistry struct {
	checkErr   error
	checkerErr error
}

func (f fakeRegistry) Descriptor(provider string) (domain.SourceDescriptor, error) {
	if provider != "demo" {
		return domain.SourceDescriptor{}, domain.ErrNotFound
	}
	return demoDescriptor(), nil
}
func (f fakeRegistry) Checker(context.Context, domain.Connection) (SourceChecker, error) {
	if f.checkerErr != nil {
		return nil, f.checkerErr
	}
	return fakeChecker{f.checkErr}, nil
}
func demoDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{Provider: "demo", DisplayName: "Demo", Capabilities: domain.Capabilities{AuthModes: []domain.AuthMode{domain.AuthToken}}, DefaultPublish: domain.PublishLocalOnly, ConfigFields: []domain.ConfigField{{Key: "endpoint", Label: "Endpoint"}, {Key: "token", Label: "Token", Secret: true}}}
}

type fakeCredentials struct{ value string }

func (f *fakeCredentials) Set(_ context.Context, _, _, value string) error {
	f.value = value
	return nil
}

type fakeJobs struct {
	ids  []string
	opts []RunOptions
	err  error
}

func (f *fakeJobs) Queue(_ context.Context, id string, opts RunOptions) (string, error) {
	f.ids = append(f.ids, id)
	f.opts = append(f.opts, opts)
	if f.err != nil {
		return "", f.err
	}
	return "job-1", nil
}

type fakeCancels struct {
	statuses map[string]string
	ids      []string
	err      error
}

func (f *fakeCancels) CancelJob(_ context.Context, id string) (string, error) {
	f.ids = append(f.ids, id)
	if f.err != nil {
		return "", f.err
	}
	status, ok := f.statuses[id]
	if !ok {
		return "", domain.ErrNotFound
	}
	if status != "queued" && status != "running" {
		return "", domain.ErrConflict
	}
	return status, nil
}

type fakeRunLog struct {
	lines  chan LogLine
	closed chan struct{}
	once   sync.Once
}

func newFakeRunLog() *fakeRunLog {
	return &fakeRunLog{lines: make(chan LogLine, 2), closed: make(chan struct{})}
}
func (f *fakeRunLog) Lines() <-chan LogLine { return f.lines }
func (f *fakeRunLog) Close() error          { f.once.Do(func() { close(f.closed) }); return nil }

type fakeRuns struct {
	runs []domain.ConnectorRun
	log  *fakeRunLog
}

func (f *fakeRuns) ListRuns(context.Context, int) ([]domain.ConnectorRun, error) { return f.runs, nil }
func (f *fakeRuns) GetRun(_ context.Context, id string) (domain.ConnectorRun, error) {
	for _, r := range f.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return domain.ConnectorRun{}, domain.ErrNotFound
}
func (f *fakeRuns) OpenRunLog(context.Context, string) (RunLog, error) {
	if f.log == nil {
		return nil, domain.ErrNotFound
	}
	return f.log, nil
}

type fakeSchedules struct {
	items       []Schedule
	connections *fakeConnections
	id, value   string
}

func (f *fakeSchedules) ListSchedules(context.Context) ([]Schedule, error) { return f.items, nil }
func (f *fakeSchedules) SetSchedule(ctx context.Context, id, value string) error {
	if f.connections != nil {
		if _, err := f.connections.Patch(ctx, id, func(connection *domain.Connection) error {
			connection.Schedule = value
			return nil
		}); err != nil {
			return err
		}
	}
	f.id, f.value = id, value
	return nil
}

// patchingSchedules mirrors the production schedule store: SetSchedule writes
// the connection through its atomic Patch primitive.
type patchingSchedules struct{ connections *fakeConnections }

func (patchingSchedules) ListSchedules(context.Context) ([]Schedule, error) { return nil, nil }
func (s patchingSchedules) SetSchedule(ctx context.Context, id, value string) error {
	_, err := s.connections.Patch(ctx, id, func(connection *domain.Connection) error {
		connection.Schedule = value
		return nil
	})
	return err
}

type fakeProjects struct{ items []Project }

func (f fakeProjects) ListProjects(context.Context) ([]Project, error) { return f.items, nil }

type fakeSync struct {
	status  SyncView
	result  SyncView
	err     error
	pushed  bool
	statusC int
}

func (f *fakeSync) Status(context.Context) (SyncView, error) {
	f.statusC++
	return f.status, nil
}

func (f *fakeSync) Sync(_ context.Context, push bool) (SyncView, error) {
	f.pushed = push
	return f.result, f.err
}

type fakeSettings struct{ values map[string]string }

func (f *fakeSettings) GetSettings(context.Context) (map[string]string, error) { return f.values, nil }
func (f *fakeSettings) PutSettings(_ context.Context, v map[string]string) error {
	f.values = v
	return nil
}

// codedFakeError lets a test return the stale_plan code from any route.
type codedFakeError struct {
	code    string
	message string
}

func (e codedFakeError) Error() string { return e.message }
func (e codedFakeError) Code() string  { return e.code }

// fakeWorkspace models the merge, digest, path and journal use cases and
// records the request of every route so a test can prove the handler resolved
// and validated it first.
type fakeWorkspace struct {
	plan        MergePlan
	planErr     error
	apply       MergeResult
	applyErr    error
	rollback    MergeRollbackResult
	rollbackErr error
	digest      DigestView
	digestErr   error
	adopt       AdoptReportView
	adoptErr    error
	project     Project
	pathErr     error
	journal     JournalPage
	journalErr  error

	lastPlan     MergePlanRequest
	lastApply    MergeApplyRequest
	lastRollback MergeRollbackRequest
	lastDigest   DigestRequest
	lastAdopt    bool
	lastPath     AddProjectPathRequest
	lastJournal  JournalQuery
}

func (f *fakeWorkspace) PlanMerge(_ context.Context, request MergePlanRequest) (MergePlan, error) {
	f.lastPlan = request
	return f.plan, f.planErr
}

func (f *fakeWorkspace) ApplyMerge(_ context.Context, request MergeApplyRequest) (MergeResult, error) {
	f.lastApply = request
	return f.apply, f.applyErr
}

func (f *fakeWorkspace) RollbackMerge(_ context.Context, request MergeRollbackRequest) (MergeRollbackResult, error) {
	f.lastRollback = request
	return f.rollback, f.rollbackErr
}

func (f *fakeWorkspace) Digest(_ context.Context, request DigestRequest) (DigestView, error) {
	f.lastDigest = request
	return f.digest, f.digestErr
}

func (f *fakeWorkspace) AdoptLocalPaths(_ context.Context, apply bool) (AdoptReportView, error) {
	f.lastAdopt = apply
	return f.adopt, f.adoptErr
}

func (f *fakeWorkspace) AddProjectPath(_ context.Context, request AddProjectPathRequest) (Project, error) {
	f.lastPath = request
	return f.project, f.pathErr
}

func (f *fakeWorkspace) Journal(_ context.Context, query JournalQuery) (JournalPage, error) {
	f.lastJournal = query
	return f.journal, f.journalErr
}

type fakeHooks struct {
	items []HookStatus
	err   error
}

func (f *fakeHooks) Hooks(context.Context) ([]HookStatus, error) { return f.items, f.err }

type fakeUpdater struct{ calls int }

func (f *fakeUpdater) Update(context.Context) (UpdateResult, error) {
	f.calls++
	return UpdateResult{Available: true, Applied: true, Version: "1.2.3"}, nil
}

type testFixture struct {
	handler     *Handler
	connections *fakeConnections
	credentials *fakeCredentials
	jobs        *fakeJobs
	cancels     *fakeCancels
	runs        *fakeRuns
	schedules   *fakeSchedules
	settings    *fakeSettings
	updater     *fakeUpdater
	sync        *fakeSync
	workspace   *fakeWorkspace
	hooks       *fakeHooks
	ideas       *fakeIdeas
	shutdown    chan struct{}
}

type synchronizedWriter struct {
	mu     sync.Mutex
	header http.Header
	status int
	body   bytes.Buffer
}

func newSynchronizedWriter() *synchronizedWriter {
	return &synchronizedWriter{header: make(http.Header)}
}
func (w *synchronizedWriter) Header() http.Header { return w.header }
func (w *synchronizedWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = code
	}
}
func (w *synchronizedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}
func (w *synchronizedWriter) Flush() {}
func (w *synchronizedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func newFixture(t *testing.T) *testFixture {
	t.Helper()
	f := &testFixture{connections: newFakeConnections(), credentials: &fakeCredentials{}, jobs: &fakeJobs{}, cancels: &fakeCancels{statuses: map[string]string{"run-1": "queued"}}, runs: &fakeRuns{log: newFakeRunLog()}, schedules: &fakeSchedules{}, settings: &fakeSettings{values: map[string]string{"theme": "dark", "api_token": "should-not-escape"}}, updater: &fakeUpdater{}, sync: &fakeSync{status: SyncView{Provider: "git", Branch: "main", Conflicts: []string{}, ConflictRecords: []string{}}, result: SyncView{Provider: "git", Branch: "main", Conflicts: []string{}, ConflictRecords: []string{}}}, shutdown: make(chan struct{})}
	f.workspace = &fakeWorkspace{
		plan: MergePlan{PlanID: "plan-1", Project: "brain", Survivor: MergeRecordView{ID: "note-1"},
			Duplicates: []MergeRecordView{}, Conflicts: []string{}},
		apply:    MergeResult{PlanID: "plan-1", Project: "brain", Applied: true, Manifest: "merges/2026.json", Files: []MergeFileView{}},
		rollback: MergeRollbackResult{PlanID: "plan-1", Manifest: "merges/2026.json", Files: []MergeFileView{}},
		digest:   DigestView{Projects: []DigestProjectView{}},
		adopt:    AdoptReportView{Machine: "test", Projects: []ProjectOwnershipView{}},
		project:  Project{ID: "brain", Name: "Brain", Paths: []string{}},
		journal:  JournalPage{Items: []JournalView{}, Limit: 50},
	}
	f.hooks = &fakeHooks{items: []HookStatus{{Host: "claude", Path: "/home/.claude/settings.json", Installed: true, Detail: "ok"}}}
	f.ideas = newFakeIdeas()
	f.schedules.connections = f.connections
	var err error
	f.handler, err = NewHandler(Options{Token: TokenFunc(func() string { return testToken }), Port: testPort, Version: "test", PID: 99, StartedAt: time.Now().Add(-time.Minute), Now: time.Now, MaxBody: 256, Heartbeat: 10 * time.Millisecond, Connections: f.connections, Sources: fakeRegistry{}, Credentials: f.credentials, Jobs: f.jobs, Cancels: f.cancels, Runs: f.runs, Schedules: f.schedules, Projects: fakeProjects{items: []Project{{ID: "brain", Name: "Brain"}}}, Settings: f.settings, Sync: f.sync, Merge: f.workspace, Digest: f.workspace, ProjectAdmin: f.workspace, Journal: f.workspace, Hooks: f.hooks, Ideas: f.ideas, DataDir: "C:/brain-data", Updater: f.updater, Shutdown: ShutdownFunc(func(context.Context) error { close(f.shutdown); return nil })})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func request(f *testFixture, method, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = net.JoinHostPort("127.0.0.1", strconvI(testPort))
	r.Header.Set("Authorization", "Bearer "+testToken)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func strconvI(value int) string { return strconv.Itoa(value) }
func decodeMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAuthHostOriginAndNoCORS(t *testing.T) {
	f := newFixture(t)
	host := "127.0.0.1:" + strconvI(testPort)
	cases := []struct {
		name, auth, host, origin string
		want                     int
	}{{"missing", "", host, "", 401}, {"wrong", "Bearer wrong", host, "", 401}, {"valid", "Bearer " + testToken, host, "", 200}, {"localhost host", "Bearer " + testToken, "localhost:" + strconvI(testPort), "", 403}, {"IPv6 loopback host", "Bearer " + testToken, "[::1]:" + strconvI(testPort), "", 403}, {"host suffix", "Bearer " + testToken, host + ".evil", "", 403}, {"dns rebinding", "Bearer " + testToken, "evil.test:" + strconvI(testPort), "", 403}, {"loopback alias mismatch", "Bearer " + testToken, "127.0.0.1:80", "", 403}, {"null origin", "Bearer " + testToken, host, "null", 403}, {"same-site origin", "Bearer " + testToken, host, "http://127.0.0.1:" + strconvI(testPort), 403}, {"localhost origin", "Bearer " + testToken, host, "http://localhost:" + strconvI(testPort), 403}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
			r.Host = tc.host
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("CORS header was emitted")
			}
		})
	}
	if got := request(f, http.MethodGet, "/v1/health", "", nil); got.Code != 200 {
		t.Fatal(got.Code)
	}
	if got := request(f, http.MethodGet, "/v1/health", "", func(r *http.Request) { r.Header["Origin"] = []string{""} }); got.Code != 403 {
		t.Fatalf("empty Origin header accepted: %d", got.Code)
	}
	if got := request(f, http.MethodGet, "/v1/health", "", func(r *http.Request) { r.Header.Add("Authorization", "Bearer wrong") }); got.Code != 401 {
		t.Fatalf("duplicate Authorization accepted: %d", got.Code)
	}
}

func TestConnectionCRUDValidationSecretsAndCredentialSetter(t *testing.T) {
	f := newFixture(t)
	body := `{"id":"demo-1","provider":"demo","baseUrl":"https://example.test?access_token=secret-in-query","settings":{"endpoint":"https://example.test"}}`
	w := request(f, http.MethodPost, "/v1/connections", body, nil)
	if w.Code != 400 || strings.Contains(w.Body.String(), "secret-in-query") {
		t.Fatalf("query-secret create=%d %s", w.Code, w.Body.String())
	}
	body = `{"id":"demo-1","provider":"demo","baseUrl":"https://example.test","settings":{"endpoint":"https://example.test"}}`
	w = request(f, http.MethodPost, "/v1/connections", body, nil)
	if w.Code != 201 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("create=%d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "token") {
		t.Fatalf("secret field leaked: %s", w.Body.String())
	}
	w = request(f, http.MethodGet, "/v1/connections", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "demo-1") {
		t.Fatalf("list=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodGet, "/v1/connections/demo-1", "", nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(f, http.MethodPatch, "/v1/connections/demo-1", `{"account":"workspace"}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "workspace") || !strings.Contains(w.Body.String(), "https://example.test") {
		t.Fatalf("partial patch=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodPatch, "/v1/connections/demo-1", `{"id":"demo-1","provider":"demo","settings":{"token":"NEVER-RESPONSE"}}`, nil)
	if w.Code != 400 || strings.Contains(w.Body.String(), "NEVER-RESPONSE") {
		t.Fatalf("secret patch=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodPost, "/v1/connections", `{"id":"bad","provider":"demo","settings":{"unknown":"x"}}`, nil)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = request(f, http.MethodPost, "/v1/connections/demo-1/credentials/token", `{"value":"credential-secret-value"}`, nil)
	if w.Code != 204 || f.credentials.value != "credential-secret-value" || strings.Contains(w.Body.String(), f.credentials.value) {
		t.Fatalf("credential setter=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodDelete, "/v1/connections/demo-1", "", nil)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	w = request(f, http.MethodGet, "/v1/connections/demo-1", "", nil)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestRunQueueBusyTestAndRunViews(t *testing.T) {
	f := newFixture(t)
	_ = f.connections.Create(context.Background(), domain.Connection{ID: "demo-1", Provider: "demo", Settings: map[string]string{"endpoint": "x"}})
	w := request(f, http.MethodPost, "/v1/connections/demo-1/run", `{"limit":12,"days":3}`, nil)
	if w.Code != 202 || len(f.jobs.ids) != 1 || f.jobs.opts[0].Limit != 12 || f.jobs.opts[0].Days != 3 {
		t.Fatalf("queue=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodPost, "/v1/connections/demo-1/run", "", nil)
	if w.Code != 202 || len(f.jobs.ids) != 2 {
		t.Fatalf("empty run body=%d %s", w.Code, w.Body.String())
	}
	f.jobs.err = domain.ErrConflict
	w = request(f, http.MethodPost, "/v1/connections/demo-1/run", `{}`, nil)
	if w.Code != 409 || len(f.jobs.ids) != 3 {
		t.Fatalf("busy=%d %s", w.Code, w.Body.String())
	}
	f.sourcesErrForTest()
	if got := request(f, http.MethodPost, "/v1/connections/demo-1/test", "", nil); got.Code != 200 {
		t.Fatal(got.Code)
	}
	f.runs.runs = []domain.ConnectorRun{{ID: "run-1", Source: "demo", ConnectionID: "demo-1", Status: domain.ConnectorFailed, Error: "credential-secret-value"}}
	w = request(f, http.MethodGet, "/v1/runs", "", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "credential-secret-value") {
		t.Fatalf("run list leaked: %d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodGet, "/v1/runs/run-1", "", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "credential-secret-value") {
		t.Fatalf("run get leaked: %d %s", w.Code, w.Body.String())
	}
}

func (f *testFixture) sourcesErrForTest() {}

func TestRunCancelAcceptsActiveJobsAndRejectsUnknownOrFinished(t *testing.T) {
	f := newFixture(t)
	f.cancels.statuses = map[string]string{"queued-job": "queued", "running-job": "running", "done-job": "success"}
	for _, jobID := range []string{"queued-job", "running-job"} {
		w := request(f, http.MethodPost, "/v1/runs/"+jobID+"/cancel", "", nil)
		if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"status":"canceling"`) {
			t.Fatalf("cancel %s = %d %s", jobID, w.Code, w.Body.String())
		}
	}
	if got := request(f, http.MethodPost, "/v1/runs/missing/cancel", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("unknown cancel = %d %s", got.Code, got.Body.String())
	}
	if got := request(f, http.MethodPost, "/v1/runs/done-job/cancel", "", nil); got.Code != http.StatusConflict {
		t.Fatalf("finished cancel = %d %s", got.Code, got.Body.String())
	}
	if got := request(f, http.MethodGet, "/v1/runs/queued-job/cancel", "", nil); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != "POST" {
		t.Fatalf("cancel method = %d Allow=%q", got.Code, got.Header().Get("Allow"))
	}
	if got := strings.Join(f.cancels.ids, ","); got != "queued-job,running-job,missing,done-job" {
		t.Fatalf("cancel calls = %s", got)
	}
}

func TestRunCancelServiceErrorIsSanitized(t *testing.T) {
	f := newFixture(t)
	f.cancels.err = errors.New("cancel failed with " + testToken)
	w := request(f, http.MethodPost, "/v1/runs/run-1/cancel", "", nil)
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("unsafe cancel error: %d %s", w.Code, w.Body.String())
	}
}

func TestTestEndpointSanitizesProviderFailure(t *testing.T) {
	f := newFixture(t)
	_ = f.connections.Create(context.Background(), domain.Connection{ID: "demo-1", Provider: "demo"})
	f.handler.opts.Sources = fakeRegistry{checkErr: errors.New("request failed using " + testToken)}
	w := request(f, http.MethodPost, "/v1/connections/demo-1/test", "", nil)
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("unsafe test response: %d %s", w.Code, w.Body.String())
	}
	envelope := decodeMap(t, w.Body.String())
	errorBody, ok := envelope["error"].(map[string]any)
	if !ok || errorBody["code"] != app.ConnectionCodeCheckFailed {
		t.Fatalf("envelope=%s", w.Body.String())
	}
	if message, _ := errorBody["message"].(string); message == "" || !strings.Contains(message, "[redacted]") {
		t.Fatalf("message=%q", message)
	}
}

func TestTestEndpointReportsMissingCredential(t *testing.T) {
	f := newFixture(t)
	_ = f.connections.Create(context.Background(), domain.Connection{ID: "demo-1", Provider: "demo"})
	f.handler.opts.Sources = fakeRegistry{checkErr: fmt.Errorf("credential lookup for %s: %w", testToken, domain.ErrNotFound)}
	w := request(f, http.MethodPost, "/v1/connections/demo-1/test", "", nil)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("missing credential response: %d %s", w.Code, w.Body.String())
	}
	envelope := decodeMap(t, w.Body.String())
	errorBody, ok := envelope["error"].(map[string]any)
	if !ok || errorBody["code"] != app.ConnectionCodeCredentialMissing {
		t.Fatalf("envelope=%s", w.Body.String())
	}
}

func TestTestEndpointReportsUnsupportedProviderWithoutInternalError(t *testing.T) {
	f := newFixture(t)
	_ = f.connections.Create(context.Background(), domain.Connection{ID: "demo-1", Provider: "demo"})
	f.handler.opts.Sources = fakeRegistry{checkerErr: fmt.Errorf("unknown provider: %w", domain.ErrNotFound)}
	w := request(f, http.MethodPost, "/v1/connections/demo-1/test", "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unsupported response: %d %s", w.Code, w.Body.String())
	}
	envelope := decodeMap(t, w.Body.String())
	errorBody, ok := envelope["error"].(map[string]any)
	if !ok || errorBody["code"] != app.ConnectionCodeUnsupported {
		t.Fatalf("envelope=%s", w.Body.String())
	}
}

func TestRunsSSEHeartbeatCancellationAndRedaction(t *testing.T) {
	f := newFixture(t)
	f.runs.log.lines <- LogLine{Text: "safe-line " + testToken}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/v1/runs/run-1/log", nil).WithContext(ctx)
	r.Host = "127.0.0.1:" + strconvI(testPort)
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := newSynchronizedWriter()
	done := make(chan struct{})
	go func() { f.handler.ServeHTTP(w, r); close(done) }()
	deadline := time.After(time.Second)
	for !strings.Contains(w.String(), "data: safe-line") || !strings.Contains(w.String(), "heartbeat") {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("SSE did not emit line and heartbeat: %q", w.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if strings.Contains(w.String(), testToken) {
		cancel()
		<-done
		t.Fatal("token leaked to SSE")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE ignored request cancellation")
	}
	select {
	case <-f.runs.log.closed:
	default:
		t.Fatal("log producer was not closed")
	}
}

func TestSyncStatusAndAction(t *testing.T) {
	f := newFixture(t)
	f.sync.status = SyncView{Provider: "git", Branch: "main", Ahead: 2, Dirty: true, Conflicts: []string{}, ConflictRecords: []string{}}
	w := request(f, http.MethodGet, "/v1/sync", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var view SyncView
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Provider != "git" || view.Branch != "main" || view.Ahead != 2 || !view.Dirty {
		t.Fatalf("status view = %+v", view)
	}

	w = request(f, http.MethodPost, "/v1/sync", `{"push":true}`, nil)
	if w.Code != http.StatusOK || !f.sync.pushed {
		t.Fatalf("sync = %d pushed=%t: %s", w.Code, f.sync.pushed, w.Body.String())
	}

	f.sync.err = fmt.Errorf("sync: %w: unresolved conflicts in state.md", domain.ErrConflict)
	w = request(f, http.MethodPost, "/v1/sync", `{"push":true}`, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("conflict = %d: %s", w.Code, w.Body.String())
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "sync_conflict" {
		t.Fatalf("conflict code = %q", envelope.Error.Code)
	}
}

func TestSyncRouteUnavailableWithoutProvider(t *testing.T) {
	f := newFixture(t)
	f.handler.opts.Sync = nil
	if w := request(f, http.MethodGet, "/v1/sync", "", nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("unavailable = %d: %s", w.Code, w.Body.String())
	}
}

func TestScheduleProjectsSettingsUpdateShutdown(t *testing.T) {
	f := newFixture(t)
	_ = f.connections.Create(context.Background(), domain.Connection{ID: "demo-1", Provider: "demo"})
	w := request(f, http.MethodPut, "/v1/schedules/demo-1", `{"schedule":"@every 30s"}`, nil)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = request(f, http.MethodPut, "/v1/schedules/demo-1", `{"schedule":"@every 1m"}`, nil)
	if w.Code != 200 || f.schedules.value != "@every 1m" {
		t.Fatalf("schedule=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodPut, "/v1/schedules/missing", `{"schedule":"@hourly"}`, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown schedule=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodGet, "/v1/projects", "", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "paths") {
		t.Fatalf("projects=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodGet, "/v1/settings", "", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "api_token") || strings.Contains(w.Body.String(), "should-not-escape") {
		t.Fatalf("settings leak=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodPut, "/v1/settings", `{"theme":"light","password":"dont-return"}`, nil)
	if w.Code != 400 || strings.Contains(w.Body.String(), "dont-return") {
		t.Fatalf("secret settings=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodPut, "/v1/settings", `{"theme":"light"}`, nil)
	if w.Code != 200 || f.settings.values["theme"] != "light" {
		t.Fatalf("settings=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodPost, "/v1/engine/update", `{"confirm":false}`, nil)
	if w.Code != 400 || f.updater.calls != 0 {
		t.Fatalf("unconfirmed update=%d calls=%d", w.Code, f.updater.calls)
	}
	w = request(f, http.MethodPost, "/v1/engine/update", `{"confirm":true}`, nil)
	if w.Code != 200 || f.updater.calls != 1 {
		t.Fatalf("confirmed update=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodPost, "/v1/shutdown", "", nil)
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	select {
	case <-f.shutdown:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback not invoked")
	}
}

// waitForConnectionMutation returns once a request has either taken the old
// Get snapshot (and is blocked) or entered the atomic Patch path.
func waitForConnectionMutation(t *testing.T, connections *fakeConnections) {
	t.Helper()
	select {
	case <-connections.getStarted:
	case <-connections.patchStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach the connection store")
	}
}

func TestConcurrentPatchesPreserveDisjointFields(t *testing.T) {
	f := newFixture(t)
	if err := f.connections.Create(context.Background(), domain.Connection{ID: "demo-1", Provider: "demo", Account: "old"}); err != nil {
		t.Fatal(err)
	}
	f.connections.blockNextGet = true
	f.connections.getStarted = make(chan struct{})
	f.connections.getRelease = make(chan struct{})
	f.connections.patchStarted = make(chan struct{})

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- request(f, http.MethodPatch, "/v1/connections/demo-1", `{"settings":{"endpoint":"new"}}`, nil)
	}()
	waitForConnectionMutation(t, f.connections)
	second := request(f, http.MethodPatch, "/v1/connections/demo-1", `{"schedule":"@hourly"}`, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second patch=%d %s", second.Code, second.Body.String())
	}
	close(f.connections.getRelease)
	if got := <-first; got.Code != http.StatusOK {
		t.Fatalf("first patch=%d %s", got.Code, got.Body.String())
	}
	stored, err := f.connections.Get(context.Background(), "demo-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Settings["endpoint"] != "new" || stored.Schedule != "@hourly" {
		t.Fatalf("lost update: %+v", stored)
	}
}

func TestConcurrentPatchAndSchedulePutPreserveDisjointFields(t *testing.T) {
	f := newFixture(t)
	if err := f.connections.Create(context.Background(), domain.Connection{ID: "demo-1", Provider: "demo", Account: "old"}); err != nil {
		t.Fatal(err)
	}
	f.handler.opts.Schedules = patchingSchedules{connections: f.connections}
	f.connections.blockNextGet = true
	f.connections.getStarted = make(chan struct{})
	f.connections.getRelease = make(chan struct{})
	f.connections.patchStarted = make(chan struct{})

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- request(f, http.MethodPatch, "/v1/connections/demo-1", `{"settings":{"endpoint":"new"}}`, nil)
	}()
	waitForConnectionMutation(t, f.connections)
	w := request(f, http.MethodPut, "/v1/schedules/demo-1", `{"schedule":"@daily"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("schedule put=%d %s", w.Code, w.Body.String())
	}
	close(f.connections.getRelease)
	if got := <-first; got.Code != http.StatusOK {
		t.Fatalf("patch=%d %s", got.Code, got.Body.String())
	}
	stored, err := f.connections.Get(context.Background(), "demo-1")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Settings["endpoint"] != "new" || stored.Schedule != "@daily" {
		t.Fatalf("lost update: %+v", stored)
	}
}

func TestConnectionPatchReplacesSettingsAndProjects(t *testing.T) {
	f := newFixture(t)
	if err := f.connections.Create(context.Background(), domain.Connection{
		ID: "demo-1", Provider: "demo",
		Projects: []string{"old"}, Settings: map[string]string{"endpoint": "old", "extra": "drop"},
	}); err != nil {
		t.Fatal(err)
	}
	w := request(f, http.MethodPatch, "/v1/connections/demo-1", `{"projects":["new"],"settings":{"endpoint":"new"}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("patch=%d %s", w.Code, w.Body.String())
	}
	stored, err := f.connections.Get(context.Background(), "demo-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Projects) != 1 || stored.Projects[0] != "new" {
		t.Fatalf("projects = %v, want replace", stored.Projects)
	}
	if len(stored.Settings) != 1 || stored.Settings["endpoint"] != "new" {
		t.Fatalf("settings = %v, want replace", stored.Settings)
	}
}

func TestBodyLimitContentTypeAndMethod(t *testing.T) {
	f := newFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/connections", strings.NewReader(`{"id":"demo-1","provider":"demo","settings":{"endpoint":"`+strings.Repeat("x", 256)+`"}}`))
	r.Host = "127.0.0.1:" + strconvI(testPort)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("body=%d %s", w.Code, w.Body.String())
	}
	w = request(f, http.MethodPut, "/v1/settings", `{}`, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
	if w.Code != 415 {
		t.Fatal(w.Code)
	}
	w = request(f, http.MethodDelete, "/v1/health", "", nil)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" {
		t.Fatalf("method mismatch=%d Allow=%q", w.Code, w.Header().Get("Allow"))
	}
}

func TestMethodMismatchReturns405WithAllow(t *testing.T) {
	cases := []struct {
		name, method, path, allow string
	}{
		{"health", http.MethodPost, "/v1/health", "GET"},
		{"connections collection", http.MethodDelete, "/v1/connections", "GET, POST"},
		{"connection item", http.MethodPost, "/v1/connections/demo-1", "GET, PATCH, DELETE"},
		{"credentials", http.MethodGet, "/v1/connections/demo-1/credentials/token", "POST"},
		{"connection test", http.MethodGet, "/v1/connections/demo-1/test", "POST"},
		{"connection run", http.MethodGet, "/v1/connections/demo-1/run", "POST"},
		{"runs collection", http.MethodPost, "/v1/runs", "GET"},
		{"run item", http.MethodDelete, "/v1/runs/run-1", "GET"},
		{"run log", http.MethodPost, "/v1/runs/run-1/log", "GET"},
		{"run cancel", http.MethodGet, "/v1/runs/run-1/cancel", "POST"},
		{"schedules collection", http.MethodPost, "/v1/schedules", "GET"},
		{"schedule item", http.MethodGet, "/v1/schedules/demo-1", "PUT"},
		{"projects", http.MethodPost, "/v1/projects", "GET"},
		{"settings", http.MethodDelete, "/v1/settings", "GET, PUT"},
		{"engine update", http.MethodGet, "/v1/engine/update", "POST"},
		{"shutdown", http.MethodGet, "/v1/shutdown", "POST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			w := request(f, tc.method, tc.path, "", nil)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Allow"); got != tc.allow {
				t.Fatalf("Allow=%q, want %q", got, tc.allow)
			}
			envelope := decodeMap(t, w.Body.String())
			errorBody, ok := envelope["error"].(map[string]any)
			if !ok || errorBody["code"] != "method_not_allowed" {
				t.Fatalf("envelope=%s", w.Body.String())
			}
			if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("content type=%q", w.Header().Get("Content-Type"))
			}
		})
	}
}

func TestRunLogTruncationIsSignalled(t *testing.T) {
	f := newFixture(t)
	// Fill the producer exactly to the cap; the handler must stop there and
	// append a named truncation event instead of ending the stream silently.
	// The producer runs concurrently because the fixture buffer holds only two
	// lines and nothing consumes it before ServeHTTP starts.
	go func() {
		for i := 0; i < MaxLogLines; i++ {
			f.runs.log.lines <- LogLine{Text: "line"}
		}
	}()
	w := request(f, http.MethodGet, "/v1/runs/run-1/log", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "event: truncated\n") {
		t.Fatalf("missing truncation event: %q", w.Body.String())
	}
	if strings.Count(w.Body.String(), "data: line\n") != MaxLogLines {
		t.Fatalf("unexpected data events: %d", strings.Count(w.Body.String(), "data: line\n"))
	}
}

func TestServerBindsLoopbackAndReportsPort(t *testing.T) {
	s, err := NewServer(Options{Token: TokenFunc(func() string { return testToken }), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Listener.Close()
	address := s.Listener.Addr().(*net.TCPAddr)
	t.Logf("selected loopback port: %d", s.Port())
	if !address.IP.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("listener bound to %s", address.IP)
	}
	if s.Port() < 1 || s.Handler.opts.Port != s.Port() {
		t.Fatalf("port mismatch listener=%d handler=%d", s.Port(), s.Handler.opts.Port)
	}
}

func TestOpenAPIParsesAndListsRoutes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(".", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document.OpenAPI != "3.0.3" {
		t.Fatalf("version=%q", document.OpenAPI)
	}
	methods := map[string][]string{"/v1/health": {"get"}, "/v1/connections": {"get", "post"}, "/v1/connections/{id}": {"get", "patch", "delete"}, "/v1/connections/{id}/test": {"post"}, "/v1/connections/{id}/run": {"post"}, "/v1/connections/{id}/credentials/{field}": {"post"}, "/v1/runs": {"get"}, "/v1/runs/{id}": {"get"}, "/v1/runs/{id}/cancel": {"post"}, "/v1/runs/{id}/log": {"get"}, "/v1/schedules": {"get"}, "/v1/schedules/{connectionId}": {"put"}, "/v1/projects": {"get"}, "/v1/settings": {"get", "put"}, "/v1/engine/update": {"post"}, "/v1/shutdown": {"post"}}
	for path, expected := range methods {
		operations, ok := document.Paths[path]
		if !ok {
			t.Errorf("OpenAPI missing route %s", path)
			continue
		}
		for _, method := range expected {
			if _, ok := operations[method]; !ok {
				t.Errorf("OpenAPI missing %s %s", method, path)
			}
		}
		actualMethods := 0
		for method := range operations {
			if method != "parameters" {
				actualMethods++
			}
		}
		if actualMethods != len(expected) {
			t.Errorf("OpenAPI route %s has operations %v, want %v", path, operations, expected)
		}
	}
}

func TestNoInternalErrorTextOrSecretOnServiceFailure(t *testing.T) {
	f := newFixture(t)
	_ = f.connections.Create(context.Background(), domain.Connection{ID: "demo-1", Provider: "demo"})
	f.handler.opts.Jobs = &errorJobQueue{err: errors.New("secret=" + testToken)}
	w := request(f, http.MethodPost, "/v1/connections/demo-1/run", `{}`, nil)
	if w.Code != 500 || strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("unsafe service error: %d %s", w.Code, w.Body.String())
	}
}

type errorJobQueue struct{ err error }

func (f *errorJobQueue) Queue(context.Context, string, RunOptions) (string, error) { return "", f.err }

func TestSecretKeyClassification(t *testing.T) {
	for _, key := range []string{"token", "api-token", "accessKey", "passwd", "pwd", "session", "cookie", "bearer", "auth", "authorization", "client_secret"} {
		if !isSecretKey(key) {
			t.Fatalf("%q should be classified as secret", key)
		}
	}
	for _, key := range []string{"theme", "author", "startup", "update-channel", "log-level"} {
		if isSecretKey(key) {
			t.Fatalf("%q should not be classified as secret", key)
		}
	}
}

func TestWorkspaceRoutesRequireAuth(t *testing.T) {
	f := newFixture(t)
	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/merge/plan", `{"project":"brain"}`},
		{http.MethodPost, "/v1/merge/apply", `{"plan_id":"plan-1"}`},
		{http.MethodPost, "/v1/merge/rollback", `{"plan_id":"plan-1"}`},
		{http.MethodPost, "/v1/digest", `{}`},
		{http.MethodPost, "/v1/projects/adopt", `{"apply":false}`},
		{http.MethodPost, "/v1/projects/brain/paths", `{"path":"C:\\work\\brain"}`},
		{http.MethodGet, "/v1/journal", ""},
		{http.MethodGet, "/v1/hooks", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Host = net.JoinHostPort("127.0.0.1", strconvI(testPort))
			if tc.body != "" {
				r.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("unauthenticated status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestMergeRoutesResolveProjectAndMapStalePlan(t *testing.T) {
	f := newFixture(t)
	f.handler.opts.Memory = newFakeMemory()
	f.workspace.plan.Survivor.Title = "leaked " + testToken

	w := request(f, http.MethodPost, "/v1/merge/plan", `{"project":"brain","survivor":"note-1","duplicates":["note-2"]}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("plan=%d %s", w.Code, w.Body.String())
	}
	if f.workspace.lastPlan.ProjectID != "brain" || f.workspace.lastPlan.Survivor != "note-1" ||
		len(f.workspace.lastPlan.Duplicates) != 1 {
		t.Fatalf("plan request = %+v", f.workspace.lastPlan)
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("token leaked through a plan title")
	}
	if got := request(f, http.MethodPost, "/v1/merge/plan", `{"survivor":"note-1"}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("missing project=%d %s", got.Code, got.Body.String())
	}
	if got := request(f, http.MethodGet, "/v1/merge/plan", "", nil); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != "POST" {
		t.Fatalf("plan method=%d Allow=%q", got.Code, got.Header().Get("Allow"))
	}

	f.handler.opts.ProjectAllowlist = []string{"brain"}
	if got := request(f, http.MethodPost, "/v1/merge/apply", `{"plan_id":"plan-1"}`, nil); got.Code != http.StatusOK {
		t.Fatalf("apply=%d %s", got.Code, got.Body.String())
	}
	if f.workspace.lastApply.PlanID != "plan-1" || len(f.workspace.lastApply.AllowedProjects) != 1 {
		t.Fatalf("apply request = %+v", f.workspace.lastApply)
	}
	w = request(f, http.MethodPost, "/v1/merge/rollback", `{"plan_id":"plan-1"}`, nil)
	if w.Code != http.StatusOK || f.workspace.lastRollback.PlanID != "plan-1" {
		t.Fatalf("rollback=%d %s", w.Code, w.Body.String())
	}

	f.workspace.applyErr = codedFakeError{code: "stale_plan", message: "stale_plan: refused " + testToken}
	w = request(f, http.MethodPost, "/v1/merge/apply", `{"plan_id":"plan-1"}`, nil)
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("stale apply=%d %s", w.Code, w.Body.String())
	}
	envelope := decodeMap(t, w.Body.String())
	errorBody, ok := envelope["error"].(map[string]any)
	if !ok || errorBody["code"] != "merge_stale" {
		t.Fatalf("stale envelope=%s", w.Body.String())
	}
	if got := request(f, http.MethodPost, "/v1/merge/apply", `{}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("missing plan_id=%d %s", got.Code, got.Body.String())
	}
}

func TestDigestRouteScopeAndDryRun(t *testing.T) {
	f := newFixture(t)
	f.handler.opts.Memory = newFakeMemory()

	w := request(f, http.MethodPost, "/v1/digest", `{"project":"brain","write":false}`, nil)
	if w.Code != http.StatusOK || f.workspace.lastDigest.ProjectID != "brain" || f.workspace.lastDigest.Write {
		t.Fatalf("digest=%d %s last=%+v", w.Code, w.Body.String(), f.workspace.lastDigest)
	}
	w = request(f, http.MethodPost, "/v1/digest", `{"project":"brain","write":true}`, nil)
	if w.Code != http.StatusOK || !f.workspace.lastDigest.Write {
		t.Fatalf("digest write=%d %s", w.Code, w.Body.String())
	}
	f.handler.opts.ProjectAllowlist = []string{"brain"}
	if got := request(f, http.MethodPost, "/v1/digest", `{}`, nil); got.Code != http.StatusOK || f.workspace.lastDigest.ProjectID != "brain" {
		t.Fatalf("single-project digest=%d %s", got.Code, got.Body.String())
	}
	f.handler.opts.ProjectAllowlist = []string{"brain", "demo"}
	if got := request(f, http.MethodPost, "/v1/digest", `{}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("multi-project digest=%d %s", got.Code, got.Body.String())
	}
	f.workspace.digestErr = errors.New("digest failed with " + testToken)
	if got := request(f, http.MethodPost, "/v1/digest", `{"project":"brain"}`, nil); got.Code != http.StatusInternalServerError ||
		strings.Contains(got.Body.String(), testToken) {
		t.Fatalf("unsafe digest error=%d %s", got.Code, got.Body.String())
	}
}

func TestProjectAdoptAndPathRoutes(t *testing.T) {
	f := newFixture(t)
	f.handler.opts.Memory = newFakeMemory()

	w := request(f, http.MethodPost, "/v1/projects/adopt", `{"apply":true}`, nil)
	if w.Code != http.StatusOK || !f.workspace.lastAdopt {
		t.Fatalf("adopt=%d %s last=%v", w.Code, w.Body.String(), f.workspace.lastAdopt)
	}
	f.handler.opts.ProjectAllowlist = []string{"brain"}
	if got := request(f, http.MethodPost, "/v1/projects/adopt", `{"apply":false}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("scoped adopt=%d %s", got.Code, got.Body.String())
	}
	f.handler.opts.ProjectAllowlist = nil

	f.workspace.project = Project{ID: "brain", Name: "Brain", Paths: []string{"C:/work/brain"}}
	w = request(f, http.MethodPost, "/v1/projects/brain/paths", `{"path":"C:\\work\\brain"}`, nil)
	if w.Code != http.StatusOK || f.workspace.lastPath.ProjectID != "brain" || f.workspace.lastPath.Path != `C:\work\brain` {
		t.Fatalf("add path=%d %s last=%+v", w.Code, w.Body.String(), f.workspace.lastPath)
	}
	if got := request(f, http.MethodPost, "/v1/projects/brain/paths", `{"path":" "}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("empty path=%d %s", got.Code, got.Body.String())
	}
	// The existing project state route keeps its handler.
	if got := request(f, http.MethodGet, "/v1/projects/brain/state", "", nil); got.Code != http.StatusOK {
		t.Fatalf("state route=%d %s", got.Code, got.Body.String())
	}
	if got := request(f, http.MethodGet, "/v1/projects/brain/paths", "", nil); got.Code != http.StatusMethodNotAllowed ||
		got.Header().Get("Allow") != "POST" {
		t.Fatalf("paths method=%d Allow=%q", got.Code, got.Header().Get("Allow"))
	}
}

func TestJournalRouteScopeLimitAndRedaction(t *testing.T) {
	f := newFixture(t)
	f.handler.opts.Memory = newFakeMemory()
	f.workspace.journal = JournalPage{
		Items: []JournalView{{Event: "note", Project: "brain", ID: "note-1", Title: "safe " + testToken}},
		Limit: 5,
	}

	w := request(f, http.MethodGet, "/v1/journal?project=brain&limit=5", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("journal=%d %s", w.Code, w.Body.String())
	}
	if f.workspace.lastJournal.ProjectID != "brain" || f.workspace.lastJournal.Limit != 5 {
		t.Fatalf("journal query = %+v", f.workspace.lastJournal)
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("token leaked through the journal")
	}
	if got := request(f, http.MethodGet, "/v1/journal?limit=0", "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("bad limit=%d %s", got.Code, got.Body.String())
	}
	f.handler.opts.ProjectAllowlist = []string{"brain", "demo"}
	if got := request(f, http.MethodGet, "/v1/journal", "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("unscoped journal=%d %s", got.Code, got.Body.String())
	}
	if got := request(f, http.MethodGet, "/v1/journal?project=other", "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("out-of-scope journal=%d %s", got.Code, got.Body.String())
	}
	f.handler.opts.ProjectAllowlist = []string{"brain"}
	if got := request(f, http.MethodGet, "/v1/journal", "", nil); got.Code != http.StatusOK || f.workspace.lastJournal.ProjectID != "brain" {
		t.Fatalf("single-project journal=%d %s", got.Code, got.Body.String())
	}
}

func TestHooksRouteStatusAndSanitizedFailure(t *testing.T) {
	f := newFixture(t)
	w := request(f, http.MethodGet, "/v1/hooks", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"host":"claude"`) {
		t.Fatalf("hooks=%d %s", w.Code, w.Body.String())
	}
	if got := request(f, http.MethodPost, "/v1/hooks", "", nil); got.Code != http.StatusMethodNotAllowed || got.Header().Get("Allow") != "GET" {
		t.Fatalf("hooks method=%d Allow=%q", got.Code, got.Header().Get("Allow"))
	}
	f.hooks.err = errors.New("hooks failed with " + testToken)
	if got := request(f, http.MethodGet, "/v1/hooks", "", nil); got.Code != http.StatusInternalServerError ||
		strings.Contains(got.Body.String(), testToken) {
		t.Fatalf("unsafe hooks error=%d %s", got.Code, got.Body.String())
	}
}
