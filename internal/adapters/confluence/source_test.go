package confluence

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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

const (
	testToken = "confluence-test-token"
	testEmail = "tester@example.test"
	badToken  = "confluence-wrong-token"
)

type testCredentials map[string]string

func (c testCredentials) Get(_ context.Context, connectionID, field string) (string, error) {
	if field != "token" {
		return "", domain.ErrNotFound
	}
	token, ok := c[connectionID]
	if !ok || token == "" {
		return "", domain.ErrNotFound
	}
	return token, nil
}

var _ ports.CredentialProvider = testCredentials{}

// fakeConfluence is an httptest upstream used by both the focused tests and the
// reusable ingestion contract suite.
type fakeConfluence struct {
	mu         sync.Mutex
	deployment string
	capAt      int
	status     int
	pages      []searchResult
	comments   map[string][]searchResult
	clock      time.Time
	nextID     int

	lastAuthorization string
	lastCQL           string
	lastQuery         url.Values
	searchStarts      []int
	requestURLs       []string
}

func newFakeConfluence(deployment string) *fakeConfluence {
	return &fakeConfluence{
		deployment: deployment,
		capAt:      50,
		comments:   make(map[string][]searchResult),
		clock:      time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (f *fakeConfluence) addPage(title, storage, space string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addPageLocked(title, storage, space, f.clock.Add(time.Second))
}

func (f *fakeConfluence) addPageAt(title, storage, space string, at time.Time) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addPageLocked(title, storage, space, at)
}

func (f *fakeConfluence) addPageLocked(title, storage, space string, at time.Time) string {
	f.nextID++
	id := strconv.Itoa(1000 + f.nextID)
	if at.After(f.clock) {
		f.clock = at
	}
	page := searchResult{
		ID:     id,
		Type:   "page",
		Status: "current",
		Title:  title,
		Version: struct {
			Number int       `json:"number"`
			When   time.Time `json:"when"`
			By     versionBy `json:"by"`
		}{Number: 1, When: at, By: versionBy{DisplayName: "Alice", Username: "alice", AccountID: "acc-alice"}},
	}
	page.Body.Storage.Value = storage
	page.Space.Key = space
	page.Links.WebUI = "/spaces/" + space + "/pages/" + id
	f.pages = append(f.pages, page)
	return id
}

func (f *fakeConfluence) addComment(pageID, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := "c-" + strconv.Itoa(len(f.comments[pageID])+1)
	comment := searchResult{
		ID:   id,
		Type: "comment",
		Version: struct {
			Number int       `json:"number"`
			When   time.Time `json:"when"`
			By     versionBy `json:"by"`
		}{Number: 1, When: at, By: versionBy{DisplayName: "Bob", Username: "bob"}},
	}
	comment.Body.Storage.Value = body
	f.comments[pageID] = append(f.comments[pageID], comment)
}

func (f *fakeConfluence) updateFirstPage() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pages) == 0 {
		return
	}
	f.clock = f.clock.Add(time.Second)
	f.pages[0].Title += " updated"
	f.pages[0].Body.Storage.Value += "<p>revised</p>"
	f.pages[0].Version.When = f.clock
	f.pages[0].Version.Number++
}

func (f *fakeConfluence) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeConfluence) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requestURLs = append(f.requestURLs, r.URL.String())
	status := f.status
	auth := r.Header.Get("Authorization")
	f.lastAuthorization = auth
	want := f.expectedAuthorizationLocked()
	deployment := f.deployment
	f.mu.Unlock()

	if status != 0 {
		http.Error(w, "upstream failure", status)
		return
	}
	if auth != want {
		w.Header().Set("WWW-Authenticate", `Basic realm="confluence"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/user/current"):
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"username":"tester","accountId":"acc-tester","displayName":"Tester"}`))
	case strings.HasSuffix(r.URL.Path, "/content/search"):
		f.handleSearch(w, r, deployment)
	case strings.Contains(r.URL.Path, "/child/comment"):
		f.handleComments(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeConfluence) expectedAuthorizationLocked() string {
	if f.deployment == deploymentCloud {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(testEmail+":"+testToken))
	}
	return "Bearer " + testToken
}

func (f *fakeConfluence) handleSearch(w http.ResponseWriter, r *http.Request, deployment string) {
	query := r.URL.Query()
	start := atoiDefault(query.Get("start"), 0)
	limit := atoiDefault(query.Get("limit"), providerPageSize)
	cql := query.Get("cql")

	f.mu.Lock()
	pages := append([]searchResult(nil), f.pages...)
	capAt := f.capAt
	f.lastCQL = cql
	f.lastQuery = query
	f.searchStarts = append(f.searchStarts, start)
	f.mu.Unlock()

	filtered := filterSearch(pages, cql)
	base := requestSiteBase(r, deployment)
	if deployment == deploymentCloud && capAt > 0 && limit > capAt {
		limit = capAt
	}
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	results := append([]searchResult(nil), filtered[start:end]...)
	for index := range results {
		results[index].Links.Base = base
	}
	writeSearchResponse(w, searchResponse{
		Results:   results,
		Start:     start,
		Limit:     limit,
		Size:      len(results),
		TotalSize: len(filtered),
		Links: struct {
			Base string `json:"base"`
			Next string `json:"next"`
		}{Base: base},
	})
}

func (f *fakeConfluence) handleComments(w http.ResponseWriter, r *http.Request) {
	pageID := commentPageID(r.URL.Path)
	query := r.URL.Query()
	start := atoiDefault(query.Get("start"), 0)
	limit := atoiDefault(query.Get("limit"), providerPageSize)

	f.mu.Lock()
	comments := append([]searchResult(nil), f.comments[pageID]...)
	deployment := f.deployment
	f.mu.Unlock()
	sort.SliceStable(comments, func(i, j int) bool {
		if !comments[i].UpdatedAt().Equal(comments[j].UpdatedAt()) {
			return comments[i].UpdatedAt().Before(comments[j].UpdatedAt())
		}
		return comments[i].ID < comments[j].ID
	})
	if start > len(comments) {
		start = len(comments)
	}
	end := start + limit
	if end > len(comments) {
		end = len(comments)
	}
	results := append([]searchResult(nil), comments[start:end]...)
	base := requestSiteBase(r, deployment)
	for index := range results {
		results[index].Links.Base = base
	}
	writeSearchResponse(w, searchResponse{
		Results:   results,
		Start:     start,
		Limit:     limit,
		Size:      len(results),
		TotalSize: len(comments),
		Links: struct {
			Base string `json:"base"`
			Next string `json:"next"`
		}{Base: base},
	})
}

func writeSearchResponse(w http.ResponseWriter, response searchResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func commentPageID(path string) string {
	_, rest, ok := strings.Cut(path, "/content/")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

func requestSiteBase(r *http.Request, deployment string) string {
	prefix := "/wiki"
	if deployment == deploymentDataCenter {
		prefix = "/confluence"
	}
	return "http://" + r.Host + prefix
}

var (
	cqlBoundPattern = regexp.MustCompile(`lastmodified\s*>=\s*"([^"]+)"`)
	cqlSpacePattern = regexp.MustCompile(`space\s+in\s*\(([^)]*)\)`)
)

func filterSearch(pages []searchResult, cql string) []searchResult {
	var bound time.Time
	hasBound := false
	if match := cqlBoundPattern.FindStringSubmatch(cql); match != nil {
		if parsed, err := time.Parse(cqlTimeLayout, match[1]); err == nil {
			bound, hasBound = parsed, true
		}
	}
	var spaces []string
	if match := cqlSpacePattern.FindStringSubmatch(cql); match != nil {
		for _, part := range strings.Split(match[1], ",") {
			part = strings.Trim(strings.TrimSpace(part), `"`)
			if part != "" {
				spaces = append(spaces, part)
			}
		}
	}
	filtered := make([]searchResult, 0, len(pages))
	for _, page := range pages {
		if hasBound && page.UpdatedAt().Before(bound) {
			continue
		}
		if len(spaces) > 0 && !containsString(spaces, page.Space.Key) {
			continue
		}
		filtered = append(filtered, page)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].UpdatedAt().Equal(filtered[j].UpdatedAt()) {
			return filtered[i].UpdatedAt().Before(filtered[j].UpdatedAt())
		}
		return filtered[i].ID < filtered[j].ID
	})
	return filtered
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func atoiDefault(raw string, fallback int) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func (f *fakeConfluence) snapshot() (lastCQL string, starts []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastCQL, append([]int(nil), f.searchStarts...)
}

func (f *fakeConfluence) requestSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requestURLs...)
}

func fixedNow() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }

func newTestSource(t *testing.T, conn domain.Connection, credentials ports.CredentialProvider, client *http.Client) *Source {
	t.Helper()
	source, err := NewSource(conn, credentials, client, fixedNow)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return source
}

func cloudConnection(baseURL string) domain.Connection {
	return domain.Connection{
		ID:       "conf-test",
		Provider: Name,
		BaseURL:  baseURL,
		Settings: map[string]string{"email": testEmail},
	}
}

func dataCenterConnection(baseURL string) domain.Connection {
	return domain.Connection{
		ID:       "conf-test",
		Provider: Name,
		BaseURL:  baseURL,
		Settings: map[string]string{"deployment": deploymentDataCenter},
	}
}

func TestCloudBasicAuthAndCheck(t *testing.T) {
	fixture := newFakeConfluence(deploymentCloud)
	fixture.addPage("One", "<p>Body</p>", "ENG")
	server := httptest.NewServer(fixture)
	defer server.Close()

	conn := cloudConnection(server.URL + "/wiki")
	source := newTestSource(t, conn, testCredentials{"conf-test": testToken}, server.Client())
	if source.apiBase != server.URL+"/wiki/rest/api" {
		t.Fatalf("apiBase = %q, want %q", source.apiBase, server.URL+"/wiki/rest/api")
	}
	if source.siteBase != server.URL+"/wiki" {
		t.Fatalf("siteBase = %q, want %q", source.siteBase, server.URL+"/wiki")
	}
	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(testEmail+":"+testToken))
	fixture.mu.Lock()
	got := fixture.lastAuthorization
	fixture.mu.Unlock()
	if got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestDataCenterBearerAndCheck(t *testing.T) {
	fixture := newFakeConfluence(deploymentDataCenter)
	fixture.addPage("One", "<p>Body</p>", "ENG")
	server := httptest.NewServer(fixture)
	defer server.Close()

	conn := dataCenterConnection(server.URL + "/confluence")
	source := newTestSource(t, conn, testCredentials{"conf-test": testToken}, server.Client())
	if source.apiBase != server.URL+"/confluence/rest/api" {
		t.Fatalf("apiBase = %q, want %q", source.apiBase, server.URL+"/confluence/rest/api")
	}
	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := "Bearer " + testToken
	fixture.mu.Lock()
	got := fixture.lastAuthorization
	fixture.mu.Unlock()
	if got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestCQLIncludesInclusiveBoundSpaceFilterAndOrder(t *testing.T) {
	fixture := newFakeConfluence(deploymentCloud)
	fixture.addPageAt("Old", "<p>old</p>", "ENG", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	fixture.addPageAt("Edge", "<p>edge</p>", "ENG", time.Date(2026, 11, 2, 0, 0, 30, 0, time.UTC))
	fixture.addPageAt("New", "<p>new</p>", "OPS", time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	server := httptest.NewServer(fixture)
	defer server.Close()

	conn := cloudConnection(server.URL + "/wiki")
	conn.Settings["spaces"] = "ops, eng, OPS"
	conn.Settings["cql"] = `creator = "alice"`
	source := newTestSource(t, conn, testCredentials{"conf-test": testToken}, server.Client())
	since := time.Date(2026, 11, 2, 0, 0, 30, 0, time.UTC)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Since: &since, Limit: 10})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 2 {
		t.Fatalf("items = %d, want the inclusive edge page and the newer page", len(batch.Items))
	}
	cql, _ := fixture.snapshot()
	for _, want := range []string{
		`lastmodified >= "2026-11-02 00:00"`,
		`space in ("ENG","OPS")`,
		`creator = "alice"`,
		"ORDER BY lastmodified ASC",
	} {
		if !strings.Contains(cql, want) {
			t.Fatalf("cql = %q, want it to contain %q", cql, want)
		}
	}
	fixture.mu.Lock()
	query := fixture.lastQuery
	fixture.mu.Unlock()
	if got := query.Get("expand"); got != expandFields {
		t.Fatalf("expand = %q, want %q", got, expandFields)
	}
	if query.Get("start") != "0" || query.Get("limit") != strconv.Itoa(providerPageSize) {
		t.Fatalf("pagination query = %v", query)
	}
}

func TestCloudFiftyResultCapIsNotTreatedAsComplete(t *testing.T) {
	fixture := newFakeConfluence(deploymentCloud)
	for index := 0; index < 50; index++ {
		fixture.addPage(fmt.Sprintf("Page %d", index), "<p>body</p>", "ENG")
	}
	server := httptest.NewServer(fixture)
	defer server.Close()

	conn := cloudConnection(server.URL + "/wiki")
	source := newTestSource(t, conn, testCredentials{"conf-test": testToken}, server.Client())
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 500})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 50 || batch.More {
		t.Fatalf("items=%d more=%v, want 50 items and a completed pass", len(batch.Items), batch.More)
	}
	_, starts := fixture.snapshot()
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 50 {
		t.Fatalf("search starts = %v, want [0 50]; the first 50 rows must be confirmed by an empty page", starts)
	}
}

func TestCloudPaginationAdvancesPastFifty(t *testing.T) {
	fixture := newFakeConfluence(deploymentCloud)
	for index := 0; index < 120; index++ {
		fixture.addPage(fmt.Sprintf("Page %d", index), "<p>body</p>", "ENG")
	}
	server := httptest.NewServer(fixture)
	defer server.Close()

	conn := cloudConnection(server.URL + "/wiki")
	source := newTestSource(t, conn, testCredentials{"conf-test": testToken}, server.Client())
	var items []domain.Item
	checkpoints := map[string]domain.Checkpoint{}
	for batches := 0; ; batches++ {
		if batches > 20 {
			t.Fatal("More never cleared")
		}
		batch, err := source.Read(context.Background(), ports.ReadRequest{
			Connection: conn, Checkpoints: checkpoints, Limit: 500,
		})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		items = append(items, batch.Items...)
		for _, checkpoint := range batch.Checkpoints {
			checkpoints[checkpoint.Stream] = checkpoint
		}
		if !batch.More {
			break
		}
	}
	if len(items) != 120 {
		t.Fatalf("items = %d, want 120", len(items))
	}
	_, starts := fixture.snapshot()
	want := []int{0, 50, 100}
	if len(starts) != len(want) {
		t.Fatalf("search starts = %v, want %v", starts, want)
	}
	for index := range want {
		if starts[index] != want[index] {
			t.Fatalf("search starts = %v, want %v", starts, want)
		}
	}
}

func TestSinceIsInclusiveAndNarrowsWithCursor(t *testing.T) {
	fixture := newFakeConfluence(deploymentCloud)
	fixture.addPageAt("Old", "<p>old</p>", "ENG", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	fixture.addPageAt("Edge", "<p>edge</p>", "ENG", time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC))
	fixture.addPageAt("New", "<p>new</p>", "ENG", time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	server := httptest.NewServer(fixture)
	defer server.Close()

	conn := cloudConnection(server.URL + "/wiki")
	source := newTestSource(t, conn, testCredentials{"conf-test": testToken}, server.Client())
	edge := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Since: &edge, Limit: 10})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if got := externalIDs(first.Items); strings.Join(got, ",") != "page/1002,page/1003" {
		t.Fatalf("first items = %v, want the inclusive edge and the newer page", got)
	}
	newest := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	checkpoints := map[string]domain.Checkpoint{checkpointStream: first.Checkpoints[0]}
	second, err := source.Read(context.Background(), ports.ReadRequest{
		Connection: conn, Checkpoints: checkpoints, Since: &newest, Limit: 10,
	})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if got := externalIDs(second.Items); strings.Join(got, ",") != "page/1003" {
		t.Fatalf("second items = %v, want only the page at the inclusive Since bound", got)
	}
}

func externalIDs(items []domain.Item) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Ref.ExternalID)
	}
	return out
}

func TestCommentsKeepFiveNewestAndStayUTF8Safe(t *testing.T) {
	fixture := newFakeConfluence(deploymentCloud)
	pageID := fixture.addPage("Page", "<p>body</p>", "ENG")
	for index := 1; index <= 7; index++ {
		body := fmt.Sprintf("<p>comment-%d</p>", index)
		if index == 7 {
			body = "<p>comment-7 " + strings.Repeat("я", 20000) + "</p>"
		}
		fixture.addComment(pageID, body, time.Date(2026, 11, 1, 0, index, 0, 0, time.UTC))
	}
	server := httptest.NewServer(fixture)
	defer server.Close()

	conn := cloudConnection(server.URL + "/wiki")
	source := newTestSource(t, conn, testCredentials{"conf-test": testToken}, server.Client())
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(batch.Items))
	}
	body := batch.Items[0].Body
	if strings.Contains(body, "comment-1") || strings.Contains(body, "comment-2") {
		t.Fatalf("body contains an old comment: %q", body)
	}
	for index := 3; index <= 7; index++ {
		if !strings.Contains(body, fmt.Sprintf("comment-%d", index)) {
			t.Fatalf("body is missing comment-%d", index)
		}
	}
	if count := strings.Count(body, "- **"); count != maxCommentsPerItem {
		t.Fatalf("comment count = %d, want %d", count, maxCommentsPerItem)
	}
	if !utf8.ValidString(body) {
		t.Fatal("body is not valid UTF-8 after truncation")
	}
	if len(body) > maxSourceBodyBytes {
		t.Fatalf("body bytes = %d, exceeds %d", len(body), maxSourceBodyBytes)
	}
}

func TestMalformedAndNonMonotonicCursorsAreRejected(t *testing.T) {
	fixture := newFakeConfluence(deploymentCloud)
	fixture.addPage("One", "<p>body</p>", "ENG")
	server := httptest.NewServer(fixture)
	defer server.Close()
	conn := cloudConnection(server.URL + "/wiki")
	source := newTestSource(t, conn, testCredentials{"conf-test": testToken}, server.Client())

	cases := []struct {
		name string
		cp   domain.Checkpoint
	}{
		{"invalid-json", domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: []byte(`{`)}},
		{"negative-start", domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: []byte(`{"start":-1}`)}},
		{"offset-mismatch", domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: []byte(`{"start":0,"offset":2,"seen":["a"]}`)}},
		{"non-monotonic", domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: []byte(`{"watermark":"2026-11-05T00:00:00Z","passMax":"2026-11-01T00:00:00Z","start":0}`)}},
		{"done-with-page", domain.Checkpoint{Stream: checkpointStream, Version: checkpointVersion, Data: []byte(`{"done":true,"start":50}`)}},
		{"wrong-stream", domain.Checkpoint{Stream: "other", Version: checkpointVersion, Data: []byte(`{}`)}},
		{"wrong-version", domain.Checkpoint{Stream: checkpointStream, Version: 9, Data: []byte(`{}`)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := source.Read(context.Background(), ports.ReadRequest{
				Connection:  conn,
				Checkpoints: map[string]domain.Checkpoint{checkpointStream: testCase.cp},
				Limit:       5,
			})
			if err == nil {
				t.Fatal("Read accepted a malformed cursor")
			}
		})
	}
}

func TestHTTPFailuresFailClosedWithoutTokenLeak(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			fixture := newFakeConfluence(deploymentCloud)
			fixture.addPage("One", "<p>body</p>", "ENG")
			fixture.setStatus(status)
			server := httptest.NewServer(fixture)
			defer server.Close()

			conn := cloudConnection(server.URL + "/wiki")
			source := newTestSource(t, conn, testCredentials{"conf-test": testToken}, server.Client())
			if err := source.Check(context.Background()); err == nil {
				t.Fatalf("Check with HTTP %d succeeded", status)
			} else if strings.Contains(err.Error(), testToken) {
				t.Fatalf("Check error leaked the token: %v", err)
			}
			_, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 5})
			if err == nil {
				t.Fatalf("Read with HTTP %d succeeded", status)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("Read error leaked the token: %v", err)
			}
			for _, requestURL := range fixture.requestSnapshot() {
				if strings.Contains(requestURL, testToken) || strings.Contains(requestURL, testEmail) {
					t.Fatalf("request URL leaked a credential: %s", requestURL)
				}
			}
		})
	}
}

func TestNewSourceRejectsUnsafeBaseURLsAndConfig(t *testing.T) {
	credentials := testCredentials{"conf-test": testToken}
	cases := []struct {
		name string
		conn domain.Connection
	}{
		{"missing-base", domain.Connection{ID: "conf-test", Provider: Name, Settings: map[string]string{"email": testEmail}}},
		{"userinfo", domain.Connection{ID: "conf-test", Provider: Name, BaseURL: "https://user:pass@example.test/wiki", Settings: map[string]string{"email": testEmail}}},
		{"query", domain.Connection{ID: "conf-test", Provider: Name, BaseURL: "https://example.test/wiki?token=x", Settings: map[string]string{"email": testEmail}}},
		{"fragment", domain.Connection{ID: "conf-test", Provider: Name, BaseURL: "https://example.test/wiki#x", Settings: map[string]string{"email": testEmail}}},
		{"plain-http", domain.Connection{ID: "conf-test", Provider: Name, BaseURL: "http://example.test/wiki", Settings: map[string]string{"email": testEmail}}},
		{"cloud-without-email", domain.Connection{ID: "conf-test", Provider: Name, BaseURL: "https://example.test/wiki"}},
		{"bad-deployment", domain.Connection{ID: "conf-test", Provider: Name, BaseURL: "https://example.test/wiki", Settings: map[string]string{"deployment": "server", "email": testEmail}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewSource(testCase.conn, credentials, nil, fixedNow); err == nil {
				t.Fatal("NewSource accepted an unsafe connection")
			}
		})
	}
}

func TestDeterministicReplayFromSameCheckpoint(t *testing.T) {
	fixture := newFakeConfluence(deploymentCloud)
	for index := 0; index < 6; index++ {
		fixture.addPage(fmt.Sprintf("Page %d", index), "<p>body</p>", "ENG")
	}
	server := httptest.NewServer(fixture)
	defer server.Close()
	conn := cloudConnection(server.URL + "/wiki")

	read := func() ports.ReadBatch {
		source := newTestSource(t, conn, testCredentials{"conf-test": testToken}, server.Client())
		batch, err := source.Read(context.Background(), ports.ReadRequest{
			Connection: conn, Checkpoints: map[string]domain.Checkpoint{}, Limit: 2,
		})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		return batch
	}
	first, second := read(), read()
	if first.More != second.More || len(first.Items) != len(second.Items) || len(first.Checkpoints) != len(second.Checkpoints) {
		t.Fatalf("repeated reads diverged: first=%#v second=%#v", first, second)
	}
	for index := range first.Items {
		if first.Items[index].Ref != second.Items[index].Ref || first.Items[index].ComputeHash() != second.Items[index].ComputeHash() {
			t.Fatalf("item %d diverged between identical reads", index)
		}
	}
	if !first.Checkpoints[0].Equal(second.Checkpoints[0]) {
		t.Fatal("checkpoint diverged between identical reads")
	}
}

func TestNormalizeBaseURLAppendsPrefixExactlyOnce(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		deployment string
		api        string
		site       string
	}{
		{"cloud-root", "https://site.atlassian.net", deploymentCloud, "https://site.atlassian.net/wiki/rest/api", "https://site.atlassian.net/wiki"},
		{"cloud-wiki", "https://site.atlassian.net/wiki", deploymentCloud, "https://site.atlassian.net/wiki/rest/api", "https://site.atlassian.net/wiki"},
		{"cloud-api", "https://site.atlassian.net/wiki/rest/api/", deploymentCloud, "https://site.atlassian.net/wiki/rest/api", "https://site.atlassian.net/wiki"},
		{"cloud-bare-api", "https://site.atlassian.net/rest/api", deploymentCloud, "https://site.atlassian.net/wiki/rest/api", "https://site.atlassian.net/wiki"},
		{"dc-confluence", "https://host.example/confluence", deploymentDataCenter, "https://host.example/confluence/rest/api", "https://host.example/confluence"},
		{"dc-api", "https://host.example/confluence/rest/api", deploymentDataCenter, "https://host.example/confluence/rest/api", "https://host.example/confluence"},
		{"dc-root", "https://host.example", deploymentDataCenter, "https://host.example/rest/api", "https://host.example"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			api, site, err := normalizeBaseURL(testCase.raw, testCase.deployment)
			if err != nil {
				t.Fatalf("normalizeBaseURL: %v", err)
			}
			if api != testCase.api || site != testCase.site {
				t.Fatalf("normalizeBaseURL(%q) = (%q, %q), want (%q, %q)", testCase.raw, api, site, testCase.api, testCase.site)
			}
		})
	}
}

func TestItemURLUsesLinksBaseThenFallback(t *testing.T) {
	source := &Source{siteBase: "https://site.example/wiki"}
	var result searchResult
	result.Links.WebUI = "/spaces/ENG/pages/42/Title"
	if got := source.itemURL(result, ""); got != "https://site.example/wiki/spaces/ENG/pages/42/Title" {
		t.Fatalf("fallback URL = %q", got)
	}
	if got := source.itemURL(result, "https://envelope.example/wiki"); got != "https://envelope.example/wiki/spaces/ENG/pages/42/Title" {
		t.Fatalf("envelope base URL = %q", got)
	}
	result.Links.Base = "https://links.example/wiki"
	if got := source.itemURL(result, "https://envelope.example/wiki"); got != "https://links.example/wiki/spaces/ENG/pages/42/Title" {
		t.Fatalf("links.base URL = %q", got)
	}
	result.Links.WebUI = "https://absolute.example/page"
	if got := source.itemURL(result, ""); got != "https://absolute.example/page" {
		t.Fatalf("absolute webui = %q", got)
	}
}

func TestSourceContract(t *testing.T) {
	fixture := newFakeConfluence(deploymentCloud)
	fixture.addPage("Page 1", "<p><strong>Body 1</strong></p>", "ENG")
	fixture.addPage("Page 2", "<p>Body 2</p>", "ENG")
	fixture.addPage("Page 3", "<p>Body 3</p>", "OPS")
	server := httptest.NewServer(fixture)
	defer server.Close()

	conn := cloudConnection(server.URL + "/wiki")
	bad := conn
	bad.ID = "conf-bad"
	bad.Account = "bad"
	credentials := testCredentials{"conf-test": testToken, "conf-bad": badToken}

	h := ingestiontest.Harness{
		Connection: conn,
		NewSource: func(c domain.Connection) ports.Source {
			source, err := NewSource(c, credentials, server.Client(), fixedNow)
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			return source
		},
		AddItem: func() {
			fixture.addPage("Added", "<p>added</p>", "ENG")
		},
		UpdateItem: fixture.updateFirstPage,
		AddMany: func(n int) {
			for index := 0; index < n; index++ {
				fixture.addPage(fmt.Sprintf("Many %d", index), "<p>many</p>", "ENG")
			}
		},
		BadConnection: &bad,
		PageSize:      3,
	}
	ingestiontest.RunSourceSuite(t, h)
}
