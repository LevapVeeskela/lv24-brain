package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

const sourceToken = "linear-test-token"

type sourceCredentials struct{ token string }

func (c sourceCredentials) Get(_ context.Context, connectionID, field string) (string, error) {
	if connectionID != "ln-test" || field != "token" || c.token == "" {
		return "", domain.ErrNotFound
	}
	return c.token, nil
}

// linearFake is a stateful GraphQL fake: issues stay sorted by updatedAt,
// the updatedAt gte filter, the team key filter and Relay after/first
// pagination are all honored.
type linearFake struct {
	mu       sync.Mutex
	issues   []gqlIssue
	now      time.Time
	next     int
	token    string
	received []map[string]any
	queries  []string

	viewerBroken  bool
	failStatus    int
	graphQLErrors bool
}

func (f *linearFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != f.token && got != "Bearer "+f.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var payload struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.received = append(f.received, payload.Variables)
	f.queries = append(f.queries, payload.Query)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(payload.Query, "viewer") {
		if f.viewerBroken {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "u1"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"viewer": map[string]any{"id": "u1", "name": "Ada"}}})
		return
	}
	if f.failStatus != 0 {
		http.Error(w, http.StatusText(f.failStatus), f.failStatus)
		return
	}
	if f.graphQLErrors {
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": "boom: " + f.token}}})
		return
	}
	f.mu.Lock()
	issues := append([]gqlIssue(nil), f.issues...)
	f.mu.Unlock()
	filtered := applyFakeFilter(issues, payload.Variables)
	first := fakeFirst(payload.Variables)
	start := fakeStart(filtered, payload.Variables)
	end := min(start+first, len(filtered))
	page := filtered[start:end]
	var endCursor *string
	if len(page) > 0 {
		cursor := page[len(page)-1].ID
		endCursor = &cursor
	}
	_ = json.NewEncoder(w).Encode(gqlResponse{Data: &gqlData{Issues: &gqlIssueConnection{
		Nodes:    page,
		PageInfo: gqlPageInfo{HasNextPage: end < len(filtered), EndCursor: endCursor},
	}}})
}

func applyFakeFilter(issues []gqlIssue, variables map[string]any) []gqlIssue {
	var gte time.Time
	var teams map[string]bool
	if filter, ok := variables["filter"].(map[string]any); ok {
		if updated, ok := filter["updatedAt"].(map[string]any); ok {
			if raw, ok := updated["gte"].(string); ok {
				gte, _ = time.Parse(time.RFC3339Nano, raw)
			}
		}
		if team, ok := filter["team"].(map[string]any); ok {
			if key, ok := team["key"].(map[string]any); ok {
				if in, ok := key["in"].([]any); ok {
					teams = make(map[string]bool, len(in))
					for _, entry := range in {
						if name, ok := entry.(string); ok {
							teams[name] = true
						}
					}
				}
			}
		}
	}
	kept := issues[:0:0]
	for _, issue := range issues {
		if !gte.IsZero() && issue.UpdatedAt.Before(gte) {
			continue
		}
		if teams != nil {
			key := ""
			if issue.Team != nil {
				key = issue.Team.Key
			}
			if !teams[key] {
				continue
			}
		}
		kept = append(kept, issue)
	}
	return kept
}

func fakeFirst(variables map[string]any) int {
	first := pageMaxFirst
	if raw, ok := variables["first"].(float64); ok && raw >= 1 {
		first = int(raw)
	}
	return first
}

func fakeStart(issues []gqlIssue, variables map[string]any) int {
	after, _ := variables["after"].(string)
	if after == "" {
		return 0
	}
	for i, issue := range issues {
		if issue.ID == after {
			return i + 1
		}
	}
	return len(issues)
}

func (f *linearFake) addIssue() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addIssueLocked()
}

func (f *linearFake) addIssueLocked() {
	f.next++
	f.now = f.now.Add(time.Second)
	id := strings.Repeat("0", 32-len(strconv.Itoa(f.next))) + strconv.Itoa(f.next)
	team := "ENG"
	if f.next%2 == 0 {
		team = "OPS"
	}
	f.issues = append(f.issues, gqlIssue{
		ID:          "issue-" + id,
		Identifier:  team + "-" + strconv.Itoa(f.next),
		Title:       "Issue " + strconv.Itoa(f.next),
		Description: "body " + strconv.Itoa(f.next),
		URL:         "https://linear.test/issue/" + strconv.Itoa(f.next),
		CreatedAt:   f.now,
		UpdatedAt:   f.now,
		State:       &gqlName{Name: "Todo"},
		Team:        &gqlTeam{Key: team},
		Labels:      &gqlLabelConnection{Nodes: []gqlName{{Name: "bug"}}},
		Assignee:    &gqlName{Name: "Ada"},
	})
}

func (f *linearFake) updateIssue() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.issues) == 0 {
		f.addIssueLocked()
	}
	f.now = f.now.Add(time.Minute)
	f.issues[0].Description += " updated"
	f.issues[0].UpdatedAt = f.now
}

func testConn(serverURL string, settings map[string]string) domain.Connection {
	if settings == nil {
		settings = map[string]string{}
	}
	return domain.Connection{ID: "ln-test", Provider: Name, BaseURL: serverURL, Projects: []string{"lv24"}, Settings: settings}
}

func newTestSource(t *testing.T, conn domain.Connection, token string, server *httptest.Server) *Source {
	t.Helper()
	source, err := NewSource(conn, sourceCredentials{token: token}, server.Client(), func() time.Time {
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
	if desc.Provider != Name || desc.DisplayName != "Linear" {
		t.Fatalf("descriptor = %+v, want provider linear / Linear", desc)
	}
	if !desc.Capabilities.Backfill || !desc.Capabilities.Incremental || desc.Capabilities.Deletes {
		t.Fatalf("capabilities = %+v, want backfill+incremental without deletes", desc.Capabilities)
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
		t.Fatalf("configFields = %+v, want a required secret token", desc.ConfigFields)
	}
	if _, ok := fields["teams"]; !ok {
		t.Fatalf("configFields = %+v, want optional teams", desc.ConfigFields)
	}
	scheme, ok := fields["authscheme"]
	if !ok || scheme.Secret || scheme.Required {
		t.Fatalf("configFields = %+v, want optional non-secret authscheme", desc.ConfigFields)
	}
	if len(desc.Streams) != 1 || desc.Streams[0] != checkpointStream {
		t.Fatalf("streams = %v, want [%q]", desc.Streams, checkpointStream)
	}
}

func TestAuthSchemeHeader(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]string
		want     string
	}{
		{name: "api key default", settings: nil, want: sourceToken},
		{name: "api key explicit", settings: map[string]string{"authscheme": "api_key"}, want: sourceToken},
		{name: "oauth", settings: map[string]string{"authscheme": "oauth"}, want: "Bearer " + sourceToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"u1","name":"Ada"}}}`))
			}))
			defer server.Close()
			source := newTestSource(t, testConn(server.URL, tc.settings), sourceToken, server)
			if err := source.Check(context.Background()); err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Authorization = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewSourceRejectsBadAuthSchemeAndBaseURL(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	for _, tc := range []struct {
		name string
		conn domain.Connection
	}{
		{name: "authscheme", conn: testConn(server.URL, map[string]string{"authscheme": "basic"})},
		{name: "userinfo", conn: domain.Connection{ID: "ln-test", Provider: Name, BaseURL: "https://user@example.com/graphql"}},
		{name: "query", conn: domain.Connection{ID: "ln-test", Provider: Name, BaseURL: server.URL + "?token=x"}},
		{name: "fragment", conn: domain.Connection{ID: "ln-test", Provider: Name, BaseURL: server.URL + "#frag"}},
		{name: "plain http", conn: domain.Connection{ID: "ln-test", Provider: Name, BaseURL: "http://example.com/graphql"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSource(tc.conn, sourceCredentials{token: sourceToken}, server.Client(), nil); err == nil {
				t.Fatal("NewSource accepted an invalid connection")
			}
		})
	}
}

func TestCheckRequiresViewerIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"u1"}}}`))
	}))
	defer server.Close()
	source := newTestSource(t, testConn(server.URL, nil), sourceToken, server)
	if err := source.Check(context.Background()); err == nil {
		t.Fatal("Check accepted a viewer without a name")
	}
}

func TestReadFailsClosedWithoutTokenLeak(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "denied", status)
			}))
			defer server.Close()
			source := newTestSource(t, testConn(server.URL, nil), sourceToken, server)
			_, err := source.Read(context.Background(), ports.ReadRequest{Connection: testConn(server.URL, nil)})
			if err == nil {
				t.Fatalf("Read with status %d succeeded", status)
			}
			if strings.Contains(err.Error(), sourceToken) {
				t.Fatalf("error leaks the token: %v", err)
			}
		})
	}
}

func TestReadFailsClosedOnGraphQLErrors(t *testing.T) {
	fake := &linearFake{token: sourceToken, graphQLErrors: true}
	server := httptest.NewServer(fake)
	defer server.Close()
	source := newTestSource(t, testConn(server.URL, nil), sourceToken, server)
	_, err := source.Read(context.Background(), ports.ReadRequest{Connection: testConn(server.URL, nil)})
	if err == nil {
		t.Fatal("Read accepted a GraphQL error payload")
	}
	if strings.Contains(err.Error(), sourceToken) || strings.Contains(err.Error(), "boom") {
		t.Fatalf("error echoes upstream body or token: %v", err)
	}
}

func TestReadPaginationTeamFilterAndBounds(t *testing.T) {
	fake := &linearFake{token: sourceToken, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	for range 4 {
		fake.addIssue()
	}
	server := httptest.NewServer(fake)
	defer server.Close()

	conn := testConn(server.URL, map[string]string{"teams": "ENG"})
	source := newTestSource(t, conn, sourceToken, server)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 1})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 || !batch.More {
		t.Fatalf("items=%d more=%v, want one item and more", len(batch.Items), batch.More)
	}
	item := batch.Items[0]
	if item.Kind != domain.ItemIssue || !strings.HasPrefix(item.Ref.ExternalID, "ENG-") {
		t.Fatalf("item = %+v, want a Linear ENG issue", item)
	}
	if item.Author != "Ada" || len(item.Labels) != 1 || item.ProjectID != "lv24" {
		t.Fatalf("item = %+v, want author/labels/project mapping", item)
	}
	if len(batch.Checkpoints) != 1 || batch.Checkpoints[0].Stream != checkpointStream {
		t.Fatalf("checkpoints = %+v, want the linear stream", batch.Checkpoints)
	}
	// The team filter must reach the provider, first must stay within the
	// Relay bound, and the query must ask for updatedAt ordering.
	fake.mu.Lock()
	last := fake.received[len(fake.received)-1]
	fake.mu.Unlock()
	filter, _ := last["filter"].(map[string]any)
	team, _ := filter["team"].(map[string]any)
	key, _ := team["key"].(map[string]any)
	in, _ := key["in"].([]any)
	if len(in) != 1 || in[0] != "ENG" {
		t.Fatalf("team filter = %v, want [ENG]", in)
	}
	if first, _ := last["first"].(float64); first < 1 || first > pageMaxFirst {
		t.Fatalf("first = %v, want within (0, 100]", first)
	}
	if order, _ := last["orderBy"].(string); order != "updatedAt" {
		t.Fatalf("orderBy = %v, want updatedAt", order)
	}

	checkpoints := map[string]domain.Checkpoint{checkpointStream: batch.Checkpoints[0]}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints, Limit: 10})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if len(second.Items) != 1 || second.More {
		t.Fatalf("second batch items=%d more=%v, want the remaining ENG issue and completion", len(second.Items), second.More)
	}
	seen := map[string]bool{item.Ref.ExternalID: true, second.Items[0].Ref.ExternalID: true}
	if len(seen) != 2 {
		t.Fatalf("duplicate external IDs across pages: %+v", batch.Items)
	}
}

func TestReadSinceNarrowsEvenWithCursor(t *testing.T) {
	fake := &linearFake{token: sourceToken, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	fake.addIssue()
	fake.addIssue()
	server := httptest.NewServer(fake)
	defer server.Close()
	conn := testConn(server.URL, nil)
	source := newTestSource(t, conn, sourceToken, server)
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if len(first.Items) != 2 {
		t.Fatalf("items=%d, want 2", len(first.Items))
	}
	checkpoints := map[string]domain.Checkpoint{checkpointStream: first.Checkpoints[0]}
	future := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	filtered, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints, Since: &future, Limit: 10})
	if err != nil {
		t.Fatalf("filtered Read: %v", err)
	}
	if len(filtered.Items) != 0 || filtered.More {
		t.Fatalf("filtered batch items=%d more=%v, want empty and complete", len(filtered.Items), filtered.More)
	}
}

func TestReadCommentsBoundedAndUTF8(t *testing.T) {
	fake := &linearFake{token: sourceToken, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	fake.addIssue()
	fake.mu.Lock()
	comments := []gqlComment{{ID: "c-empty", Body: "   "}}
	for i := 1; i <= 6; i++ {
		comments = append(comments, gqlComment{
			ID:        "c-" + strconv.Itoa(i),
			Body:      "comment " + strconv.Itoa(i) + " " + strings.Repeat("я", 20<<10),
			CreatedAt: fake.now.Add(time.Duration(i) * time.Second),
			UpdatedAt: fake.now.Add(time.Duration(i) * time.Second),
			User:      &gqlName{Name: "Bob"},
		})
	}
	for i := range fake.issues {
		fake.issues[i].Comments = &gqlCommentConnection{Nodes: comments}
	}
	fake.mu.Unlock()
	server := httptest.NewServer(fake)
	defer server.Close()
	conn := testConn(server.URL, nil)
	source := newTestSource(t, conn, sourceToken, server)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 1})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	body := batch.Items[0].Body
	fake.mu.Lock()
	lastQuery := fake.queries[len(fake.queries)-1]
	fake.mu.Unlock()
	if !strings.Contains(lastQuery, "comments(last: 5") {
		t.Fatalf("query does not request the five newest comments: %s", lastQuery)
	}
	if strings.Contains(body, "c-empty") || strings.Contains(body, "comment 1 ") {
		t.Fatalf("body keeps the empty or the oldest comment: %q", body[:minInt(200, len(body))])
	}
	for _, want := range []string{"comment 2 ", "comment 6 "} {
		if !strings.Contains(body, want) {
			t.Fatalf("body misses %q; want the 5 newest comments", want)
		}
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8")
	}
	if len(body) > maxSourceBodyBytes {
		t.Fatalf("body is %d bytes, want at most %d", len(body), maxSourceBodyBytes)
	}
}

func TestReadRejectsMalformedCursor(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	conn := testConn(server.URL, nil)
	source := newTestSource(t, conn, sourceToken, server)
	for _, tc := range []struct {
		name string
		cp   domain.Checkpoint
	}{
		{name: "bad json", cp: domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: []byte("{nope")}},
		{name: "bad stream", cp: domain.Checkpoint{Stream: "other", Version: checkpointVersion, Data: []byte(`{}`)}},
		{name: "bad version", cp: domain.Checkpoint{Stream: checkpointStream, Version: 999, Data: []byte(`{}`)}},
		{name: "negative offset", cp: domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: []byte(`{"offset":-1}`)}},
		{name: "huge offset", cp: domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: []byte(`{"offset":1000}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := source.Read(context.Background(), ports.ReadRequest{
				Connection:  conn,
				Checkpoints: map[string]domain.Checkpoint{checkpointStream: tc.cp},
			})
			if err == nil {
				t.Fatal("Read accepted a malformed cursor")
			}
		})
	}
}

func TestSourceContract(t *testing.T) {
	fake := &linearFake{token: sourceToken, now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	fake.addIssue()
	fake.addIssue()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	conn := testConn(server.URL, nil)
	bad := conn
	bad.ID = "ln-bad"
	harness := ingestiontest.Harness{
		Connection:    conn,
		BadConnection: &bad,
		PageSize:      5,
		NewSource: func(connection domain.Connection) ports.Source {
			source, err := NewSource(connection, sourceCredentials{token: sourceToken}, server.Client(), func() time.Time {
				return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
			})
			if err != nil {
				t.Fatalf("NewSource(%s): %v", connection.ID, err)
			}
			return source
		},
		AddItem:    func() { fake.addIssue() },
		UpdateItem: func() { fake.updateIssue() },
		AddMany: func(n int) {
			for range n {
				fake.addIssue()
			}
		},
	}
	ingestiontest.RunSourceSuite(t, harness)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ ports.CredentialProvider = sourceCredentials{}
