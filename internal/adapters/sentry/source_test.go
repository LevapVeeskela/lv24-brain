package sentry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ingestiontest"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	testToken = "sentry-test-token-xyz"
	testOrg   = "acme"
)

type testCredentials struct{ token string }

func (c testCredentials) Get(_ context.Context, connectionID, field string) (string, error) {
	if field != "token" || c.token == "" {
		return "", domain.ErrNotFound
	}
	if connectionID != "sentry-test" {
		return "", domain.ErrNotFound
	}
	return c.token, nil
}

func testConnection(baseURL string) domain.Connection {
	return domain.Connection{
		ID: "sentry-test", Provider: Name, BaseURL: baseURL,
		Projects: []string{"lv24"}, Settings: map[string]string{"org": testOrg},
	}
}

func newTestSource(t *testing.T, conn domain.Connection) *Source {
	t.Helper()
	source, err := NewSource(conn, testCredentials{token: testToken}, nil, func() time.Time {
		return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return source
}

func TestDescriptor(t *testing.T) {
	desc := Descriptor()
	if err := desc.Validate(); err != nil {
		t.Fatalf("Descriptor.Validate: %v", err)
	}
	if desc.Provider != "sentry" || desc.DisplayName != "Sentry" {
		t.Fatalf("descriptor identity = %q/%q", desc.Provider, desc.DisplayName)
	}
	if !desc.Capabilities.Backfill || !desc.Capabilities.Incremental || desc.Capabilities.Deletes {
		t.Fatalf("capabilities = %+v, want Backfill+Incremental without Deletes", desc.Capabilities)
	}
	if len(desc.Capabilities.AuthModes) != 1 || desc.Capabilities.AuthModes[0] != domain.AuthToken {
		t.Fatalf("authModes = %v, want [token]", desc.Capabilities.AuthModes)
	}
	if desc.DefaultPublish != domain.PublishShared {
		t.Fatalf("defaultPublish = %q, want shared", desc.DefaultPublish)
	}
	fields := map[string]domain.ConfigField{}
	for _, field := range desc.ConfigFields {
		fields[field.Key] = field
	}
	token, ok := fields["token"]
	if !ok || !token.Required || !token.Secret {
		t.Fatalf("token field = %+v, want required secret", token)
	}
	org, ok := fields["org"]
	if !ok || !org.Required || org.Secret {
		t.Fatalf("org field = %+v, want required non-secret", org)
	}
	if _, ok := fields["projects"]; !ok {
		t.Fatal("missing optional projects field")
	}
	if _, ok := fields["environment"]; !ok {
		t.Fatal("missing optional environment field")
	}
}

func TestNewSourceBaseURL(t *testing.T) {
	conn := testConnection("")
	source, err := NewSource(conn, testCredentials{token: testToken}, nil, nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if source.baseURL != DefaultBaseURL {
		t.Fatalf("baseURL = %q, want default %q", source.baseURL, DefaultBaseURL)
	}
	if !strings.HasPrefix(source.stream, "sentry-") {
		t.Fatalf("stream = %q, want sentry-<hash>", source.stream)
	}
	conn.BaseURL = "http://example.com"
	if _, err := NewSource(conn, testCredentials{token: testToken}, nil, nil); err == nil {
		t.Fatal("NewSource accepted non-loopback http base URL")
	}
}

func TestCheckRequiresOrg(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","slug":"acme","name":"Acme"}`))
	}))
	defer server.Close()
	conn := testConnection(server.URL)
	if err := newTestSource(t, conn).Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestCheckUnauthorizedLeaksNoToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid token"}`))
	}))
	defer server.Close()
	conn := testConnection(server.URL)
	source, err := NewSource(conn, testCredentials{token: "super-secret-token-abc"}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	err = source.Check(context.Background())
	if err == nil {
		t.Fatal("Check succeeded against a 401 fake")
	}
	if strings.Contains(err.Error(), "super-secret-token-abc") {
		t.Fatalf("Check error leaks the token: %v", err)
	}
}

func TestReadUnauthorizedLeaksNoToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"forbidden"}`))
	}))
	defer server.Close()
	conn := testConnection(server.URL)
	source, err := NewSource(conn, testCredentials{token: "super-secret-token-abc"}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	_, err = source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 5})
	if err == nil {
		t.Fatal("Read succeeded against a 403 fake")
	}
	if strings.Contains(err.Error(), "super-secret-token-abc") {
		t.Fatalf("Read error leaks the token: %v", err)
	}
}

func TestReadIssuesWithEventsAndLinkPaging(t *testing.T) {
	fixture := newPagingFixture()
	server := httptest.NewServer(fixture)
	defer server.Close()
	conn := testConnection(server.URL)
	source, err := NewSource(conn, testCredentials{token: testToken}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 2})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 2 || !batch.More {
		t.Fatalf("items=%d more=%v, want 2 items with More", len(batch.Items), batch.More)
	}
	item := batch.Items[0]
	if item.Kind != domain.ItemIssue {
		t.Fatalf("kind = %q, want issue", item.Kind)
	}
	if item.Ref.ExternalID != "acme/web/1" {
		t.Fatalf("externalID = %q, want acme/web/1", item.Ref.ExternalID)
	}
	if !strings.Contains(item.Body, "ZeroDivisionError") || !strings.Contains(item.Body, "views.py") {
		t.Fatalf("body missing metadata/event content:\n%s", item.Body)
	}
	if item.Author != "api.views in index" {
		t.Fatalf("author = %q", item.Author)
	}
	if item.ProjectID != "lv24" {
		t.Fatalf("ProjectID = %q, want the single configured brain project", item.ProjectID)
	}
	if item.URL == "" || len(item.Labels) == 0 {
		t.Fatalf("item missing URL/labels: %#v", item)
	}
	wantUpdated := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if !item.UpdatedAt.Equal(wantUpdated) {
		t.Fatalf("UpdatedAt = %v, want lastSeen %v", item.UpdatedAt, wantUpdated)
	}
	if len(batch.Checkpoints) != 1 || batch.Checkpoints[0].Stream != source.stream {
		t.Fatalf("checkpoints = %#v, want one cursor for stream %q", batch.Checkpoints, source.stream)
	}
	// Resume from the pending cursor and finish the pass.
	checkpoints := map[string]domain.Checkpoint{source.stream: batch.Checkpoints[0]}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints, Limit: 10})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if len(second.Items) != 3 || second.More {
		t.Fatalf("second batch items=%d more=%v, want remaining 3 and done", len(second.Items), second.More)
	}
	// Deterministic replay from the same checkpoint.
	replay, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints, Limit: 10})
	if err != nil {
		t.Fatalf("replay Read: %v", err)
	}
	if len(replay.Items) != len(second.Items) || !replay.Checkpoints[0].Equal(second.Checkpoints[0]) || replay.More != second.More {
		t.Fatalf("replay diverged from the identical checkpoint")
	}
	for i := range replay.Items {
		if replay.Items[i].ComputeHash() != second.Items[i].ComputeHash() {
			t.Fatalf("replay item %d differs", i)
		}
	}
}

func TestReadAppliesLastSeenBoundAndOverlap(t *testing.T) {
	fixture := &sentryFixture{token: testToken, org: testOrg, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	fixture.addIssueAt("2026-11-01T00:00:00Z")
	fixture.addIssueAt("2026-11-08T00:00:00Z")
	fixture.addIssueAt("2026-11-09T00:00:00Z")
	server := httptest.NewServer(fixture)
	defer server.Close()
	conn := testConnection(server.URL)
	source, err := NewSource(conn, testCredentials{token: testToken}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if len(first.Items) != 3 || first.More {
		t.Fatalf("first batch items=%d more=%v, want 3 done", len(first.Items), first.More)
	}
	// One issue inside the 5m overlap, one older than the overlap.
	fixture.addIssueAt("2026-11-08T23:58:00Z")
	fixture.addIssueAt("2026-11-08T23:00:00Z")
	second, err := source.Read(context.Background(), ports.ReadRequest{
		Connection: conn, Checkpoints: map[string]domain.Checkpoint{source.stream: first.Checkpoints[0]}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	ids := map[string]bool{}
	for _, item := range second.Items {
		ids[item.Ref.ExternalID] = true
	}
	if len(second.Items) != 2 || !ids["acme/web/4"] || !ids["acme/web/3"] {
		t.Fatalf("second batch = %v, want overlap issue 4 and newest issue 3", ids)
	}
	if ids["acme/web/5"] || ids["acme/web/1"] || ids["acme/web/2"] {
		t.Fatalf("second batch leaked items below the overlap bound: %v", ids)
	}
	// The server-side query must carry the lastSeen hint.
	fixture.mu.Lock()
	lastQuery := fixture.queries[len(fixture.queries)-1]
	fixture.mu.Unlock()
	if !strings.Contains(lastQuery.Get("query"), "lastSeen:>") {
		t.Fatalf("issues query = %q, want a lastSeen hint", lastQuery.Encode())
	}
}

func TestReadCapsEventsPerIssue(t *testing.T) {
	fixture := &sentryFixture{token: testToken, org: testOrg, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	fixture.addIssueAt("2026-11-09T00:00:00Z")
	fixture.mu.Lock()
	var events []sentryEvent
	for i := 1; i <= 5; i++ {
		events = append(events, sentryEvent{ID: fmt.Sprintf("ev-%d", i), Message: fmt.Sprintf("boom %d", i)})
	}
	fixture.events["1"] = events
	fixture.mu.Unlock()
	server := httptest.NewServer(fixture)
	defer server.Close()
	conn := testConnection(server.URL)
	source, err := NewSource(conn, testCredentials{token: testToken}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 5})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("items=%d, want 1", len(batch.Items))
	}
	body := batch.Items[0].Body
	for _, want := range []string{"ev-1", "ev-2", "ev-3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing capped event %s:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"ev-4", "ev-5"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("body exceeds the 3-event cap with %s:\n%s", unwanted, body)
		}
	}
}

func TestReadRejectsMalformedCursors(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	conn := testConnection(server.URL)
	source, err := NewSource(conn, testCredentials{token: testToken}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	readWith := func(data string, version int, stream string) error {
		cp := domain.Checkpoint{Stream: stream, Version: version, Data: []byte(data)}
		_, err := source.Read(context.Background(), ports.ReadRequest{
			Connection: conn, Checkpoints: map[string]domain.Checkpoint{stream: cp},
		})
		return err
	}
	cases := map[string]struct {
		data    string
		version int
		stream  string
	}{
		"bad json":       {`{`, checkpointVersion, source.stream},
		"negative off":   {`{"offset":-1}`, checkpointVersion, source.stream},
		"offset too big": {`{"offset":101}`, checkpointVersion, source.stream},
		"bad version":    {`{}`, checkpointVersion + 1, source.stream},
		"bad cursor":     {`{"cursor":"has space"}`, checkpointVersion, source.stream},
		"cursor too big": {`{"cursor":"` + strings.Repeat("a", maxCursorBytes+1) + `"}`, checkpointVersion, source.stream},
	}
	for name, tc := range cases {
		if err := readWith(tc.data, tc.version, source.stream); err == nil {
			t.Fatalf("%s: Read accepted a malformed cursor", name)
		}
	}
	wrong := domain.Checkpoint{Stream: source.stream, Version: checkpointVersion, Data: []byte(`{}`)}
	wrong.Stream = "sentry-deadbeefcafe"
	if _, err := source.Read(context.Background(), ports.ReadRequest{
		Connection: conn, Checkpoints: map[string]domain.Checkpoint{source.stream: wrong},
	}); err == nil {
		t.Fatal("Read accepted a mismatched checkpoint stream")
	}
	_ = cases
}

func TestReadSendsProjectAndEnvironmentFilters(t *testing.T) {
	fixture := &sentryFixture{token: testToken, org: testOrg, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	fixture.addIssueAt("2026-11-09T00:00:00Z")
	server := httptest.NewServer(fixture)
	defer server.Close()
	conn := testConnection(server.URL)
	conn.Settings["projects"] = "web, api"
	conn.Settings["environment"] = "production"
	source, err := NewSource(conn, testCredentials{token: testToken}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 5})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("items=%d, want 1", len(batch.Items))
	}
	fixture.mu.Lock()
	query := fixture.queries[len(fixture.queries)-1]
	fixture.mu.Unlock()
	projects := query["project"]
	if len(projects) != 2 || projects[0] != "api" || projects[1] != "web" {
		t.Fatalf("project params = %v, want [api web]", projects)
	}
	if !strings.Contains(query.Get("query"), "environment:production") {
		t.Fatalf("query = %q, want the environment filter", query.Encode())
	}
	if got := query.Get("sort"); got != "last_seen" {
		t.Fatalf("sort = %q, want last_seen", got)
	}
}

func TestSourceContract(t *testing.T) {
	fixture := &sentryFixture{token: testToken, org: testOrg, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	fixture.addIssueAt("2026-11-01T00:00:00Z")
	fixture.addIssueAt("2026-11-02T00:00:00Z")
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	conn := testConnection(server.URL)
	badConnection := conn
	badConnection.ID = "sentry-bad"
	harness := ingestiontest.Harness{
		Connection:    conn,
		BadConnection: &badConnection,
		PageSize:      5,
		NewSource: func(connection domain.Connection) ports.Source {
			source, err := NewSource(connection, testCredentials{token: testToken}, server.Client(), func() time.Time {
				return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
			})
			if err != nil {
				t.Fatalf("NewSource(%s): %v", connection.ID, err)
			}
			return source
		},
		AddItem:    func() { fixture.addItem() },
		UpdateItem: func() { fixture.updateItem() },
		AddMany: func(n int) {
			for range n {
				fixture.addItem()
			}
		},
	}
	ingestiontest.RunSourceSuite(t, harness)
}

// pagingFixture forces Link pagination with two-item server pages.
func newPagingFixture() *sentryFixture {
	fixture := &sentryFixture{token: testToken, org: testOrg, pageSize: 2, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	for i := 0; i < 5; i++ {
		fixture.addIssueAt(time.Date(2026, 11, 1, i, 0, 0, 0, time.UTC).Format(time.RFC3339))
	}
	return fixture
}

type sentryFixture struct {
	mu       sync.Mutex
	token    string
	org      string
	now      time.Time
	nextID   int
	issues   []sentryIssue
	events   map[string][]sentryEvent
	pageSize int
	queries  []url.Values
}

func (f *sentryFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.token {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"invalid token"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/0/organizations/"+f.org+"/":
		_ = json.NewEncoder(w).Encode(sentryOrg{ID: "1", Slug: f.org, Name: "Test Org"})
	case r.URL.Path == "/api/0/organizations/"+f.org+"/issues/":
		f.serveIssues(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/0/issues/") && strings.HasSuffix(r.URL.Path, "/events/"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/0/issues/"), "/events/")
		f.mu.Lock()
		events := f.events[id]
		f.mu.Unlock()
		if events == nil {
			events = []sentryEvent{}
		}
		_ = json.NewEncoder(w).Encode(events)
	default:
		http.NotFound(w, r)
	}
}

func (f *sentryFixture) serveIssues(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	issues := append([]sentryIssue(nil), f.issues...)
	f.queries = append(f.queries, cloneQuery(r.URL.Query()))
	f.mu.Unlock()
	sort.SliceStable(issues, func(i, j int) bool {
		if !issues[i].LastSeen.Equal(issues[j].LastSeen) {
			return issues[i].LastSeen.Before(issues[j].LastSeen)
		}
		return issues[i].ID < issues[j].ID
	})
	start := 0
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 || offset > len(issues) {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
		start = offset
	}
	size := len(issues) - start
	f.mu.Lock()
	if f.pageSize > 0 && size > f.pageSize {
		size = f.pageSize
	}
	f.mu.Unlock()
	page := issues[start : start+size]
	if start+size < len(issues) {
		next := strconv.Itoa(start + size)
		base := "http://" + r.Host + r.URL.Path
		w.Header().Set("Link", fmt.Sprintf(`<%s?cursor=%s>; rel="previous"; results="false"; cursor="0", <%s?cursor=%s>; rel="next"; results="true"; cursor="%s"`, base, "0", base, next, next))
	}
	_ = json.NewEncoder(w).Encode(page)
}

func cloneQuery(query url.Values) url.Values {
	out := make(url.Values, len(query))
	for key, values := range query {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func (f *sentryFixture) addIssueAt(raw string) {
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.Itoa(f.nextID)
	if at.After(f.now) {
		f.now = at
	}
	issue := sentryIssue{
		ID: id, ShortID: "ACME-" + id, Title: "Issue " + id,
		Culprit: "api.views in index", Permalink: "https://sentry.io/acme/web/issues/" + id,
		Level: "error", Logger: "django", FirstSeen: at, LastSeen: at,
		Project:  sentryProjectRef{ID: "2", Slug: "web", Name: "Web"},
		Metadata: map[string]any{"type": "ZeroDivisionError", "value": "division by zero", "filename": "views.py"},
	}
	f.issues = append(f.issues, issue)
	if f.events == nil {
		f.events = map[string][]sentryEvent{}
	}
	if _, ok := f.events[id]; !ok {
		f.events[id] = []sentryEvent{{
			ID: "event-" + id, Message: "ZeroDivisionError: division by zero",
			DateCreated: at,
			Entries: []sentryEntry{{Type: "exception", Data: sentryEntryData{Values: []sentryException{{
				Type: "ZeroDivisionError", Value: "division by zero",
				Stacktrace: &sentryStacktrace{Frames: []sentryFrame{
					{Filename: "views.py", Function: "index", Lineno: 42},
					{Filename: "urls.py", Function: "dispatch", Lineno: 7},
				}},
			}}}},
			}}}
	}
}

func (f *sentryFixture) addItem() {
	f.mu.Lock()
	next := f.now.Add(time.Second).UTC().Format(time.RFC3339)
	f.mu.Unlock()
	f.addIssueAt(next)
}

func (f *sentryFixture) updateItem() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.issues) == 0 {
		return
	}
	f.now = f.now.Add(time.Minute)
	f.issues[0].Title += " updated"
	f.issues[0].Metadata["value"] = "division by zero updated"
	f.issues[0].LastSeen = f.now
}

var _ ports.CredentialProvider = testCredentials{}
