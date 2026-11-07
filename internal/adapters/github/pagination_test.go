package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var pagedBase = time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)

// fakeCollection serves numbered items 1..total for any collection path. Item N
// is updated N hours before pagedBase, so ascending N is updated_at descending,
// matching the API ordering the connector requests.
type fakeCollection struct {
	t        *testing.T
	total    int
	pageSize int
	// foreignLink, when set, replaces the Link header on every page.
	foreignLink string

	mu       sync.Mutex
	requests []*url.URL
	// baseLink is the server origin used to build rel="next" targets.
	baseLink string
}

func (f *fakeCollection) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.URL)
	f.mu.Unlock()

	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		f.t.Errorf("Authorization = %q", got)
	}
	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			f.t.Errorf("page = %q", raw)
		}
		page = parsed
	}
	last := min(page*f.pageSize, f.total)
	var items []string
	for n := (page-1)*f.pageSize + 1; n <= last; n++ {
		items = append(items, fakeItem(n))
	}
	switch {
	case f.foreignLink != "":
		w.Header().Set("Link", f.foreignLink)
	case last < f.total:
		query := r.URL.Query()
		query.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf("<%s%s?%s>; rel=\"next\"", f.baseLink, r.URL.Path, query.Encode()))
	}
	writeJSON(f.t, w, "["+strings.Join(items, ",")+"]")
}

func (f *fakeCollection) requestURLs() []*url.URL {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*url.URL(nil), f.requests...)
}

func fakeItem(n int) string {
	updated := pagedBase.Add(-time.Duration(n) * time.Hour).Format(time.RFC3339)
	return fmt.Sprintf(`{"number":%d,"title":"Item %d","state":"open","html_url":"https://github.test/acme/widgets/issues/%d",`+
		`"user":{"login":"alice"},"created_at":%q,"updated_at":%q}`, n, n, n, updated, updated)
}

func newPagedConnector(t *testing.T, fake *fakeCollection) *Connector {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	fake.baseLink = server.URL
	connector, err := New(Config{
		BaseURL:    server.URL,
		Repos:      []string{"acme/widgets"},
		Secrets:    fakeSecrets{value: testToken},
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return connector
}

func numbers(items []issue) []int {
	out := make([]int, 0, len(items))
	for _, item := range items {
		out = append(out, item.Number)
	}
	return out
}

func TestListIssuesFollowsLinkAcrossThreePages(t *testing.T) {
	fake := &fakeCollection{t: t, total: 9, pageSize: 3}
	connector := newPagedConnector(t, fake)

	items, err := connector.listIssues(context.Background(), testToken, "acme/widgets", nil, 100)
	if err != nil {
		t.Fatalf("listIssues: %v", err)
	}
	if got := len(fake.requestURLs()); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	if got := numbers(items); len(got) != 9 || got[0] != 1 || got[8] != 9 {
		t.Fatalf("numbers = %v", got)
	}
	for _, req := range fake.requestURLs() {
		if got := req.Query().Get("per_page"); got != "100" {
			t.Fatalf("per_page = %q, want 100", got)
		}
	}
}

func TestListIssuesTruncatesMidPage(t *testing.T) {
	fake := &fakeCollection{t: t, total: 9, pageSize: 3}
	connector := newPagedConnector(t, fake)

	items, err := connector.listIssues(context.Background(), testToken, "acme/widgets", nil, 5)
	if err != nil {
		t.Fatalf("listIssues: %v", err)
	}
	if got := numbers(items); len(got) != 5 || got[4] != 5 {
		t.Fatalf("numbers = %v, want 1..5", got)
	}
	if got := len(fake.requestURLs()); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if got := fake.requestURLs()[0].Query().Get("per_page"); got != "5" {
		t.Fatalf("per_page = %q, want 5", got)
	}
}

func TestListIssuesStopsAtMaxPages(t *testing.T) {
	fake := &fakeCollection{t: t, total: 1000, pageSize: 1}
	connector := newPagedConnector(t, fake)

	items, err := connector.listIssues(context.Background(), testToken, "acme/widgets", nil, MaxLimit)
	if err != nil {
		t.Fatalf("listIssues: %v", err)
	}
	if got := len(fake.requestURLs()); got != maxPages {
		t.Fatalf("requests = %d, want maxPages=%d", got, maxPages)
	}
	if len(items) != maxPages {
		t.Fatalf("items = %d, want %d", len(items), maxPages)
	}
}

func TestListIssuesIgnoresForeignHostNextLink(t *testing.T) {
	var foreignHits int
	var foreignMu sync.Mutex
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignMu.Lock()
		foreignHits++
		foreignMu.Unlock()
		writeJSON(t, w, `[]`)
	}))
	t.Cleanup(foreign.Close)

	fake := &fakeCollection{t: t, total: 9, pageSize: 3, foreignLink: fmt.Sprintf("<%s/steal?page=2>; rel=\"next\"", foreign.URL)}
	connector := newPagedConnector(t, fake)

	items, err := connector.listIssues(context.Background(), testToken, "acme/widgets", nil, 100)
	if err != nil {
		t.Fatalf("listIssues: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want first page only", len(items))
	}
	if got := len(fake.requestURLs()); got != 1 {
		t.Fatalf("API requests = %d, want 1", got)
	}
	foreignMu.Lock()
	defer foreignMu.Unlock()
	if foreignHits != 0 {
		t.Fatalf("foreign host received %d requests", foreignHits)
	}
}

func TestListPullRequestsStopsPagingBeforeSince(t *testing.T) {
	fake := &fakeCollection{t: t, total: 9, pageSize: 3}
	connector := newPagedConnector(t, fake)
	// Item N is updated N hours before pagedBase, so items 1..5 are not older than since.
	since := pagedBase.Add(-5 * time.Hour)

	pulls, err := connector.listPullRequests(context.Background(), testToken, "acme/widgets", &since, 100)
	if err != nil {
		t.Fatalf("listPullRequests: %v", err)
	}
	if len(pulls) != 5 {
		t.Fatalf("pulls = %d, want 5", len(pulls))
	}
	requests := fake.requestURLs()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2 (page 3 must not be fetched)", len(requests))
	}
	if got := requests[0].Query().Get("sort"); got != "updated" {
		t.Fatalf("sort = %q", got)
	}
	if got := requests[0].Query().Get("direction"); got != "desc" {
		t.Fatalf("direction = %q", got)
	}
}

func TestNormalizeLimitBounds(t *testing.T) {
	cases := map[int]int{0: DefaultLimit, -3: DefaultLimit, 100: 100, 1000: 1000, 5000: MaxLimit}
	for in, want := range cases {
		if got := normalizeLimit(in); got != want {
			t.Fatalf("normalizeLimit(%d) = %d, want %d", in, got, want)
		}
	}
}
