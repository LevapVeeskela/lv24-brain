package jira

import (
	"context"
	"encoding/base64"
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
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ingestiontest"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const testToken = "jira-secret-token"

type testCredentials struct{ token string }

func (c testCredentials) Get(context.Context, string, string) (string, error) { return c.token, nil }

type testIssue struct {
	Key, Project, Summary, Description, Status, Type, Reporter, Assignee string
	Labels                                                               []string
	Created, Updated                                                     time.Time
}

type testJira struct {
	mu           sync.Mutex
	deployment   string
	issues       []testIssue
	comments     map[string][]map[string]any
	status       int
	queries      []url.Values
	searchPaths  []string
	commentKeys  []string
	commentCalls int
	searchCalls  int
}

func newTestJira(deployment string, issues []testIssue) *testJira {
	return &testJira{deployment: deployment, issues: issues, comments: map[string][]map[string]any{}}
}

func (f *testJira) handler(w http.ResponseWriter, r *http.Request) {
	if f.status != 0 {
		w.WriteHeader(f.status)
		_, _ = fmt.Fprintf(w, `{"error":"request rejected: %s"}`, testToken)
		return
	}
	wantAuth := "Bearer " + testToken
	if f.deployment == "cloud" {
		wantAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte("user@example.test:"+testToken))
	}
	if got := r.Header.Get("Authorization"); got != wantAuth {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/myself") {
		writeJSON(w, map[string]any{"accountId": "account-1", "name": "jira-user"})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/comment") {
		key := r.URL.Path[strings.LastIndex(r.URL.Path, "/issue/")+len("/issue/"):]
		key = strings.TrimSuffix(key, "/comment")
		f.mu.Lock()
		f.commentCalls++
		f.commentKeys = append(f.commentKeys, key)
		comments := append([]map[string]any(nil), f.comments[key]...)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"comments": comments, "total": len(comments), "startAt": 0})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/search/jql") || strings.HasSuffix(r.URL.Path, "/search") {
		f.search(r, w)
		return
	}
	http.NotFound(w, r)
}

func (f *testJira) search(r *http.Request, w http.ResponseWriter) {
	f.mu.Lock()
	f.searchCalls++
	f.queries = append(f.queries, r.URL.Query())
	f.searchPaths = append(f.searchPaths, r.URL.Path)
	f.mu.Unlock()
	issues := append([]testIssue(nil), f.issues...)
	sort.SliceStable(issues, func(i, j int) bool { return issues[i].Updated.Before(issues[j].Updated) })
	if raw := boundFromJQL(r.URL.Query().Get("jql")); !raw.IsZero() {
		kept := issues[:0]
		for _, value := range issues {
			if !value.Updated.Before(raw) {
				kept = append(kept, value)
			}
		}
		issues = kept
	}
	start := 0
	if f.deployment == "cloud" {
		if token := r.URL.Query().Get("nextPageToken"); strings.HasPrefix(token, "page-") {
			start, _ = strconv.Atoi(strings.TrimPrefix(token, "page-"))
		}
	} else {
		start, _ = strconv.Atoi(r.URL.Query().Get("startAt"))
	}
	end := start + pageSize
	if end > len(issues) {
		end = len(issues)
	}
	if start > len(issues) {
		start = len(issues)
	}
	values := make([]map[string]any, 0, end-start)
	for _, value := range issues[start:end] {
		values = append(values, issueJSON(value, f.deployment == "cloud"))
	}
	if f.deployment == "cloud" {
		result := map[string]any{"issues": values, "isLast": end >= len(issues)}
		if end < len(issues) {
			result["nextPageToken"] = "page-" + strconv.Itoa(end)
		}
		writeJSON(w, result)
		return
	}
	writeJSON(w, map[string]any{"issues": values, "startAt": start, "maxResults": pageSize, "total": len(issues)})
}

func issueJSON(value testIssue, cloud bool) map[string]any {
	description := any(value.Description)
	if cloud {
		description = map[string]any{"type": "doc", "version": 1, "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": value.Description}}}}}
	}
	return map[string]any{
		"key": value.Key,
		"fields": map[string]any{
			"summary": value.Summary, "description": description, "labels": value.Labels,
			"created": value.Created.Format(time.RFC3339), "updated": value.Updated.Format(time.RFC3339),
			"reporter": map[string]any{"displayName": value.Reporter}, "assignee": map[string]any{"displayName": value.Assignee},
			"status": map[string]any{"name": value.Status}, "issuetype": map[string]any{"name": value.Type},
			"project": map[string]any{"key": value.Project, "name": "Project " + value.Project},
		},
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func boundFromJQL(query string) time.Time {
	index := strings.Index(query, `updated >= "`)
	if index < 0 {
		return time.Time{}
	}
	start := index + len(`updated >= "`)
	end := strings.Index(query[start:], `"`)
	if end < 0 {
		return time.Time{}
	}
	at, _ := time.ParseInLocation("2006-01-02 15:04", query[start:start+end], time.UTC)
	return at
}

func testConnection(serverURL, deployment string) domain.Connection {
	settings := map[string]string{"deployment": deployment}
	if deployment == "cloud" {
		settings["email"] = "user@example.test"
	}
	return domain.Connection{ID: "jira-test", Provider: Name, BaseURL: serverURL, Settings: settings}
}

func buildSource(t *testing.T, conn domain.Connection, client *http.Client) *Source {
	t.Helper()
	source, err := NewSource(conn, testCredentials{token: testToken}, client, func() time.Time {
		return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func issueAt(key string, at time.Time) testIssue {
	return testIssue{Key: key, Project: "ENG", Summary: "Summary " + key, Description: "description " + key,
		Status: "Open", Type: "Task", Reporter: "Reporter", Assignee: "Assignee", Labels: []string{"backend"}, Created: at.Add(-time.Hour), Updated: at}
}

func TestDescriptorAndBaseURL(t *testing.T) {
	descriptor := Descriptor()
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Provider != "jira" || descriptor.DisplayName != "Jira" || descriptor.DefaultPublish != domain.PublishShared || !descriptor.Capabilities.Backfill || !descriptor.Capabilities.Incremental || descriptor.Capabilities.Deletes || !descriptor.SupportsBackend(domain.BackendNative) {
		t.Fatalf("unexpected descriptor: %#v", descriptor)
	}
	tokenField, ok := descriptor.Field("token")
	if !ok || !tokenField.Required || !tokenField.Secret {
		t.Fatalf("token setting is not required/secret: %#v", tokenField)
	}
	for _, key := range []string{"email", "deployment", "projects", "jql"} {
		field, ok := descriptor.Field(key)
		if !ok || field.Required || field.Secret {
			t.Errorf("optional non-secret setting %q is incorrect: %#v", key, field)
		}
	}
	for _, raw := range []string{"https://user:pass@example.test", "https://example.test?token=x", "https://example.test?", "https://example.test/#fragment", "https://example.test/#", "http://example.test"} {
		if _, err := sourceBaseURL(raw); err == nil {
			t.Errorf("sourceBaseURL(%q) succeeded", raw)
		}
	}
	if got, err := sourceBaseURL("http://127.0.0.1:8080/jira/"); err != nil || got != "http://127.0.0.1:8080/jira" {
		t.Fatalf("loopback URL = %q, %v", got, err)
	}
}

func TestCustomJQLIsCombinedWithProjectAndStableOrder(t *testing.T) {
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	fake := newTestJira("cloud", []testIssue{issueAt("ENG-1", at)})
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	conn := testConnection(server.URL, "cloud")
	conn.Settings["projects"] = "eng,OPS"
	conn.Settings["jql"] = "assignee = currentUser() ORDER BY created DESC"
	source := buildSource(t, conn, server.Client())
	if _, err := source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Since: &at}); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	jql := fake.queries[0].Get("jql")
	if !strings.Contains(jql, `project in ("ENG", "OPS")`) || !strings.Contains(jql, "(assignee = currentUser())") || !strings.Contains(jql, `updated >= "2026-11-09 10:00"`) || !strings.HasSuffix(jql, "ORDER BY updated ASC") || strings.Contains(jql, "created DESC") {
		t.Fatalf("custom JQL was not composed read-only with stable order: %q", jql)
	}
}

func TestCloudAuthPaginationAndItems(t *testing.T) {
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	issues := make([]testIssue, 52)
	for i := range issues {
		issues[i] = issueAt(fmt.Sprintf("ENG-%d", i+1), at.Add(time.Duration(i)*time.Minute))
	}
	fake := newTestJira("cloud", issues)
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	source := buildSource(t, testConnection(server.URL, "cloud"), server.Client())
	if err := source.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if batch.More || len(batch.Items) != len(issues) {
		t.Fatalf("got %d issues, More=%v", len(batch.Items), batch.More)
	}
	if batch.Items[0].Ref.ExternalID != "ENG/ENG-1" || batch.Items[0].Kind != domain.ItemIssue || batch.Items[0].Author != "Reporter" || batch.Items[0].URL != server.URL+"/browse/ENG-1" {
		t.Fatalf("unexpected normalized item: %#v", batch.Items[0])
	}
	if !strings.Contains(batch.Items[0].Body, "**Status:** Open") || !strings.Contains(batch.Items[0].Body, "**Issue type:** Task") || !strings.Contains(batch.Items[0].Body, "description ENG-1") {
		t.Fatalf("missing Jira fields in body: %q", batch.Items[0].Body)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.searchCalls != 2 || fake.searchPaths[0] != "/rest/api/3/search/jql" || fake.searchPaths[1] != "/rest/api/3/search/jql" {
		t.Fatalf("cloud search calls/paths: %d %v", fake.searchCalls, fake.searchPaths)
	}
	if fake.commentCalls != len(issues) {
		t.Fatalf("comments fetched %d times for %d returned issues", fake.commentCalls, len(issues))
	}
	if fake.queries[1].Get("nextPageToken") != "page-50" {
		t.Fatalf("second cloud page token = %q", fake.queries[1].Get("nextPageToken"))
	}
	for _, query := range fake.queries {
		jql := query.Get("jql")
		if !strings.Contains(jql, "project is not EMPTY") || !strings.HasSuffix(jql, "ORDER BY updated ASC") || strings.Contains(jql, "updated >=") {
			t.Errorf("unexpected initial JQL: %q", jql)
		}
	}
}

func TestDataCenterBearerStartAtAndBoundedComments(t *testing.T) {
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	issues := make([]testIssue, 52)
	for i := range issues {
		issues[i] = issueAt(fmt.Sprintf("ENG-%d", i+1), at.Add(time.Duration(i)*time.Minute))
	}
	fake := newTestJira("datacenter", issues)
	for i := 0; i < 7; i++ {
		fake.comments["ENG-1"] = append(fake.comments["ENG-1"], map[string]any{
			"id":     strconv.Itoa(i),
			"author": map[string]any{"displayName": "Commenter"}, "created": at.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			"body": fmt.Sprintf("comment-%d %s", i, strings.Repeat("界", 17000)),
		})
	}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	conn := testConnection(server.URL+"/jira", "datacenter")
	source := buildSource(t, conn, server.Client())
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Limit: 100, Since: &at})
	if err != nil {
		t.Fatal(err)
	}
	if batch.More || len(batch.Items) != len(issues) {
		t.Fatalf("got %d issues, More=%v", len(batch.Items), batch.More)
	}
	if !strings.Contains(batch.Items[0].Body, "**Project:** Project ENG") {
		t.Fatalf("plain-text Data Center issue fields not retained: %q", batch.Items[0].Body[:min(100, len(batch.Items[0].Body))])
	}
	if len(batch.Items[0].Body) > maxIssueBodyBytes || !utf8.ValidString(batch.Items[0].Body) {
		t.Fatalf("comment body cap/UTF-8 failed: len=%d valid=%v", len(batch.Items[0].Body), utf8.ValidString(batch.Items[0].Body))
	}
	if strings.Count(batch.Items[0].Body, "Commenter") != maxCommentsPerIssue {
		t.Fatalf("expected at most %d newest comments: got %d", maxCommentsPerIssue, strings.Count(batch.Items[0].Body, "Commenter"))
	}
	if !strings.Contains(batch.Items[0].Body, "comment-6") || strings.Contains(batch.Items[0].Body, "comment-1") {
		t.Fatalf("comments were not the newest five: %q", batch.Items[0].Body[:min(400, len(batch.Items[0].Body))])
	}
	for _, rendered := range strings.Split(batch.Items[0].Body, "\n- **")[1:] {
		if len(rendered) > maxCommentBytes {
			t.Fatalf("rendered comment exceeded cap: %d bytes", len(rendered))
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.searchCalls != 2 || fake.queries[0].Get("startAt") != "0" || fake.queries[1].Get("startAt") != "50" {
		t.Fatalf("Data Center startAt pagination: queries=%v", fake.queries)
	}
	if fake.searchPaths[0] != "/jira/rest/api/2/search" {
		t.Fatalf("DC endpoint path = %q", fake.searchPaths[0])
	}
	if !strings.Contains(fake.queries[0].Get("jql"), `updated >= "2026-11-09 10:00"`) || !strings.HasSuffix(fake.queries[0].Get("jql"), "ORDER BY updated ASC") {
		t.Fatalf("DC JQL bound/order incorrect: %q", fake.queries[0].Get("jql"))
	}
}

func TestSinceInclusiveOverlapAndCursorNarrowing(t *testing.T) {
	at := time.Date(2026, 11, 9, 10, 15, 0, 0, time.UTC)
	fake := newTestJira("cloud", []testIssue{
		issueAt("ENG-1", at.Add(-time.Second)), issueAt("ENG-2", at), issueAt("ENG-3", at.Add(time.Minute)),
	})
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	source := buildSource(t, testConnection(server.URL, "cloud"), server.Client())
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Since: &at})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Items) != 2 || batch.Items[0].Ref.ExternalID != "ENG/ENG-2" {
		t.Fatalf("inclusive since returned %#v", batch.Items)
	}
	fake.mu.Lock()
	if !strings.Contains(fake.queries[0].Get("jql"), `updated >= "2026-11-09 10:15"`) {
		t.Fatalf("JQL omitted inclusive bound: %q", fake.queries[0].Get("jql"))
	}
	fake.mu.Unlock()

	watermark := at.Add(10 * time.Minute)
	encoded, _ := json.Marshal(cursor{Watermark: watermark, ScanSince: watermark.Add(-watermarkOverlap), NextPage: "opaque-page", Offset: 1, Pages: 2})
	checkpoint := domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: encoded}
	newSince := watermark.Add(-time.Minute)
	narrowed, err := source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Since: &newSince, Checkpoints: map[string]domain.Checkpoint{checkpointStream: checkpoint}})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed.Items) != 0 {
		t.Fatalf("narrowed future cursor returned items: %#v", narrowed.Items)
	}
	fake.mu.Lock()
	last := fake.queries[len(fake.queries)-1]
	if last.Get("nextPageToken") != "" || !strings.Contains(last.Get("jql"), `updated >= "2026-11-09 10:24"`) {
		t.Fatalf("existing cursor was not narrowed/restarted: %v", last)
	}
	fake.mu.Unlock()

	completed, _ := json.Marshal(cursor{Watermark: watermark, Completed: true})
	_, err = source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Checkpoints: map[string]domain.Checkpoint{
		checkpointStream: {Stream: checkpointStream, Version: checkpointVersion, Data: completed},
	}})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !strings.Contains(fake.queries[len(fake.queries)-1].Get("jql"), `updated >= "2026-11-09 10:20"`) {
		t.Fatalf("watermark overlap missing: %q", fake.queries[len(fake.queries)-1].Get("jql"))
	}
}

func TestWatermarkAdvancesOnlyAfterWholePageIsReturned(t *testing.T) {
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	fake := newTestJira("cloud", []testIssue{issueAt("ENG-1", at), issueAt("ENG-2", at.Add(time.Minute))})
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	source := buildSource(t, testConnection(server.URL, "cloud"), server.Client())
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	var firstCursor cursor
	if err := json.Unmarshal(first.Checkpoints[0].Data, &firstCursor); err != nil {
		t.Fatal(err)
	}
	if !first.More || !firstCursor.Watermark.IsZero() || firstCursor.Offset != 1 {
		t.Fatalf("partial page advanced watermark or cursor incorrectly: More=%v cursor=%#v", first.More, firstCursor)
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Limit: 1, Checkpoints: map[string]domain.Checkpoint{checkpointStream: first.Checkpoints[0]}})
	if err != nil {
		t.Fatal(err)
	}
	var secondCursor cursor
	if err := json.Unmarshal(second.Checkpoints[0].Data, &secondCursor); err != nil {
		t.Fatal(err)
	}
	if second.More || !secondCursor.Completed || !secondCursor.Watermark.Equal(at.Add(time.Minute)) {
		t.Fatalf("complete page did not finish/advance watermark: More=%v cursor=%#v", second.More, secondCursor)
	}
}

func TestMalformedCursorAndHTTPFailuresDoNotLeakToken(t *testing.T) {
	serverFake := newTestJira("cloud", []testIssue{issueAt("ENG-1", time.Now().UTC())})
	server := httptest.NewServer(http.HandlerFunc(serverFake.handler))
	defer server.Close()
	source := buildSource(t, testConnection(server.URL, "cloud"), server.Client())
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "bad json", data: []byte("{")},
		{name: "invalid offset", data: []byte(`{"offset":51}`)},
		{name: "nonmonotonic token", data: []byte(`{"nextPageToken":"same","previousPageToken":"same"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Checkpoints: map[string]domain.Checkpoint{
				checkpointStream: {Stream: checkpointStream, Version: checkpointVersion, Data: test.data},
			}})
			if err == nil || strings.Contains(err.Error(), testToken) {
				t.Fatalf("malformed cursor error = %v", err)
			}
		})
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			serverFake.status = status
			err := source.Check(context.Background())
			serverFake.status = 0
			if err == nil || strings.Contains(err.Error(), testToken) {
				t.Fatalf("Check status %d error = %v", status, err)
			}
			serverFake.status = status
			_, err = source.Read(context.Background(), ports.ReadRequest{Connection: source.connection})
			serverFake.status = 0
			if err == nil || strings.Contains(err.Error(), testToken) {
				t.Fatalf("Read status %d error = %v", status, err)
			}
		})
	}
}

func TestDeterministicReplayAndCommentsOnlyForReturnedItems(t *testing.T) {
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	fake := newTestJira("cloud", []testIssue{issueAt("ENG-1", at), issueAt("ENG-2", at.Add(time.Minute))})
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	source := buildSource(t, testConnection(server.URL, "cloud"), server.Client())
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: source.connection, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.Items[0].ComputeHash() != second.Items[0].ComputeHash() || !first.Checkpoints[0].Equal(second.Checkpoints[0]) || first.More != second.More {
		t.Fatal("replay from identical empty cursor was not deterministic")
	}
	if fake.commentCalls != 2 || strings.Join(fake.commentKeys, ",") != "ENG-1,ENG-1" {
		t.Fatalf("comments fetched for non-returned items: %v", fake.commentKeys)
	}
}

func TestSourceContract(t *testing.T) {
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	initial := []testIssue{issueAt("ENG-1", at), issueAt("ENG-2", at.Add(time.Minute)), issueAt("ENG-3", at.Add(2*time.Minute))}
	fake := newTestJira("cloud", initial)
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	conn := testConnection(server.URL, "cloud")
	bad := conn
	bad.Settings = map[string]string{"deployment": "cloud", "email": "bad@example.test"}
	h := ingestiontest.Harness{
		Connection: conn,
		PageSize:   4,
		NewSource: func(connection domain.Connection) ports.Source {
			return buildSource(t, connection, server.Client())
		},
		AddMany: func(n int) {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			for i := 0; i < n; i++ {
				id := len(fake.issues) + 1
				fake.issues = append(fake.issues, issueAt(fmt.Sprintf("ENG-%d", id), at.Add(time.Duration(id)*time.Minute)))
			}
		},
		UpdateItem: func() {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			fake.issues[0].Summary += " updated"
			fake.issues[0].Updated = at.Add(time.Hour)
		},
		BadConnection: &bad,
	}
	// The fake accepts only the expected Cloud email, so the harness's bad
	// connection variant fails Check through its different Basic credentials.
	ingestiontest.RunSourceSuite(t, h)
}
