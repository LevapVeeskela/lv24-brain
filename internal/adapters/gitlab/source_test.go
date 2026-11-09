package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

const sourceTestToken = "gitlab-source-test-token"

type sourceTestCredentials struct{ token string }

func (c sourceTestCredentials) Get(_ context.Context, connectionID, field string) (string, error) {
	if connectionID != "gl-test" || field != "token" || c.token == "" {
		return "", domain.ErrNotFound
	}
	return c.token, nil
}

func TestSourceReadsIssueDescriptionAndBoundedNotesWithCheckpoint(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != sourceTestToken {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		requests = append(requests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/api/v4/user":
			_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
		case "/api/v4/projects/acme%2Fwidgets/issues":
			_, _ = w.Write([]byte(`[{"iid":7,"title":"Bug","description":"issue description","state":"opened","web_url":"https://gitlab.test/acme/widgets/-/issues/7","author":{"username":"alice"},"created_at":"2026-11-01T00:00:00Z","updated_at":"2026-11-02T00:00:00Z","labels":["backend"]}]`))
		case "/api/v4/projects/acme%2Fwidgets/issues/7/notes":
			_, _ = w.Write([]byte(`[{"id":31,"body":"note body","author":{"username":"bob"},"created_at":"2026-11-02T01:00:00Z","system":false}]`))
		case "/api/v4/projects/acme%2Fwidgets/merge_requests":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v4/projects/acme%2Fwidgets/repository/commits":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request path: %s", r.URL.RequestURI())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	conn := domain.Connection{ID: "gl-test", Provider: Name, BaseURL: server.URL, Settings: map[string]string{"projects": "acme/widgets"}}
	source, err := NewSource(conn, sourceTestCredentials{token: sourceTestToken}, server.Client(), func() time.Time {
		return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	// The source constructor and descriptor are available to native registry
	// wiring without changing the legacy Connector API.
	desc := Descriptor()
	if desc.Provider != Name || !desc.Capabilities.Incremental || desc.Capabilities.Deletes {
		t.Fatalf("Descriptor() = %#v", desc)
	}

	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 1})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 || batch.Items[0].Kind != domain.ItemIssue {
		t.Fatalf("items = %#v, want one issue", batch.Items)
	}
	item := batch.Items[0]
	if item.Ref.ExternalID != "acme/widgets#issue-7" || !strings.Contains(item.Body, "issue description") || !strings.Contains(item.Body, "note body") {
		t.Fatalf("item = %#v, want stable id and body with note", item)
	}
	if len(batch.Checkpoints) < 1 || batch.Checkpoints[0].Stream == "" || !batch.More {
		t.Fatalf("batch checkpoints = %#v more=%v; want resumable next stream", batch.Checkpoints, batch.More)
	}
	if len(requests) < 3 || !strings.Contains(requests[1], "issues") || !strings.Contains(requests[2], "/notes") {
		t.Fatalf("requests = %v, want issue list and notes", requests)
	}
}

var _ ports.CredentialProvider = sourceTestCredentials{}

type sourceFixture struct {
	mu            sync.Mutex
	issues        []sourceIssue
	mergeRequests []sourceMergeRequest
	commits       []sourceCommit
	notes         []sourceNote
	clock         time.Time
	nextIID       int
}

func newSourceFixture() *sourceFixture {
	f := &sourceFixture{clock: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}
	f.addIssueLocked()
	f.addIssueLocked()
	f.addIssueLocked()
	f.clock = f.clock.Add(time.Second)
	f.mergeRequests = append(f.mergeRequests, sourceMergeRequest{
		IID: 1, Title: "Merge request", Description: "mr description", State: "opened",
		WebURL: "https://gitlab.test/acme/widgets/-/merge_requests/1", Author: author{Username: "bob"},
		CreatedAt: f.clock, UpdatedAt: f.clock, Labels: []string{"feature"},
	})
	f.clock = f.clock.Add(time.Second)
	f.commits = append(f.commits, sourceCommit{
		ID: "abc123", Title: "Commit title", Message: "Commit title\n\nbody", AuthorName: "Alice",
		AuthorEmail: "alice@example.test", CommittedDate: f.clock, AuthoredDate: f.clock,
		WebURL: "https://gitlab.test/acme/widgets/-/commit/abc123",
	})
	f.notes = append(f.notes, sourceNote{
		ID: 901, Body: "a comment", Author: author{Username: "carol"}, CreatedAt: f.clock,
	})
	return f
}

func (f *sourceFixture) addIssueLocked() {
	f.nextIID++
	f.clock = f.clock.Add(time.Second)
	iid := f.nextIID
	f.issues = append(f.issues, sourceIssue{
		IID: iid, Title: fmt.Sprintf("Issue %d", iid), Description: fmt.Sprintf("description %d", iid),
		State: "opened", WebURL: fmt.Sprintf("https://gitlab.test/acme/widgets/-/issues/%d", iid),
		Author: author{Username: "alice"}, CreatedAt: f.clock, UpdatedAt: f.clock, Labels: []string{"fixture"},
	})
}

func (f *sourceFixture) handler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("PRIVATE-TOKEN") != sourceTestToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/v4/user":
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	case strings.HasSuffix(r.URL.Path, "/notes") || strings.HasSuffix(r.URL.Path, "/comments"):
		f.mu.Lock()
		notes := append([]sourceNote(nil), f.notes...)
		f.mu.Unlock()
		writeSourcePage(w, r, notes)
	case strings.HasSuffix(r.URL.Path, "/issues"):
		f.mu.Lock()
		items := append([]sourceIssue(nil), f.issues...)
		f.mu.Unlock()
		filtered := make([]sourceIssue, 0, len(items))
		for _, item := range items {
			if after := r.URL.Query().Get("updated_after"); after != "" {
				at, _ := time.Parse(time.RFC3339Nano, after)
				if !item.UpdatedAt.After(at) {
					continue
				}
			}
			filtered = append(filtered, item)
		}
		sort.Slice(filtered, func(i, j int) bool {
			if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
				return filtered[i].UpdatedAt.Before(filtered[j].UpdatedAt)
			}
			return filtered[i].IID < filtered[j].IID
		})
		writeSourcePage(w, r, filtered)
	case strings.HasSuffix(r.URL.Path, "/merge_requests"):
		f.mu.Lock()
		items := append([]sourceMergeRequest(nil), f.mergeRequests...)
		f.mu.Unlock()
		filtered := make([]sourceMergeRequest, 0, len(items))
		for _, item := range items {
			if after := r.URL.Query().Get("updated_after"); after != "" {
				at, _ := time.Parse(time.RFC3339Nano, after)
				if !item.UpdatedAt.After(at) {
					continue
				}
			}
			filtered = append(filtered, item)
		}
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].UpdatedAt.Before(filtered[j].UpdatedAt) })
		writeSourcePage(w, r, filtered)
	case strings.HasSuffix(r.URL.Path, "/repository/commits"):
		f.mu.Lock()
		items := append([]sourceCommit(nil), f.commits...)
		f.mu.Unlock()
		filtered := make([]sourceCommit, 0, len(items))
		for _, item := range items {
			if since := r.URL.Query().Get("since"); since != "" {
				at, _ := time.Parse(time.RFC3339Nano, since)
				if item.CommittedDate.Before(at) {
					continue
				}
			}
			filtered = append(filtered, item)
		}
		writeSourcePage(w, r, filtered)
	default:
		http.NotFound(w, r)
	}
}

func writeSourcePage[T any](w http.ResponseWriter, r *http.Request, items []T) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage < 1 {
		perPage = sourcePageSize
	}
	start := (page - 1) * perPage
	end := min(start+perPage, len(items))
	if start > len(items) {
		start = len(items)
	}
	if end < len(items) {
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
	}
	_ = json.NewEncoder(w).Encode(items[start:end])
}

func TestSourceContract(t *testing.T) {
	fixture := newSourceFixture()
	server := httptest.NewServer(http.HandlerFunc(fixture.handler))
	defer server.Close()
	conn := domain.Connection{ID: "gl-test", Provider: Name, BaseURL: server.URL, Settings: map[string]string{"projects": "acme/widgets"}}
	newSource := func(c domain.Connection) ports.Source {
		source, err := NewSource(c, sourceTestCredentials{token: sourceTestToken}, server.Client(), nil)
		if err != nil {
			t.Fatalf("NewSource: %v", err)
		}
		return source
	}
	h := ingestiontest.Harness{
		Connection: conn,
		NewSource:  newSource,
		AddItem: func() {
			fixture.mu.Lock()
			fixture.addIssueLocked()
			fixture.mu.Unlock()
		},
		UpdateItem: func() {
			fixture.mu.Lock()
			fixture.clock = fixture.clock.Add(time.Second)
			fixture.issues[0].Title += " updated"
			fixture.issues[0].Description += " revised"
			fixture.issues[0].UpdatedAt = fixture.clock
			fixture.mu.Unlock()
		},
		AddMany: func(n int) {
			fixture.mu.Lock()
			for range n {
				fixture.addIssueLocked()
			}
			fixture.mu.Unlock()
		},
		PageSize: 3,
	}
	ingestiontest.RunSourceSuite(t, h)
}
