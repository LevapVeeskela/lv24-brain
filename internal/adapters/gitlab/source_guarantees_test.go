package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// mapCredentials resolves conn/<id>/<field> from an in-memory map.
type mapCredentials map[string]string

func (c mapCredentials) Get(_ context.Context, connectionID, field string) (string, error) {
	if value := c[connectionID+"/"+field]; value != "" {
		return value, nil
	}
	return "", domain.ErrNotFound
}

type paginationMode int

const (
	paginationHeader paginationMode = iota
	paginationNone
	paginationMalformed
	paginationNonMonotonic
	paginationLink
	paginationForeignLink
)

type fakeRequest struct {
	Path  string
	Query string
	Token string
}

// fakeGitLab is a deterministic GitLab v4 fake: numbered pages announced by
// X-Next-Page (or Link), updated_after/since filters with the documented
// inclusivity, and per-item notes.
type fakeGitLab struct {
	token string

	mu               sync.Mutex
	perPage          int
	issues           []sourceIssue
	mergeRequests    []sourceMergeRequest
	commits          []sourceCommit
	issuesByPath     map[string][]sourceIssue
	mrsByPath        map[string][]sourceMergeRequest
	commitsByPath    map[string][]sourceCommit
	notes            map[string][]sourceNote
	ignoreNoteLimit  bool
	pagination       paginationMode
	failSuffix       map[string]int
	userStatus       int
	userBody         string
	beforeIssuesPage func(page int)
	requests         []fakeRequest
	noteRequests     int
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	return &fakeGitLab{
		token:      sourceTestToken,
		perPage:    sourcePageSize,
		pagination: paginationHeader,
		notes:      make(map[string][]sourceNote),
		failSuffix: make(map[string]int),
	}
}

func (f *fakeGitLab) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, fakeRequest{Path: r.URL.Path, Query: r.URL.RawQuery, Token: r.Header.Get("PRIVATE-TOKEN")})
	w.Header().Set("Content-Type", "application/json")
	for suffix, status := range f.failSuffix {
		if strings.HasSuffix(r.URL.Path, suffix) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
			return
		}
	}
	path := r.URL.Path
	query := r.URL.Query()
	switch {
	case strings.HasSuffix(path, "/user"):
		if f.userStatus != 0 {
			w.WriteHeader(f.userStatus)
			_, _ = w.Write([]byte(f.userBody))
			return
		}
		_, _ = w.Write([]byte(`{"id":1,"username":"tester"}`))
	case strings.HasSuffix(path, "/notes"), strings.HasSuffix(path, "/comments"):
		f.noteRequests++
		f.writeNotesLocked(w, path)
	case strings.HasSuffix(path, "/issues"):
		page := pageNumber(query)
		if f.beforeIssuesPage != nil {
			f.beforeIssuesPage(page)
		}
		f.writeIssuesLocked(w, r, path, query)
	case strings.HasSuffix(path, "/merge_requests"):
		f.writeMergeRequestsLocked(w, r, path, query)
	case strings.HasSuffix(path, "/repository/commits"):
		f.writeCommitsLocked(w, r, path, query)
	default:
		http.NotFound(w, r)
	}
}

func pageNumber(query url.Values) int {
	page, err := strconv.Atoi(query.Get("page"))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

func (f *fakeGitLab) pageBounds(page, total int) (start, end int, more bool) {
	perPage := f.perPage
	if perPage <= 0 {
		perPage = sourcePageSize
	}
	start = (page - 1) * perPage
	if start > total {
		start = total
	}
	end = min(start+perPage, total)
	return start, end, end < total
}

func (f *fakeGitLab) setPagination(w http.ResponseWriter, r *http.Request, page int, more bool) {
	switch f.pagination {
	case paginationNone:
	case paginationMalformed:
		w.Header().Set("X-Next-Page", "two")
	case paginationNonMonotonic:
		if more {
			w.Header().Set("X-Next-Page", "1")
		}
	case paginationLink:
		if more {
			w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=%d>; rel=\"next\"", r.Host, r.URL.EscapedPath(), page+1))
		}
	case paginationForeignLink:
		if more {
			w.Header().Set("Link", `<https://evil.example/api/v4/projects/x/issues?page=2>; rel="next"`)
		}
	default:
		// GitLab always sends the header; the last page has an empty value.
		next := ""
		if more {
			next = strconv.Itoa(page + 1)
		}
		w.Header().Set("X-Next-Page", next)
	}
}

func (f *fakeGitLab) writeIssuesLocked(w http.ResponseWriter, r *http.Request, path string, query url.Values) {
	items := f.issues
	if f.issuesByPath != nil {
		items = f.issuesByPath[projectFromPath(r.URL.EscapedPath())]
	}
	after := query.Get("updated_after")
	var bound time.Time
	if after != "" {
		bound, _ = time.Parse(time.RFC3339Nano, after)
	}
	filtered := make([]sourceIssue, 0, len(items))
	for _, item := range items {
		// GitLab's updated_after is exclusive.
		if after != "" && !item.UpdatedAt.After(bound) {
			continue
		}
		filtered = append(filtered, item)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
			return filtered[i].UpdatedAt.Before(filtered[j].UpdatedAt)
		}
		return filtered[i].IID < filtered[j].IID
	})
	page := pageNumber(query)
	start, end, more := f.pageBounds(page, len(filtered))
	f.setPagination(w, r, page, more)
	_ = json.NewEncoder(w).Encode(filtered[start:end])
}

func (f *fakeGitLab) writeMergeRequestsLocked(w http.ResponseWriter, r *http.Request, path string, query url.Values) {
	items := f.mergeRequests
	if f.mrsByPath != nil {
		items = f.mrsByPath[projectFromPath(r.URL.EscapedPath())]
	}
	after := query.Get("updated_after")
	var bound time.Time
	if after != "" {
		bound, _ = time.Parse(time.RFC3339Nano, after)
	}
	filtered := make([]sourceMergeRequest, 0, len(items))
	for _, item := range items {
		if after != "" && !item.UpdatedAt.After(bound) {
			continue
		}
		filtered = append(filtered, item)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
			return filtered[i].UpdatedAt.Before(filtered[j].UpdatedAt)
		}
		return filtered[i].IID < filtered[j].IID
	})
	page := pageNumber(query)
	start, end, more := f.pageBounds(page, len(filtered))
	f.setPagination(w, r, page, more)
	_ = json.NewEncoder(w).Encode(filtered[start:end])
}

func (f *fakeGitLab) writeCommitsLocked(w http.ResponseWriter, r *http.Request, path string, query url.Values) {
	items := f.commits
	if f.commitsByPath != nil {
		items = f.commitsByPath[projectFromPath(r.URL.EscapedPath())]
	}
	since := query.Get("since")
	var bound time.Time
	if since != "" {
		bound, _ = time.Parse(time.RFC3339Nano, since)
	}
	filtered := make([]sourceCommit, 0, len(items))
	for _, item := range items {
		// GitLab's commit since is inclusive.
		if since != "" && item.CommittedDate.Before(bound) {
			continue
		}
		filtered = append(filtered, item)
	}
	// GitLab lists commits newest first and offers no sort parameter.
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].CommittedDate.Equal(filtered[j].CommittedDate) {
			return filtered[i].CommittedDate.After(filtered[j].CommittedDate)
		}
		return filtered[i].ID > filtered[j].ID
	})
	page := pageNumber(query)
	start, end, more := f.pageBounds(page, len(filtered))
	f.setPagination(w, r, page, more)
	_ = json.NewEncoder(w).Encode(filtered[start:end])
}

func (f *fakeGitLab) writeNotesLocked(w http.ResponseWriter, path string) {
	notes := append([]sourceNote(nil), f.notes[noteKey(path)]...)
	sort.SliceStable(notes, func(i, j int) bool {
		if !notes[i].CreatedAt.Equal(notes[j].CreatedAt) {
			return notes[i].CreatedAt.After(notes[j].CreatedAt)
		}
		return notes[i].ID > notes[j].ID
	})
	limit := maxNotesPerItem
	if f.ignoreNoteLimit {
		limit = len(notes)
	}
	if len(notes) > limit {
		notes = notes[:limit]
	}
	_ = json.NewEncoder(w).Encode(notes)
}

// projectFromPath extracts the project from an escaped request path, so a
// path-encoded slash (acme%2Fwidgets) stays part of the project.
func projectFromPath(escapedPath string) string {
	_, rest, ok := strings.Cut(escapedPath, "/projects/")
	if !ok {
		return ""
	}
	project, _, _ := strings.Cut(rest, "/")
	unescaped, err := url.PathUnescape(project)
	if err != nil {
		return project
	}
	return unescaped
}

func noteKey(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for index := 0; index+1 < len(parts); index++ {
		switch parts[index] {
		case "issues", "merge_requests":
			return parts[index] + ":" + parts[index+1]
		case "commits":
			return "commits:" + parts[index+1]
		}
	}
	return ""
}

// source builds a Source bound to the fake. The projects setting comes from
// projects and conn.BaseURL defaults to the test server root.
func (f *fakeGitLab) source(t *testing.T, conn *domain.Connection, projects string) (*Source, *httptest.Server) {
	t.Helper()
	return f.sourceAt(t, conn, projects, "")
}

// sourceAt is source with an optional base URL path suffix (self-hosted path).
func (f *fakeGitLab) sourceAt(t *testing.T, conn *domain.Connection, projects, suffix string) (*Source, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	conn.Provider = Name
	conn.BaseURL = server.URL + suffix
	if conn.ID == "" {
		conn.ID = "gl-test"
	}
	if conn.Settings == nil {
		conn.Settings = make(map[string]string)
	}
	conn.Settings["projects"] = projects
	source, err := NewSource(*conn, mapCredentials{conn.ID + "/token": f.token}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return source, server
}

func mustRead(t *testing.T, source *Source, conn domain.Connection, checkpoints map[string]domain.Checkpoint, limit int, since *time.Time) ports.ReadBatch {
	t.Helper()
	batch, err := source.Read(context.Background(), ports.ReadRequest{
		Connection:  conn,
		Checkpoints: checkpoints,
		Limit:       limit,
		Since:       since,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return batch
}

func applyCheckpoints(into map[string]domain.Checkpoint, batch ports.ReadBatch) map[string]domain.Checkpoint {
	for _, checkpoint := range batch.Checkpoints {
		into[checkpoint.Stream] = checkpoint
	}
	return into
}

func cursorOf(t *testing.T, batch ports.ReadBatch, stream string) sourceCursor {
	t.Helper()
	for _, checkpoint := range batch.Checkpoints {
		if checkpoint.Stream != stream {
			continue
		}
		if checkpoint.Version != checkpointVersion {
			t.Fatalf("checkpoint version = %d", checkpoint.Version)
		}
		var state sourceCursor
		if err := json.Unmarshal(checkpoint.Data, &state); err != nil {
			t.Fatalf("decode checkpoint: %v", err)
		}
		return state
	}
	t.Fatalf("no checkpoint for stream %q", stream)
	return sourceCursor{}
}

func streamName(t *testing.T, source *Source, project, kind string) string {
	t.Helper()
	for _, stream := range source.streams() {
		if stream.project == project && stream.kind == kind {
			return stream.name
		}
	}
	t.Fatalf("no stream for %s/%s", project, kind)
	return ""
}

func doneCheckpoints(t *testing.T, source *Source, watermark time.Time) map[string]domain.Checkpoint {
	t.Helper()
	out := make(map[string]domain.Checkpoint)
	for _, stream := range source.streams() {
		out[stream.name] = encodeSourceCursor(stream.name, sourceCursor{Watermark: watermark, Page: 1, Done: true})
	}
	return out
}

func testIssue(iid int, at time.Time) sourceIssue {
	return sourceIssue{
		IID: iid, Title: fmt.Sprintf("Issue %d", iid), Description: fmt.Sprintf("description %d", iid),
		State: "opened", WebURL: fmt.Sprintf("https://gitlab.test/acme/widgets/-/issues/%d", iid),
		Author: author{Username: "alice"}, CreatedAt: at, UpdatedAt: at,
	}
}

func requestedPages(fake *fakeGitLab, suffix string) []int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var pages []int
	for _, request := range fake.requests {
		if !strings.HasSuffix(request.Path, suffix) {
			continue
		}
		values, _ := url.ParseQuery(request.Query)
		pages = append(pages, pageNumber(values))
	}
	return pages
}

// lastQuery returns the query of the last request to an endpoint ending with
// suffix.
func lastQuery(t *testing.T, fake *fakeGitLab, suffix string) url.Values {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for index := len(fake.requests) - 1; index >= 0; index-- {
		if strings.HasSuffix(fake.requests[index].Path, suffix) {
			values, err := url.ParseQuery(fake.requests[index].Query)
			if err != nil {
				t.Fatalf("parse query: %v", err)
			}
			return values
		}
	}
	t.Fatalf("no request to %q", suffix)
	return nil
}

func TestSourceLimitBounds(t *testing.T) {
	cases := map[int]int{
		-1:                  sourceDefaultLimit,
		0:                   sourceDefaultLimit,
		1:                   1,
		sourceMaxLimit:      sourceMaxLimit,
		10 * sourceMaxLimit: sourceMaxLimit,
	}
	for input, want := range cases {
		if got := sourceLimit(input); got != want {
			t.Fatalf("sourceLimit(%d) = %d, want %d", input, got, want)
		}
	}
}

func TestSourceBaseURLAndProjectParsing(t *testing.T) {
	conn := domain.Connection{ID: "gl-test", Provider: Name, Settings: map[string]string{"projects": "acme/widgets"}}
	credentials := mapCredentials{"gl-test/token": sourceTestToken}

	source, err := NewSource(conn, credentials, nil, nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if source.baseURL != "https://gitlab.com/api/v4" {
		t.Fatalf("default baseURL = %q", source.baseURL)
	}

	valid := map[string]string{
		"https://gitlab.example.com/":        "https://gitlab.example.com/api/v4",
		"https://gitlab.example.com/gitlab":  "https://gitlab.example.com/gitlab/api/v4",
		"https://gitlab.example.com/gitlab/": "https://gitlab.example.com/gitlab/api/v4",
		"https://gitlab.example.com/api/v4":  "https://gitlab.example.com/api/v4",
		"https://gitlab.example.com/api/v4/": "https://gitlab.example.com/api/v4",
	}
	for raw, want := range valid {
		conn.BaseURL = raw
		source, err := NewSource(conn, credentials, nil, nil)
		if err != nil {
			t.Fatalf("NewSource(%q): %v", raw, err)
		}
		if source.baseURL != want {
			t.Fatalf("baseURL(%q) = %q, want %q", raw, source.baseURL, want)
		}
	}
	for _, raw := range []string{
		"http://gitlab.example.com",
		"https://user:pass@gitlab.example.com",
		"https://gitlab.example.com#fragment",
		"https://gitlab.example.com?token=leak",
		"ftp://gitlab.example.com",
		"not a url",
	} {
		conn.BaseURL = raw
		if _, err := NewSource(conn, credentials, nil, nil); err == nil || !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("NewSource(%q) error = %v, want a validation error", raw, err)
		}
	}

	conn.BaseURL = ""
	conn.Settings["projects"] = "b/a, a/z\nb/a\r\n"
	source, err = NewSource(conn, credentials, nil, nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if got := strings.Join(source.projects, ","); got != "a/z,b/a" {
		t.Fatalf("projects = %q", got)
	}
	for _, raw := range []string{"", "  ", "bad project", "../etc"} {
		conn.Settings["projects"] = raw
		if _, err := NewSource(conn, credentials, nil, nil); err == nil || !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("projects %q error = %v, want a validation error", raw, err)
		}
	}
}

func TestSourceSelfHostedPathIsUsedForRequests(t *testing.T) {
	fake := newFakeGitLab(t)
	conn := domain.Connection{ID: "gl-test", Projects: []string{"widgets"}}
	source, _ := fake.sourceAt(t, &conn, "acme/widgets", "/gitlab")
	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d", len(fake.requests))
	}
	if got := fake.requests[0].Path; got != "/gitlab/api/v4/user" {
		t.Fatalf("request path = %q, want the self-hosted api path", got)
	}
}

func TestSourceCheckIsDeterministicAndSecretFree(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fake := newFakeGitLab(t)
		source, _ := fake.source(t, &domain.Connection{}, "acme/widgets")
		if err := source.Check(context.Background()); err != nil {
			t.Fatalf("Check: %v", err)
		}
		if got := requestedPages(fake, "/user"); len(got) != 1 {
			t.Fatalf("user requests = %d", len(got))
		}
	})
	t.Run("bad token", func(t *testing.T) {
		fake := newFakeGitLab(t)
		fake.userStatus = http.StatusUnauthorized
		fake.userBody = `{"message":"401 Unauthorized: invalid token ` + sourceTestToken + `"}`
		source, _ := fake.source(t, &domain.Connection{}, "acme/widgets")
		err := source.Check(context.Background())
		if err == nil {
			t.Fatal("Check with a rejected token succeeded")
		}
		if strings.Contains(err.Error(), sourceTestToken) || strings.Contains(err.Error(), "invalid token") {
			t.Fatalf("Check error leaked the response or the token: %v", err)
		}
		if !strings.Contains(err.Error(), "401") {
			t.Fatalf("Check error = %v, want the HTTP status", err)
		}
	})
	t.Run("missing credential", func(t *testing.T) {
		conn := domain.Connection{ID: "gl-test", Provider: Name, Settings: map[string]string{"projects": "acme/widgets"}}
		source, err := NewSource(conn, mapCredentials{}, nil, nil)
		if err != nil {
			t.Fatalf("NewSource: %v", err)
		}
		if err := source.Check(context.Background()); err == nil {
			t.Fatal("Check without a token succeeded")
		} else if strings.Contains(err.Error(), sourceTestToken) {
			t.Fatalf("Check error leaked a token: %v", err)
		}
	})
}

func TestSourceCheckRejectsIncompleteIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":0,"username":""}`))
	}))
	defer server.Close()
	conn := domain.Connection{ID: "gl-test", Provider: Name, BaseURL: server.URL, Settings: map[string]string{"projects": "acme/widgets"}}
	source, err := NewSource(conn, mapCredentials{"gl-test/token": sourceTestToken}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := source.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("Check = %v, want an incomplete identity error", err)
	}
}

func TestSourcePrivateProjectFailsWithoutLeakingTheToken(t *testing.T) {
	fake := newFakeGitLab(t)
	fake.issues = []sourceIssue{testIssue(1, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))}
	fake.failSuffix["/issues"] = http.StatusNotFound
	conn := domain.Connection{ID: "gl-test"}
	source, _ := fake.source(t, &conn, "private/security")
	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check validates the token only: %v", err)
	}
	_, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err == nil {
		t.Fatal("Read of an inaccessible private project succeeded")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("Read error = %v, want the provider status", err)
	}
	if strings.Contains(err.Error(), sourceTestToken) {
		t.Fatalf("Read error leaked the token: %v", err)
	}
}

func TestSourcePageCheckpointAdvancesOnlyAfterReturningItems(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fake := newFakeGitLab(t)
	fake.perPage = 2
	for iid := 1; iid <= 5; iid++ {
		fake.issues = append(fake.issues, testIssue(iid, base.Add(time.Duration(iid)*time.Minute)))
	}
	conn := domain.Connection{ID: "gl-test", Projects: []string{"widgets"}}
	source, _ := fake.source(t, &conn, "acme/widgets")
	stream := streamName(t, source, "acme/widgets", streamKindIssues)

	checkpoints := make(map[string]domain.Checkpoint)
	batch := mustRead(t, source, conn, checkpoints, 2, nil)
	if len(batch.Items) != 2 || !batch.More {
		t.Fatalf("batch 1 = %d items, more=%v", len(batch.Items), batch.More)
	}
	if batch.Items[0].Ref.ExternalID != "acme/widgets#issue-1" || batch.Items[0].ProjectID != "widgets" {
		t.Fatalf("batch 1 item = %+v", batch.Items[0])
	}
	if state := cursorOf(t, batch, stream); state.Page != 2 || state.Offset != 0 || state.Done {
		t.Fatalf("batch 1 cursor = %+v", state)
	}
	values := lastQuery(t, fake, "/issues")
	if values.Get("state") != "all" || values.Get("scope") != "all" ||
		values.Get("order_by") != "updated_at" || values.Get("sort") != "asc" ||
		values.Get("per_page") != strconv.Itoa(sourcePageSize) {
		t.Fatalf("issue query = %q, want a stable ascending full-scope page", values.Encode())
	}
	applyCheckpoints(checkpoints, batch)
	before := checkpoints[stream].Clone()
	batch = mustRead(t, source, conn, checkpoints, 2, nil)
	if !checkpoints[stream].Equal(before) {
		t.Fatalf("Read mutated the request checkpoint map")
	}
	if len(batch.Items) != 2 || batch.Items[0].Ref.ExternalID != "acme/widgets#issue-3" {
		t.Fatalf("batch 2 items = %+v", batch.Items)
	}
	if state := cursorOf(t, batch, stream); state.Page != 3 || state.Offset != 0 || state.Done {
		t.Fatalf("batch 2 cursor = %+v", state)
	}
	applyCheckpoints(checkpoints, batch)

	batch = mustRead(t, source, conn, checkpoints, 2, nil)
	if len(batch.Items) != 1 || batch.More {
		t.Fatalf("batch 3 = %d items, more=%v; want the pass to finish", len(batch.Items), batch.More)
	}
	state := cursorOf(t, batch, stream)
	if !state.Done || state.Watermark != base.Add(5*time.Minute) {
		t.Fatalf("batch 3 cursor = %+v", state)
	}
	if pages := requestedPages(fake, "/issues"); !slices.Equal(pages, []int{1, 2, 3}) {
		t.Fatalf("issue pages requested = %v, want 1,2,3 with no re-read of finished pages", pages)
	}
	if batch.Items[0].UpdatedAt != base.Add(5*time.Minute) || batch.Items[0].Kind != domain.ItemIssue {
		t.Fatalf("item = %+v", batch.Items[0])
	}
}

// TestSourceCheckpointKeepsOffsetForUnreturnedItems proves that a page
// position only advances once every item of the page was returned: a limit
// that cuts inside a page stores the offset, not the next page.
func TestSourcePageShiftDoesNotSkipRecords(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fake := newFakeGitLab(t)
	fake.perPage = 2
	for iid := 1; iid <= 4; iid++ {
		fake.issues = append(fake.issues, testIssue(iid, base.Add(time.Duration(iid)*time.Minute)))
	}
	conn := domain.Connection{ID: "gl-test"}
	source, _ := fake.source(t, &conn, "acme/widgets")

	checkpoints := make(map[string]domain.Checkpoint)
	first := mustRead(t, source, conn, checkpoints, 1, nil)
	if len(first.Items) != 1 || first.Items[0].Ref.ExternalID != "acme/widgets#issue-1" {
		t.Fatalf("first batch = %+v", first.Items)
	}
	applyCheckpoints(checkpoints, first)

	// Insert a new issue before the saved offset. The numbered page shifts;
	// a stale offset would skip one of the later issues.
	fake.mu.Lock()
	fake.issues = append([]sourceIssue{testIssue(9, base.Add(9*time.Minute))}, fake.issues...)
	fake.mu.Unlock()

	seen := map[string]bool{"acme/widgets#issue-1": true}
	for reads := 0; reads < 10; reads++ {
		batch := mustRead(t, source, conn, checkpoints, 10, nil)
		for _, item := range batch.Items {
			seen[item.Ref.ExternalID] = true
		}
		applyCheckpoints(checkpoints, batch)
		if !batch.More {
			break
		}
	}
	for _, id := range []string{"acme/widgets#issue-2", "acme/widgets#issue-3", "acme/widgets#issue-4", "acme/widgets#issue-9"} {
		if !seen[id] {
			t.Fatalf("record %s was skipped after a page shift; seen=%v", id, seen)
		}
	}
}

func TestSourceCheckpointKeepsOffsetForUnreturnedItems(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fake := newFakeGitLab(t)
	fake.perPage = 2
	for iid := 1; iid <= 4; iid++ {
		fake.issues = append(fake.issues, testIssue(iid, base.Add(time.Duration(iid)*time.Minute)))
	}
	conn := domain.Connection{ID: "gl-test"}
	source, _ := fake.source(t, &conn, "acme/widgets")
	stream := streamName(t, source, "acme/widgets", streamKindIssues)

	checkpoints := make(map[string]domain.Checkpoint)
	want := []sourceCursor{
		{Page: 1, Offset: 1}, // one item of page 1 returned, one remains
		{Page: 2, Offset: 0}, // page 1 fully returned, page 2 pending
		{Page: 2, Offset: 1}, // page 2 partially returned
		{Page: 2, Offset: 0}, // page 2 fully returned, stream done
	}
	for index, expected := range want {
		batch := mustRead(t, source, conn, checkpoints, 1, nil)
		if len(batch.Items) != 1 {
			t.Fatalf("batch %d items = %d", index+1, len(batch.Items))
		}
		state := cursorOf(t, batch, stream)
		if state.Done != (index == len(want)-1) {
			t.Fatalf("batch %d done = %v", index+1, state.Done)
		}
		if !state.Done && (state.Page != expected.Page || state.Offset != expected.Offset) {
			t.Fatalf("batch %d cursor = %+v, want page %d offset %d", index+1, state, expected.Page, expected.Offset)
		}
		if !batch.More {
			t.Fatalf("batch %d More = false while the empty MR/commit streams are still pending", index+1)
		}
		applyCheckpoints(checkpoints, batch)
	}
	// The next Read drains the two empty streams and finishes the pass.
	batch := mustRead(t, source, conn, checkpoints, 1, nil)
	if len(batch.Items) != 0 || batch.More {
		t.Fatalf("final batch = %d items, more=%v; want a finished pass", len(batch.Items), batch.More)
	}
	if pages := requestedPages(fake, "/issues"); !slices.Equal(pages, []int{1, 1, 2, 2}) {
		t.Fatalf("issue pages = %v, want each page fetched only while its offset is pending", pages)
	}
}

func TestSourceMoreTracksPendingPages(t *testing.T) {
	fake := newFakeGitLab(t)
	conn := domain.Connection{ID: "gl-test"}
	source, _ := fake.source(t, &conn, "acme/widgets")
	checkpoints := make(map[string]domain.Checkpoint)

	batch := mustRead(t, source, conn, checkpoints, 10, nil)
	if len(batch.Items) != 0 || batch.More {
		t.Fatalf("empty backfill = %d items, more=%v", len(batch.Items), batch.More)
	}
	if len(batch.Checkpoints) != 3 {
		t.Fatalf("checkpoints = %d, want one per stream", len(batch.Checkpoints))
	}
	for _, checkpoint := range batch.Checkpoints {
		state := cursorOf(t, batch, checkpoint.Stream)
		if !state.Done {
			t.Fatalf("stream %q not done after an empty pass: %+v", checkpoint.Stream, state)
		}
	}
	applyCheckpoints(checkpoints, batch)

	// With every stream done, the next Read starts a new pass and must pick
	// up the item that appeared meanwhile.
	fake.mu.Lock()
	fake.issues = append(fake.issues, testIssue(1, time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)))
	fake.mu.Unlock()
	batch = mustRead(t, source, conn, checkpoints, 10, nil)
	if len(batch.Items) != 1 || batch.Items[0].Ref.ExternalID != "acme/widgets#issue-1" {
		t.Fatalf("second pass items = %+v", batch.Items)
	}
	if batch.More {
		t.Fatalf("More = true after the second pass finished")
	}
}

func TestSourceNewItemsBetweenPagesAreNotLost(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	fake := newFakeGitLab(t)
	fake.perPage = 1
	for iid := 1; iid <= 3; iid++ {
		fake.issues = append(fake.issues, testIssue(iid, base.Add(time.Duration(iid)*time.Minute)))
	}
	conn := domain.Connection{ID: "gl-test"}
	source, _ := fake.source(t, &conn, "acme/widgets")
	checkpoints := make(map[string]domain.Checkpoint)

	seen := make(map[string]bool)
	batches := 0
	for {
		batches++
		if batches > 20 {
			t.Fatal("More never cleared")
		}
		if batches == 2 {
			// A new item appears between pages: ascending order appends it.
			fake.mu.Lock()
			fake.issues = append(fake.issues, testIssue(4, base.Add(4*time.Minute)))
			fake.mu.Unlock()
		}
		batch := mustRead(t, source, conn, checkpoints, 1, nil)
		for _, item := range batch.Items {
			if seen[item.Key()] {
				t.Fatalf("duplicate item %q", item.Ref.ExternalID)
			}
			seen[item.Key()] = true
		}
		applyCheckpoints(checkpoints, batch)
		if !batch.More {
			break
		}
	}
	for iid := 1; iid <= 4; iid++ {
		externalID := fmt.Sprintf("acme/widgets#issue-%d", iid)
		if !seen[domain.ItemRef{ConnectionID: "gl-test", ExternalID: externalID}.Key()] {
			t.Fatalf("issue %d was lost across pages (seen=%v)", iid, seen)
		}
	}
}

func TestSourceSinceIsInclusiveAndWatermarkBoundsTheNextScan(t *testing.T) {
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	t.Run("since", func(t *testing.T) {
		fake := newFakeGitLab(t)
		fake.issues = []sourceIssue{testIssue(1, base.Add(-time.Second)), testIssue(2, base), testIssue(3, base.Add(time.Hour))}
		conn := domain.Connection{ID: "gl-test"}
		source, _ := fake.source(t, &conn, "acme/widgets")
		since := base
		batch := mustRead(t, source, conn, map[string]domain.Checkpoint{}, 10, &since)
		if len(batch.Items) != 2 || batch.Items[0].Ref.ExternalID != "acme/widgets#issue-2" {
			t.Fatalf("items = %+v, want items at or after Since (issue-1 is one second older)", batch.Items)
		}
		values := lastQuery(t, fake, "/issues")
		want := base.Add(-boundSlack).Format(checkpointTimeFormat)
		if got := values.Get("updated_after"); got != want {
			t.Fatalf("updated_after = %q, want %q (inclusive bound)", got, want)
		}
	})
	t.Run("watermark overlap", func(t *testing.T) {
		watermark := base.Add(time.Hour)
		fake := newFakeGitLab(t)
		fake.issues = []sourceIssue{
			testIssue(1, watermark.Add(-10*time.Minute)), // older than the overlap
			testIssue(2, watermark.Add(time.Second)),     // new update after the checkpoint
		}
		conn := domain.Connection{ID: "gl-test"}
		source, _ := fake.source(t, &conn, "acme/widgets")
		batch := mustRead(t, source, conn, doneCheckpoints(t, source, watermark), 10, nil)
		if len(batch.Items) != 1 || batch.Items[0].Ref.ExternalID != "acme/widgets#issue-2" {
			t.Fatalf("items = %+v, want only the item after the overlap window", batch.Items)
		}
		values := lastQuery(t, fake, "/issues")
		want := watermark.Add(-pageOverlap - boundSlack).Format(checkpointTimeFormat)
		if got := values.Get("updated_after"); got != want {
			t.Fatalf("updated_after = %q, want %q", got, want)
		}
		state := cursorOf(t, batch, streamName(t, source, "acme/widgets", streamKindIssues))
		if state.Watermark != watermark.Add(time.Second) {
			t.Fatalf("watermark = %v, want the newest returned update", state.Watermark)
		}
	})
}

func TestSourceNotesAreBoundedAndRenderedChronologically(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fake := newFakeGitLab(t)
	fake.issues = []sourceIssue{testIssue(1, base), testIssue(2, base.Add(time.Minute))}
	for i := 0; i < 7; i++ {
		note := sourceNote{
			ID:        101 + i,
			Body:      fmt.Sprintf("comment %d", 101+i),
			Author:    author{Username: "bob"},
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
			UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if i == 6 {
			note.UpdatedAt = note.CreatedAt.Add(time.Minute)
		}
		fake.notes["issues:1"] = append(fake.notes["issues:1"], note)
	}
	fake.notes["issues:1"] = append(fake.notes["issues:1"], sourceNote{
		ID: 200, Body: "system note", Author: author{Username: "gitlab"}, CreatedAt: base.Add(time.Second), System: true,
	})
	conn := domain.Connection{ID: "gl-test"}
	source, _ := fake.source(t, &conn, "acme/widgets")
	batch := mustRead(t, source, conn, map[string]domain.Checkpoint{}, 1, nil)
	if len(batch.Items) != 1 {
		t.Fatalf("items = %d, want only the limited issue", len(batch.Items))
	}
	fake.mu.Lock()
	noteRequests := fake.noteRequests
	fake.mu.Unlock()
	if noteRequests != 1 {
		t.Fatalf("note requests = %d, want only the returned item", noteRequests)
	}
	values := lastQuery(t, fake, "/notes")
	if values.Get("per_page") != strconv.Itoa(maxNotesPerItem) || values.Get("page") != "1" || values.Get("sort") != "desc" {
		t.Fatalf("notes query = %q, want one bounded newest-first page", values.Encode())
	}
	body := batch.Items[0].Body
	if !utf8.ValidString(body) || len(body) > maxSourceBodyBytes {
		t.Fatalf("body is not valid bounded UTF-8 (%d bytes)", len(body))
	}
	if got := strings.Count(body, "\n- **"); got != maxNotesPerItem {
		t.Fatalf("rendered comments = %d, want %d", got, maxNotesPerItem)
	}
	if !strings.Contains(body, "note 107") || !strings.Contains(body, "edited ") {
		t.Fatalf("body = %q, want the newest comment id and the edit timestamp", body)
	}
	if strings.Contains(body, "note 101") || strings.Contains(body, "note 102") {
		t.Fatalf("body = %q, want the newest five comments only", body)
	}
	if strings.Contains(body, "system note") {
		t.Fatalf("body = %q, want system notes skipped", body)
	}
	last := -1
	for i := 103; i <= 107; i++ {
		index := strings.Index(body, fmt.Sprintf("note %d", i))
		if index < 0 || index <= last {
			t.Fatalf("comments are not in chronological order: %q", body)
		}
		last = index
	}
}

func TestSourceNotesLocalCapEvenIfProviderIgnoresPerPage(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fake := newFakeGitLab(t)
	fake.ignoreNoteLimit = true
	fake.issues = []sourceIssue{testIssue(1, base)}
	for i := 0; i < 20; i++ {
		fake.notes["issues:1"] = append(fake.notes["issues:1"], sourceNote{
			ID: 300 + i, Body: fmt.Sprintf("comment %d", 300+i), Author: author{Username: "bob"}, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	conn := domain.Connection{ID: "gl-test"}
	source, _ := fake.source(t, &conn, "acme/widgets")
	batch := mustRead(t, source, conn, map[string]domain.Checkpoint{}, 1, nil)
	if got := strings.Count(batch.Items[0].Body, "\n- **"); got != maxNotesPerItem {
		t.Fatalf("rendered comments = %d, want the local cap", got)
	}
}

func TestSourceNotesCombinedBodyCapKeepsValidUTF8(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fake := newFakeGitLab(t)
	record := testIssue(1, base)
	record.Description = "a" + strings.Repeat("é", 300<<10)
	fake.issues = []sourceIssue{record}
	for i := 0; i < maxNotesPerItem; i++ {
		fake.notes["issues:1"] = append(fake.notes["issues:1"], sourceNote{
			ID: 400 + i, Body: strings.Repeat("ü", 20<<10), Author: author{Username: "bob"}, CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	conn := domain.Connection{ID: "gl-test"}
	source, _ := fake.source(t, &conn, "acme/widgets")
	batch := mustRead(t, source, conn, map[string]domain.Checkpoint{}, 1, nil)
	body := batch.Items[0].Body
	if len(body) > maxSourceBodyBytes {
		t.Fatalf("body = %d bytes, want at most %d", len(body), maxSourceBodyBytes)
	}
	if !utf8.ValidString(body) {
		t.Fatalf("truncated body is not valid UTF-8")
	}
	if err := batch.Items[0].WithHash().Validate(); err != nil {
		t.Fatalf("item does not validate after truncation: %v", err)
	}
}

func TestSourcePaginationHeaders(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	newSource := func(t *testing.T, mode paginationMode, perPage int) (*Source, domain.Connection, *fakeGitLab) {
		t.Helper()
		fake := newFakeGitLab(t)
		fake.pagination = mode
		fake.perPage = perPage
		fake.issues = []sourceIssue{testIssue(1, base), testIssue(2, base.Add(time.Minute))}
		conn := domain.Connection{ID: "gl-test"}
		source, _ := fake.source(t, &conn, "acme/widgets")
		return source, conn, fake
	}

	t.Run("X-Next-Page", func(t *testing.T) {
		source, conn, fake := newSource(t, paginationHeader, 1)
		batch := mustRead(t, source, conn, map[string]domain.Checkpoint{}, 10, nil)
		if len(batch.Items) != 2 || batch.More {
			t.Fatalf("items = %d, more=%v", len(batch.Items), batch.More)
		}
		if pages := requestedPages(fake, "/issues"); len(pages) != 2 {
			t.Fatalf("issue pages = %v", pages)
		}
	})
	t.Run("no headers ends the stream", func(t *testing.T) {
		source, conn, _ := newSource(t, paginationNone, 1)
		batch := mustRead(t, source, conn, map[string]domain.Checkpoint{}, 10, nil)
		if len(batch.Items) != 1 || batch.More {
			t.Fatalf("items = %d, more=%v; absent headers must end the stream, not loop", len(batch.Items), batch.More)
		}
	})
	t.Run("empty header ends a full last page", func(t *testing.T) {
		fake := newFakeGitLab(t)
		fake.pagination = paginationHeader
		fake.perPage = sourcePageSize
		for iid := 1; iid <= sourcePageSize; iid++ {
			fake.issues = append(fake.issues, testIssue(iid, base.Add(time.Duration(iid)*time.Second)))
		}
		conn := domain.Connection{ID: "gl-test"}
		source, _ := fake.source(t, &conn, "acme/widgets")
		checkpoints := make(map[string]domain.Checkpoint)
		batch := mustRead(t, source, conn, checkpoints, sourcePageSize, nil)
		if len(batch.Items) != sourcePageSize {
			t.Fatalf("items = %d", len(batch.Items))
		}
		state := cursorOf(t, batch, streamName(t, source, "acme/widgets", streamKindIssues))
		if !state.Done {
			t.Fatalf("issues cursor = %+v; an explicit empty X-Next-Page must end the stream", state)
		}
		applyCheckpoints(checkpoints, batch)
		// More is still true only because the MR and commit streams were
		// never reached; the next batch drains them.
		if batch.More {
			if final := mustRead(t, source, conn, checkpoints, sourcePageSize, nil); len(final.Items) != 0 || final.More {
				t.Fatalf("final batch = %d items, more=%v", len(final.Items), final.More)
			}
		}
	})
	t.Run("full page without headers is an error", func(t *testing.T) {
		fake := newFakeGitLab(t)
		fake.pagination = paginationNone
		fake.perPage = sourcePageSize
		for iid := 1; iid <= sourcePageSize; iid++ {
			fake.issues = append(fake.issues, testIssue(iid, base.Add(time.Duration(iid)*time.Second)))
		}
		conn := domain.Connection{ID: "gl-test"}
		source, _ := fake.source(t, &conn, "acme/widgets")
		_, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: sourcePageSize})
		if err == nil || !strings.Contains(err.Error(), "pagination headers are missing") {
			t.Fatalf("Read = %v, want a missing-pagination error instead of a silent truncation", err)
		}
	})
	t.Run("malformed header", func(t *testing.T) {
		source, conn, _ := newSource(t, paginationMalformed, 1)
		_, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
		if err == nil || !strings.Contains(err.Error(), "X-Next-Page") {
			t.Fatalf("Read = %v, want a malformed header error", err)
		}
	})
	t.Run("non-monotonic header", func(t *testing.T) {
		source, conn, _ := newSource(t, paginationNonMonotonic, 1)
		_, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
		if err == nil || !strings.Contains(err.Error(), "does not advance") {
			t.Fatalf("Read = %v, want a non-monotonic pagination error", err)
		}
	})
	t.Run("Link rel next", func(t *testing.T) {
		source, conn, _ := newSource(t, paginationLink, 1)
		batch := mustRead(t, source, conn, map[string]domain.Checkpoint{}, 10, nil)
		if len(batch.Items) != 2 || batch.More {
			t.Fatalf("items = %d, more=%v", len(batch.Items), batch.More)
		}
	})
	t.Run("foreign Link is never followed", func(t *testing.T) {
		source, conn, fake := newSource(t, paginationForeignLink, 1)
		batch := mustRead(t, source, conn, map[string]domain.Checkpoint{}, 10, nil)
		if len(batch.Items) != 1 || batch.More {
			t.Fatalf("items = %d, more=%v; a foreign next link must end the stream", len(batch.Items), batch.More)
		}
		if pages := requestedPages(fake, "/issues"); !slices.Equal(pages, []int{1}) {
			t.Fatalf("issue pages = %v, want a single page because the only next link was foreign", pages)
		}
		// The token may only go to the configured host; a foreign Link target
		// is never contacted, which is why no request to evil.example exists.
	})
}

func TestSourceRejectsInvalidCheckpoints(t *testing.T) {
	conn := domain.Connection{ID: "gl-test"}
	fake := newFakeGitLab(t)
	source, _ := fake.source(t, &conn, "acme/widgets")
	stream := streamName(t, source, "acme/widgets", streamKindIssues)

	encode := func(state any) domain.Checkpoint {
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return domain.Checkpoint{Stream: stream, Version: checkpointVersion, Data: data}
	}
	cases := map[string]domain.Checkpoint{
		"wrong version":      {Stream: stream, Version: checkpointVersion + 1, Data: []byte(`{"page":1}`)},
		"wrong stream":       {Stream: "other-stream", Version: checkpointVersion, Data: []byte(`{"page":1}`)},
		"invalid json":       {Stream: stream, Version: checkpointVersion, Data: []byte(`{`)},
		"zero page":          encode(sourceCursor{Page: 0}),
		"negative offset":    encode(sourceCursor{Page: 1, Offset: -1}),
		"offset beyond page": encode(sourceCursor{Page: 1, Offset: sourcePageSize + 1}),
		"page beyond max":    encode(sourceCursor{Page: sourceMaxPagesPerStream + 1}),
	}
	for name, checkpoint := range cases {
		t.Run(name, func(t *testing.T) {
			checkpoints := map[string]domain.Checkpoint{stream: checkpoint}
			_, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints, Limit: 10})
			if err == nil || !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Read = %v, want a validation error", err)
			}
		})
	}

	t.Run("unknown stream is ignored", func(t *testing.T) {
		checkpoints := map[string]domain.Checkpoint{"p-stale-issues": encode(sourceCursor{Page: 1})}
		if _, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints, Limit: 10}); err != nil {
			t.Fatalf("stale checkpoint stream must be ignored, got %v", err)
		}
	})
	t.Run("connection mismatch", func(t *testing.T) {
		for _, request := range []ports.ReadRequest{
			{Connection: domain.Connection{ID: "other", Provider: Name}, Limit: 10},
			{Connection: domain.Connection{ID: "gl-test", Provider: "github"}, Limit: 10},
		} {
			if _, err := source.Read(context.Background(), request); err == nil || !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Read(%+v) = %v, want a validation error", request.Connection, err)
			}
		}
	})
}

func TestSourceStreamsMatchDescriptorAndKinds(t *testing.T) {
	descriptor := Descriptor()
	if descriptor.Provider != Name || descriptor.DisplayName != "GitLab" {
		t.Fatalf("descriptor = %+v", descriptor)
	}
	if !descriptor.Capabilities.Backfill || !descriptor.Capabilities.Incremental || descriptor.Capabilities.Deletes {
		t.Fatalf("capabilities = %+v", descriptor.Capabilities)
	}
	if len(descriptor.Capabilities.AuthModes) != 1 || descriptor.Capabilities.AuthModes[0] != domain.AuthToken {
		t.Fatalf("auth modes = %v", descriptor.Capabilities.AuthModes)
	}
	if descriptor.DefaultPublish != domain.PublishShared {
		t.Fatalf("default publish = %q", descriptor.DefaultPublish)
	}
	if len(descriptor.Streams) != 0 {
		t.Fatalf("descriptor streams = %v; GitLab streams are per-project and dynamic", descriptor.Streams)
	}
	if !descriptor.SupportsBackend(domain.BackendNative) || descriptor.SupportsBackend(domain.BackendMCP) {
		t.Fatalf("backends = %v", descriptor.Backends)
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatalf("descriptor invalid: %v", err)
	}

	fake := newFakeGitLab(t)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fake.issues = []sourceIssue{testIssue(1, base)}
	fake.mergeRequests = []sourceMergeRequest{{
		IID: 2, Title: "MR", Description: "mr", WebURL: "https://gitlab.test/mr", Author: author{Username: "bob"},
		CreatedAt: base, UpdatedAt: base,
	}}
	fake.commits = []sourceCommit{{ID: "sha1", Title: "commit", Message: "commit", CommittedDate: base, AuthoredDate: base}}
	conn := domain.Connection{ID: "gl-test", Projects: []string{"alpha", "beta"}}
	source, _ := fake.source(t, &conn, "acme/widgets, other/repo")

	streams := source.streams()
	if len(streams) != 6 {
		t.Fatalf("streams = %d, want 3 per project", len(streams))
	}
	seen := make(map[string]bool)
	kindsByProject := make(map[string]map[string]bool)
	for _, stream := range streams {
		if seen[stream.name] {
			t.Fatalf("duplicate stream %q", stream.name)
		}
		seen[stream.name] = true
		if err := domain.ValidateStream("stream", stream.name); err != nil {
			t.Fatalf("stream name %q is invalid: %v", stream.name, err)
		}
		if kindsByProject[stream.project] == nil {
			kindsByProject[stream.project] = make(map[string]bool)
		}
		kindsByProject[stream.project][stream.kind] = true
	}
	for _, project := range []string{"acme/widgets", "other/repo"} {
		for _, kind := range []string{streamKindIssues, streamKindMergeRequests, streamKindCommits} {
			if !kindsByProject[project][kind] {
				t.Fatalf("project %q misses stream kind %q", project, kind)
			}
		}
	}

	// Two brain projects: the mapping is ambiguous, so items carry no
	// ProjectID.
	batch := mustRead(t, source, conn, map[string]domain.Checkpoint{}, 10, nil)
	if len(batch.Items) != 6 {
		t.Fatalf("items = %d, want one issue, MR and commit per project", len(batch.Items))
	}
	kinds := make(map[domain.ItemKind]int)
	for _, item := range batch.Items {
		kinds[item.Kind]++
		if item.ProjectID != "" {
			t.Fatalf("item %q carries ProjectID %q with several connection projects", item.Ref.ExternalID, item.ProjectID)
		}
	}
	for _, kind := range []domain.ItemKind{domain.ItemIssue, domain.ItemMergeRequest, domain.ItemCommit} {
		if kinds[kind] != 2 {
			t.Fatalf("kind %q count = %d, want 2", kind, kinds[kind])
		}
	}

	// One brain project: every item maps to it.
	one := domain.Connection{ID: "gl-test", Projects: []string{"alpha"}}
	oneSource, _ := fake.source(t, &one, "acme/widgets")
	batch = mustRead(t, oneSource, one, map[string]domain.Checkpoint{}, 10, nil)
	if len(batch.Items) != 3 {
		t.Fatalf("items = %d", len(batch.Items))
	}
	for _, item := range batch.Items {
		if item.ProjectID != "alpha" {
			t.Fatalf("item %q ProjectID = %q, want alpha", item.Ref.ExternalID, item.ProjectID)
		}
	}
}

func TestSourceConnectionIsolation(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fake := newFakeGitLab(t)
	fake.pagination = paginationNone
	fake.issuesByPath = map[string][]sourceIssue{
		"private/security": {{IID: 1, Title: "secret design", Description: "secret body", WebURL: "https://gitlab.test/private", CreatedAt: base, UpdatedAt: base}},
		"public/app":       {{IID: 2, Title: "public change", Description: "public body", WebURL: "https://gitlab.test/public", CreatedAt: base, UpdatedAt: base}},
	}

	alpha := domain.Connection{ID: "gl-alpha", Projects: []string{"alpha"}}
	beta := domain.Connection{ID: "gl-beta", Projects: []string{"beta"}}
	alphaSource, _ := fake.source(t, &alpha, "private/security")
	betaSource, _ := fake.source(t, &beta, "public/app")

	alphaBatch := mustRead(t, alphaSource, alpha, map[string]domain.Checkpoint{}, 10, nil)
	if len(alphaBatch.Items) != 1 {
		t.Fatalf("alpha items = %d", len(alphaBatch.Items))
	}
	alphaItem := alphaBatch.Items[0]
	if alphaItem.Ref.ConnectionID != "gl-alpha" || alphaItem.ProjectID != "alpha" ||
		!strings.HasPrefix(alphaItem.Ref.ExternalID, "private/security#") || !strings.Contains(alphaItem.Body, "secret body") {
		t.Fatalf("alpha item = %+v", alphaItem)
	}

	betaBatch := mustRead(t, betaSource, beta, map[string]domain.Checkpoint{}, 10, nil)
	if len(betaBatch.Items) != 1 {
		t.Fatalf("beta items = %d", len(betaBatch.Items))
	}
	betaItem := betaBatch.Items[0]
	if betaItem.Ref.ConnectionID != "gl-beta" || betaItem.ProjectID != "beta" ||
		!strings.HasPrefix(betaItem.Ref.ExternalID, "public/app#") || strings.Contains(betaItem.Body, "secret") {
		t.Fatalf("beta item = %+v", betaItem)
	}

	// Checkpoints are per connection: alpha's cursor must not position beta.
	alphaCheckpoints := applyCheckpoints(make(map[string]domain.Checkpoint), alphaBatch)
	betaBatch = mustRead(t, betaSource, beta, alphaCheckpoints, 10, nil)
	if len(betaBatch.Items) != 1 || betaBatch.Items[0].Ref.ConnectionID != "gl-beta" {
		t.Fatalf("beta read with alpha checkpoints = %+v", betaBatch.Items)
	}

	// A source refuses a foreign connection outright.
	if _, err := alphaSource.Read(context.Background(), ports.ReadRequest{Connection: beta, Limit: 10}); err == nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("cross-connection read = %v, want a validation error", err)
	}

	// The private project was read exactly once (by the alpha source); beta
	// never requested it.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	privateRequests, publicRequests := 0, 0
	for _, request := range fake.requests {
		if !strings.HasSuffix(request.Path, "/issues") {
			continue
		}
		if strings.Contains(request.Path, "private/security") {
			privateRequests++
		}
		if strings.Contains(request.Path, "public/app") {
			publicRequests++
		}
	}
	if privateRequests != 1 || publicRequests != 2 {
		t.Fatalf("issue requests: private=%d public=%d; each connection must read only its own project", privateRequests, publicRequests)
	}
}

func TestSourceTokenNeverAppearsInRequestsOrErrors(t *testing.T) {
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	fake := newFakeGitLab(t)
	fake.issues = []sourceIssue{testIssue(1, base)}
	fake.notes["issues:1"] = []sourceNote{{ID: 1, Body: "note", Author: author{Username: "bob"}, CreatedAt: base}}
	fake.mergeRequests = []sourceMergeRequest{{IID: 1, Title: "MR", WebURL: "https://gitlab.test/mr", CreatedAt: base, UpdatedAt: base}}
	fake.commits = []sourceCommit{{ID: "sha", Title: "commit", Message: "message", CommittedDate: base, AuthoredDate: base}}
	conn := domain.Connection{ID: "gl-test"}
	source, _ := fake.source(t, &conn, "acme/widgets")

	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	checkpoints := make(map[string]domain.Checkpoint)
	for {
		batch := mustRead(t, source, conn, checkpoints, 10, nil)
		applyCheckpoints(checkpoints, batch)
		if !batch.More {
			break
		}
	}

	fake.mu.Lock()
	requests := append([]fakeRequest(nil), fake.requests...)
	fake.mu.Unlock()
	if len(requests) == 0 {
		t.Fatal("no requests recorded")
	}
	for _, request := range requests {
		if strings.Contains(request.Path, sourceTestToken) || strings.Contains(request.Query, sourceTestToken) {
			t.Fatalf("token leaked into the request URL: %s?%s", request.Path, request.Query)
		}
		if request.Token != sourceTestToken {
			t.Fatalf("PRIVATE-TOKEN header = %q", request.Token)
		}
	}

	// A failing request must not echo the response body (which contains the
	// token) either.
	fake.mu.Lock()
	fake.failSuffix["/merge_requests"] = http.StatusUnauthorized
	fake.mu.Unlock()
	_, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpoints, Limit: 10})
	if err == nil {
		t.Fatal("Read with a rejected token succeeded")
	}
	if strings.Contains(err.Error(), sourceTestToken) {
		t.Fatalf("Read error leaked the token: %v", err)
	}
}
