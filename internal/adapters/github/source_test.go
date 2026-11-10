package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

const sourceToken = "sanitized-test-token"

type sourceCredentials struct{ token string }

func (c sourceCredentials) Get(_ context.Context, connectionID, field string) (string, error) {
	if connectionID != "gh-test" || field != "token" || c.token == "" {
		return "", domain.ErrNotFound
	}
	return c.token, nil
}

func TestSourceReadsBoundedIssueBodyAndCommentsWithResumableCheckpoint(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+sourceToken {
			t.Errorf("Authorization = %q", got)
		}
		paths = append(paths, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets/issues":
			_, _ = w.Write([]byte(`[{"number":7,"title":"Bug","body":"issue body","state":"open","html_url":"https://github.test/acme/widgets/issues/7","user":{"login":"alice"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-02T00:00:00Z"}]`))
		case "/repos/acme/widgets/issues/7/comments":
			_, _ = w.Write([]byte(`[{"id":31,"body":"comment body","user":{"login":"bob"},"created_at":"2026-11-02T01:00:00Z"}]`))
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	conn := domain.Connection{ID: "gh-test", Provider: Name, BaseURL: server.URL, Projects: []string{"lv24"}, Settings: map[string]string{"repos": "acme/widgets"}}
	source, err := NewSource(conn, sourceCredentials{token: sourceToken}, server.Client(), func() time.Time {
		return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 1})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 || batch.Items[0].Kind != domain.ItemIssue {
		t.Fatalf("items = %#v, want one issue", batch.Items)
	}
	item := batch.Items[0]
	if item.Ref.ExternalID != "acme/widgets#issue-7" || !strings.Contains(item.Body, "issue body") || !strings.Contains(item.Body, "comment body") {
		t.Fatalf("item = %#v, want stable id and body with comment", item)
	}
	if item.ProjectID != "lv24" {
		t.Fatalf("ProjectID = %q, want the single configured brain project", item.ProjectID)
	}
	if len(batch.Checkpoints) != 1 || batch.Checkpoints[0].Stream == "" || !batch.More {
		t.Fatalf("batch cursor = %#v more=%v; want resumable next stream", batch.Checkpoints, batch.More)
	}
	if len(paths) != 2 {
		t.Fatalf("requests = %v, want issue page and comments", paths)
	}
}

func TestSourceNormalizesIssueAndPullRequestState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/repos/acme/widgets/issues":
			_, _ = w.Write([]byte(`[
				{"number":1,"title":"Open issue","state":"open","html_url":"https://github.test/acme/widgets/issues/1","user":{"login":"alice"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-02T00:00:00Z"},
				{"number":2,"title":"Closed issue","state":"closed","html_url":"https://github.test/acme/widgets/issues/2","user":{"login":"alice"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-02T00:00:00Z","closed_at":"2026-11-02T00:00:00Z"}
			]`))
		case r.URL.Path == "/repos/acme/widgets/pulls":
			_, _ = w.Write([]byte(`[
				{"number":3,"title":"Open pull","state":"open","html_url":"https://github.test/acme/widgets/pull/3","user":{"login":"alice"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-02T00:00:00Z"},
				{"number":4,"title":"Merged pull","state":"closed","merged_at":"2026-11-02T00:00:00Z","html_url":"https://github.test/acme/widgets/pull/4","user":{"login":"alice"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-02T00:00:00Z"}
			]`))
		case r.URL.Path == "/repos/acme/widgets/commits":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	conn := domain.Connection{ID: "gh-test", Provider: Name, BaseURL: server.URL, Settings: map[string]string{"repos": "acme/widgets"}}
	source, err := NewSource(conn, sourceCredentials{token: sourceToken}, server.Client(), func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 4 {
		t.Fatalf("items = %#v, want four", batch.Items)
	}
	want := []domain.ItemState{
		domain.ItemStateOpen,
		domain.ItemStateClosed,
		domain.ItemStateOpen,
		domain.ItemStateMerged,
	}
	for index, state := range want {
		if batch.Items[index].State != state {
			t.Fatalf("item %d state = %q, want %q", index, batch.Items[index].State, state)
		}
	}
}

func TestSourceReadsPullRequestReviewCommentsAndCommitComments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+sourceToken {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets/issues":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/acme/widgets/pulls":
			_, _ = w.Write([]byte(`[{"number":3,"title":"Change","body":"PR body","state":"open","html_url":"https://github.test/acme/widgets/pull/3","user":{"login":"alice"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-02T00:00:00Z"}]`))
		case "/repos/acme/widgets/issues/3/comments":
			_, _ = w.Write([]byte(`[{"body":"conversation","user":{"login":"bob"},"created_at":"2026-11-02T01:00:00Z"}]`))
		case "/repos/acme/widgets/pulls/3/comments":
			_, _ = w.Write([]byte(`[{"body":"review note","user":{"login":"carol"},"created_at":"2026-11-02T02:00:00Z"}]`))
		case "/repos/acme/widgets/commits":
			_, _ = w.Write([]byte(`[{"sha":"0123456789abcdef","html_url":"https://github.test/acme/widgets/commit/0123456789abcdef","commit":{"author":{"name":"Alice","date":"2026-11-03T00:00:00Z"},"message":"Fix bug\nMore detail"},"author":{"login":"alice"}}]`))
		case "/repos/acme/widgets/commits/0123456789abcdef/comments":
			_, _ = w.Write([]byte(`[{"body":"commit note","user":{"login":"dave"},"created_at":"2026-11-03T01:00:00Z"}]`))
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	conn := domain.Connection{ID: "gh-test", Provider: Name, BaseURL: server.URL, Settings: map[string]string{"repos": "acme/widgets"}}
	source, err := NewSource(conn, sourceCredentials{token: sourceToken}, server.Client(), func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 2 || batch.Items[0].Kind != domain.ItemPullRequest || batch.Items[1].Kind != domain.ItemCommit || batch.More {
		t.Fatalf("batch = %#v more=%v, want PR + commit and completed page", batch.Items, batch.More)
	}
	if !strings.Contains(batch.Items[0].Body, "PR body") || !strings.Contains(batch.Items[0].Body, "conversation") || !strings.Contains(batch.Items[0].Body, "review note") {
		t.Fatalf("pull request body missing body or comments: %q", batch.Items[0].Body)
	}
	if batch.Items[1].Title != "Fix bug" || !strings.Contains(batch.Items[1].Body, "commit note") {
		t.Fatalf("commit = %#v, want first-line title and comment", batch.Items[1])
	}
}

func TestBoundedBodyCapsBytesWithoutSplittingUTF8(t *testing.T) {
	body := boundedBody(strings.Repeat("я", maxSourceBodyBytes), nil)
	if len(body) > maxSourceBodyBytes || !utf8.ValidString(body) {
		t.Fatalf("body bytes=%d validUTF8=%v", len(body), utf8.ValidString(body))
	}
}

func TestSourceKeepsPerCollectionWatermarksAndHonorsSince(t *testing.T) {
	issuePage := 0
	commitPage := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+sourceToken {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/widgets/issues":
			issuePage++
			updated := "2026-11-02T00:00:00Z"
			if issuePage > 1 {
				updated = "2026-11-05T00:00:00Z"
			}
			_, _ = w.Write([]byte(`[{"number":7,"title":"Bug","body":"issue","state":"open","html_url":"https://github.test/acme/widgets/issues/7","user":{"login":"alice"},"created_at":"2026-11-01T00:00:00Z","updated_at":"` + updated + `"}]`))
		case "/repos/acme/widgets/issues/7/comments", "/repos/acme/widgets/issues/8/comments", "/repos/acme/widgets/pulls/8/comments", "/repos/acme/widgets/commits/abcdef/comments":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/acme/widgets/pulls":
			_, _ = w.Write([]byte(`[{"number":8,"title":"PR","body":"pull","state":"open","html_url":"https://github.test/acme/widgets/pull/8","user":{"login":"alice"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-10T00:00:00Z"}]`))
		case "/repos/acme/widgets/commits":
			commitPage++
			date := "2026-11-03T00:00:00Z"
			if commitPage > 1 {
				date = "2026-11-04T00:00:00Z"
			}
			_, _ = w.Write([]byte(`[{"sha":"abcdef","html_url":"https://github.test/acme/widgets/commit/abcdef","commit":{"author":{"name":"Alice","date":"` + date + `"},"message":"commit"}}]`))
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	conn := domain.Connection{ID: "gh-test", Provider: Name, BaseURL: server.URL, Settings: map[string]string{"repos": "acme/widgets"}}
	source, err := NewSource(conn, sourceCredentials{token: sourceToken}, server.Client(), func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if len(first.Items) != 3 || first.More {
		t.Fatalf("first batch items=%d more=%v, want all three collections", len(first.Items), first.More)
	}
	checkpoints := map[string]domain.Checkpoint{checkpointStream: first.Checkpoints[0]}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints, Limit: 10})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if len(second.Items) != 3 {
		t.Fatalf("second batch items=%#v, want per-stream overlap to retain issue and commit updates", second.Items)
	}
	cutoff := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	third, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: map[string]domain.Checkpoint{checkpointStream: second.Checkpoints[0]}, Since: &cutoff, Limit: 10})
	if err != nil {
		t.Fatalf("third Read: %v", err)
	}
	if len(third.Items) != 1 || third.Items[0].Kind != domain.ItemPullRequest {
		t.Fatalf("explicit Since returned %#v, want only PR updated after cutoff", third.Items)
	}
}

func TestSourceRejectsNegativeCheckpointCollection(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	conn := domain.Connection{ID: "gh-test", Provider: Name, BaseURL: server.URL, Settings: map[string]string{"repos": "acme/widgets"}}
	source, err := NewSource(conn, sourceCredentials{token: sourceToken}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	cp := domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: []byte(`{"collection":-1,"page":1,"offset":0,"scanSince":[null,null,null],"watermarks":[null,null,null]}`)}
	if _, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: map[string]domain.Checkpoint{checkpointStream: cp}}); err == nil {
		t.Fatal("Read accepted a negative collection cursor")
	}
}

func TestSourceContractSuite(t *testing.T) {
	fixture := &githubSuiteFixture{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	fixture.addItemLocked()
	fixture.addItemLocked()
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	conn := domain.Connection{ID: "gh-test", Provider: Name, BaseURL: server.URL, Projects: []string{"lv24"}, Settings: map[string]string{"repos": "acme/widgets"}}
	badConnection := conn
	badConnection.ID = "gh-bad"
	harness := ingestiontest.Harness{
		Connection:    conn,
		BadConnection: &badConnection,
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

type githubSuiteFixture struct {
	mu     sync.Mutex
	items  []issue
	now    time.Time
	nextID int
}

func (f *githubSuiteFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+sourceToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/repos/acme/widgets":
		_, _ = w.Write([]byte(`{"full_name":"acme/widgets"}`))
	case "/repos/acme/widgets/issues":
		f.mu.Lock()
		items := append([]issue(nil), f.items...)
		f.mu.Unlock()
		if rawSince := r.URL.Query().Get("since"); rawSince != "" {
			since, err := time.Parse(time.RFC3339, rawSince)
			if err != nil {
				http.Error(w, "invalid since", http.StatusBadRequest)
				return
			}
			filtered := items[:0]
			for _, item := range items {
				if !item.UpdatedAt.Before(since) {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		}
		sort.Slice(items, func(i, j int) bool {
			if !items[i].UpdatedAt.Equal(items[j].UpdatedAt) {
				return items[i].UpdatedAt.Before(items[j].UpdatedAt)
			}
			return items[i].Number < items[j].Number
		})
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if page < 1 {
			page = 1
		}
		if perPage < 1 {
			perPage = maxSourcePage
		}
		start := min((page-1)*perPage, len(items))
		end := min(start+perPage, len(items))
		_ = json.NewEncoder(w).Encode(items[start:end])
	case "/repos/acme/widgets/pulls", "/repos/acme/widgets/commits":
		_, _ = w.Write([]byte(`[]`))
	default:
		if strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/issues/") && strings.HasSuffix(r.URL.Path, "/comments") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		http.NotFound(w, r)
	}
}

func (f *githubSuiteFixture) addItem() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addItemLocked()
}

func (f *githubSuiteFixture) addItemLocked() {
	f.nextID++
	f.now = f.now.Add(time.Second)
	f.items = append(f.items, issue{
		Number: f.nextID, Title: "Issue " + strconv.Itoa(f.nextID), Body: "body",
		State: "open", HTMLURL: "https://github.test/acme/widgets/issues/" + strconv.Itoa(f.nextID),
		User: user{Login: "alice"}, CreatedAt: f.now, UpdatedAt: f.now,
	})
}

func (f *githubSuiteFixture) updateItem() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.items) == 0 {
		f.addItemLocked()
	}
	f.now = f.now.Add(time.Minute)
	f.items[0].Body += " updated"
	f.items[0].UpdatedAt = f.now
}

var _ ports.CredentialProvider = sourceCredentials{}
