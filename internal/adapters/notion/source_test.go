package notion

import (
	"context"
	"encoding/json"
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

const testToken = "sanitized-test-token"

type testCreds struct {
	token string
	id    string
}

func (c testCreds) Get(_ context.Context, connectionID, field string) (string, error) {
	if field != "token" || c.token == "" {
		return "", domain.ErrNotFound
	}
	if c.id != "" && connectionID != c.id {
		return "", domain.ErrNotFound
	}
	return c.token, nil
}

func testConn(baseURL string, settings map[string]string) domain.Connection {
	if settings == nil {
		settings = map[string]string{}
	}
	return domain.Connection{ID: "notion-test", Provider: Name, BaseURL: baseURL, Settings: settings}
}

func newTestSource(t *testing.T, conn domain.Connection, token string, client *http.Client) *Source {
	t.Helper()
	source, err := NewSource(conn, testCreds{token: token}, client, func() time.Time { return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return source
}

func TestDescriptor(t *testing.T) {
	desc := Descriptor()
	if err := desc.Validate(); err != nil {
		t.Fatalf("descriptor invalid: %v", err)
	}
	if desc.Provider != "notion" || desc.DisplayName != "Notion" {
		t.Fatalf("descriptor identity = %q/%q", desc.Provider, desc.DisplayName)
	}
	if !desc.Capabilities.Backfill || !desc.Capabilities.Incremental || desc.Capabilities.Deletes {
		t.Fatalf("capabilities = %+v, want backfill+incremental, no deletes", desc.Capabilities)
	}
	if len(desc.Capabilities.AuthModes) != 1 || desc.Capabilities.AuthModes[0] != domain.AuthToken {
		t.Fatalf("auth modes = %v, want [token]", desc.Capabilities.AuthModes)
	}
	if desc.DefaultPublish != domain.PublishLocalOnly {
		t.Fatalf("default publish = %q, want local-only", desc.DefaultPublish)
	}
	if !desc.PersonalData {
		t.Fatal("PersonalData must be true: pages may contain personal content")
	}
	keys := map[string]bool{}
	for _, field := range desc.ConfigFields {
		keys[field.Key] = true
	}
	for _, want := range []string{"token", "databases", "pages", "version"} {
		if !keys[want] {
			t.Fatalf("config fields miss %q", want)
		}
	}
	tokenField, _ := desc.Field("token")
	if !tokenField.Required || !tokenField.Secret {
		t.Fatal("token field must be required secret")
	}
	if len(desc.Streams) != 0 {
		t.Fatalf("streams = %v, want dynamic (empty)", desc.Streams)
	}
}

func TestNewSourceValidation(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	base := map[string]string{"databases": "db-1"}
	conn := testConn("", base)
	source, err := NewSource(conn, testCreds{token: testToken}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewSource with defaults: %v", err)
	}
	if !strings.HasPrefix(source.baseURL, "https://") {
		t.Fatalf("default baseURL = %q", source.baseURL)
	}
	if source.version != DefaultVersion {
		t.Fatalf("default version = %q", source.version)
	}
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"userinfo", "https://user:pass@example.com"},
		{"query", "https://example.com/?x=1"},
		{"fragment", "https://example.com/#frag"},
		{"plain-http", "http://example.com"},
	} {
		c := testConn(tc.url, base)
		if _, err := NewSource(c, testCreds{token: testToken}, server.Client(), nil); err == nil {
			t.Fatalf("%s accepted %q", tc.name, tc.url)
		}
	}
	loopback := testConn(server.URL, base)
	if _, err := NewSource(loopback, testCreds{token: testToken}, server.Client(), nil); err != nil {
		t.Fatalf("loopback http rejected: %v", err)
	}
	noToken := testConn(server.URL, base)
	if _, err := NewSource(noToken, nil, server.Client(), nil); err == nil {
		t.Fatal("nil credentials accepted")
	}
	missingToken := testConn(server.URL, map[string]string{})
	if _, err := NewSource(missingToken, testCreds{token: ""}, server.Client(), nil); err != nil {
		// NewSource itself does not read the secret; Check/Read do.
		t.Fatalf("NewSource without secret in settings should pass descriptor validation: %v", err)
	}
}

func TestCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/users/me" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Notion-Version") == "" {
			http.Error(w, "missing version", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"user","id":"user-1","type":"person","name":"Tester"}`))
	}))
	defer server.Close()
	conn := testConn(server.URL, nil)
	if err := newTestSource(t, conn, testToken, server.Client()).Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	bad := newTestSource(t, conn, "wrong-token", server.Client())
	if err := bad.Check(context.Background()); err == nil {
		t.Fatal("Check with bad token succeeded")
	} else if strings.Contains(err.Error(), "wrong-token") {
		t.Fatalf("Check error leaks token: %v", err)
	}
}

func TestCheckRejectsIncompleteMe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"user","id":"","type":""}`))
	}))
	defer server.Close()
	conn := testConn(server.URL, nil)
	if err := newTestSource(t, conn, testToken, server.Client()).Check(context.Background()); err == nil {
		t.Fatal("Check accepted users/me without id/type")
	}
}

func writePage(w http.ResponseWriter, id, title, updated, created string) {
	props := `{"Name":{"id":"title","type":"title","title":[{"plain_text":` + strconv.Quote(title) + `}]}}`
	page := `{"object":"page","id":` + strconv.Quote(id) + `,"created_time":` + strconv.Quote(created) +
		`,"last_edited_time":` + strconv.Quote(updated) + `,"last_edited_by":{"object":"user","id":"user-1","type":"person","name":"Ed"},` +
		`"properties":` + props + `,"url":"https://www.notion.so/` + strings.ReplaceAll(id, "-", "") + `"}`
	_, _ = w.Write([]byte(page))
}

func TestReadDatabaseQueryFilter(t *testing.T) {
	var sawFilter []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/children") {
			_, _ = w.Write([]byte(`{"object":"list","results":[{"object":"block","id":"b1","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"hello"}]}}],"next_cursor":null,"has_more":false}`))
			return
		}
		if strings.Contains(r.URL.Path, "/data_sources/") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if filter, ok := body["filter"].(map[string]any); ok {
				if inner, ok := filter["last_edited_time"].(map[string]any); ok {
					if v, ok := inner["on_or_after"].(string); ok {
						sawFilter = append(sawFilter, v)
					}
				}
			}
			bound := ""
			if len(sawFilter) > 0 {
				bound = sawFilter[len(sawFilter)-1]
			}
			all := []struct{ id, title, updated string }{
				{"11111111-1111-1111-1111-111111111111", "First", "2026-11-01T00:00:00Z"},
				{"22222222-2222-2222-2222-222222222222", "Second", "2026-11-02T00:00:00Z"},
				{"33333333-3333-3333-3333-333333333333", "Third", "2026-11-03T00:00:00Z"},
			}
			var out []string
			for _, p := range all {
				if bound != "" && p.updated < bound {
					continue
				}
				props := `{"Name":{"id":"title","type":"title","title":[{"plain_text":` + strconv.Quote(p.title) + `}]}}`
				out = append(out, `{"object":"page","id":`+strconv.Quote(p.id)+`,"created_time":"2026-11-01T00:00:00Z","last_edited_time":`+strconv.Quote(p.updated)+`,"last_edited_by":{"object":"user","id":"user-1","type":"person","name":"Ed"},"properties":`+props+`,"url":"https://www.notion.so/x"}`)
			}
			_, _ = w.Write([]byte(`{"object":"list","results":[` + strings.Join(out, ",") + `],"next_cursor":null,"has_more":false}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	conn := testConn(server.URL, map[string]string{"databases": "db-1"})
	source := newTestSource(t, conn, testToken, server.Client())
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 3 || batch.More {
		t.Fatalf("items=%d more=%v, want 3 and done", len(batch.Items), batch.More)
	}
	first := batch.Items[0]
	if first.Kind != domain.ItemPage || !strings.HasPrefix(first.Ref.ExternalID, "data_source/db-1/") {
		t.Fatalf("item = %+v, want data_source external id", first)
	}
	if first.Title != "First" || first.Author != "Ed" || !strings.Contains(first.Body, "First") || !strings.Contains(first.Body, "hello") {
		t.Fatalf("item = %+v, want title/author/frontmatter/blocks", first)
	}
	if len(batch.Checkpoints) != 1 || batch.Checkpoints[0].Stream != "datasource-db-1" {
		t.Fatalf("checkpoints = %+v, want datasource-db-1", batch.Checkpoints)
	}
	// Second pass with the persisted watermark must send a last_edited_time filter.
	cps := map[string]domain.Checkpoint{batch.Checkpoints[0].Stream: batch.Checkpoints[0]}
	before := len(sawFilter)
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: cps, Limit: 10})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if len(sawFilter) <= before {
		t.Fatal("incremental pass sent no last_edited_time filter")
	}
	if len(second.Items) == 0 {
		t.Fatal("overlap pass returned no items; want watermark-5m replay")
	}
}

func TestReadBlockConversionAndPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/pages/root-1" {
			_, _ = w.Write([]byte(`{"object":"page","id":"root-1","created_time":"2026-11-01T00:00:00Z","last_edited_time":"2026-11-02T00:00:00Z","last_edited_by":{"object":"user","id":"user-1","type":"person","name":"Ed"},"properties":{},"url":"https://www.notion.so/root"}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/blocks/") {
			cursor := r.URL.Query().Get("start_cursor")
			if strings.Contains(r.URL.Path, "child-with-kids") {
				_, _ = w.Write([]byte(`{"object":"list","results":[{"object":"block","id":"grandchild","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"deep"}}],"has_children":false}],"next_cursor":null,"has_more":false}`))
				return
			}
			if cursor == "" {
				_, _ = w.Write([]byte(`{"object":"list","results":[
{"object":"block","id":"h","type":"heading_1","heading_1":{"rich_text":[{"plain_text":"Doc Title"}]}},
{"object":"block","id":"p1","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"intro"}]}},
{"object":"block","id":"withkids","type":"toggle","toggle":{"rich_text":[{"plain_text":"more"}]},"has_children":true},
{"object":"block","id":"skip1","type":"image"},
{"object":"block","id":"q","type":"quote","quote":{"rich_text":[{"plain_text":"quoted"}]}},
{"object":"block","id":"lp","type":"link_preview","link_preview":{"url":"https://example.com/x"}}
],"next_cursor":"page2","has_more":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"object":"list","results":[
{"object":"block","id":"code1","type":"code","code":{"rich_text":[{"plain_text":"x=1"}],"language":"python"}},
{"object":"block","id":"div","type":"divider"},
{"object":"block","id":"li","type":"bulleted_list_item","bulleted_list_item":{"rich_text":[{"plain_text":"item"}]}},
{"object":"block","id":"call","type":"callout","callout":{"rich_text":[{"plain_text":"note"}]}}
],"next_cursor":null,"has_more":false}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	conn := testConn(server.URL, map[string]string{"pages": "root-1"})
	source := newTestSource(t, conn, testToken, server.Client())
	// Point the toggle's children at a known endpoint by renaming: the fake
	// serves any child id, but only child-with-kids has nested content; the
	// traversal must still be deterministic and skip the image.
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("items=%d, want the single root page", len(batch.Items))
	}
	body := batch.Items[0].Body
	for _, want := range []string{"# Doc Title", "intro", "more", "> quoted", "https://example.com/x", "```python", "---", "- item", "note"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "image") {
		t.Fatalf("body should skip embeds/files: %s", body)
	}
	if batch.Items[0].Title != "Doc Title" {
		t.Fatalf("title = %q, want first heading", batch.Items[0].Title)
	}
	if batch.Items[0].Ref.ExternalID != "page/root-1" {
		t.Fatalf("external id = %q", batch.Items[0].Ref.ExternalID)
	}
}

func TestSearchWatermarkHeuristic(t *testing.T) {
	pages := []struct{ id, updated string }{
		{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "2026-11-03T00:00:00Z"},
		{"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "2026-11-01T00:00:00Z"},
		{"cccccccc-cccc-cccc-cccc-cccccccccccc", "2026-10-01T00:00:00Z"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/children") {
			_, _ = w.Write([]byte(`{"object":"list","results":[],"next_cursor":null,"has_more":false}`))
			return
		}
		var out []string
		for _, p := range pages {
			out = append(out, `{"object":"page","id":`+strconv.Quote(p.id)+`,"created_time":"2026-10-01T00:00:00Z","last_edited_time":`+strconv.Quote(p.updated)+`,"last_edited_by":{"object":"user","id":"u","type":"person"},"properties":{},"url":""}`)
		}
		_, _ = w.Write([]byte(`{"object":"list","results":[` + strings.Join(out, ",") + `],"next_cursor":null,"has_more":false}`))
	}))
	defer server.Close()
	conn := testConn(server.URL, nil)
	source := newTestSource(t, conn, testToken, server.Client())
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if len(first.Items) != 3 {
		t.Fatalf("first items=%d, want all 3 without a watermark", len(first.Items))
	}
	cps := map[string]domain.Checkpoint{first.Checkpoints[0].Stream: first.Checkpoints[0]}
	if first.Checkpoints[0].Stream != "notion-search" {
		t.Fatalf("stream = %q, want notion-search", first.Checkpoints[0].Stream)
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: cps, Limit: 10})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	// Watermark is 2026-11-03, bound is watermark-5m; the October page is
	// older and paging must stop before it.
	for _, item := range second.Items {
		if strings.HasSuffix(item.Ref.ExternalID, "cccccccc-cccc-cccc-cccc-cccccccccccc") {
			t.Fatalf("search heuristic failed: October page replayed past the bound: %+v", item)
		}
	}
}

func TestBoundsBlocksAndBody(t *testing.T) {
	var blocks []string
	for i := 0; i < 150; i++ {
		blocks = append(blocks, `{"object":"block","id":"b`+strconv.Itoa(i)+`","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"line `+strconv.Itoa(i)+`"}]}}`)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/pages/big" {
			_, _ = w.Write([]byte(`{"object":"page","id":"big","created_time":"2026-11-01T00:00:00Z","last_edited_time":"2026-11-02T00:00:00Z","properties":{},"url":""}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","results":[` + strings.Join(blocks, ",") + `],"next_cursor":null,"has_more":false}`))
	}))
	defer server.Close()
	conn := testConn(server.URL, map[string]string{"pages": "big"})
	source := newTestSource(t, conn, testToken, server.Client())
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 10})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("items=%d", len(batch.Items))
	}
	if len(batch.Items[0].Body) > domain.MaxItemBodyBytes {
		t.Fatalf("body=%d bytes, exceeds cap", len(batch.Items[0].Body))
	}
	if strings.Contains(batch.Items[0].Body, "line 149") {
		t.Fatal("block traversal exceeded the 100-block bound")
	}
	if !strings.Contains(batch.Items[0].Body, "line 0") {
		t.Fatal("bounded body lost its head")
	}
}

func TestMalformedCursorRejected(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	conn := testConn(server.URL, map[string]string{"databases": "db-1"})
	source := newTestSource(t, conn, testToken, server.Client())
	bad := []string{
		`{invalid`,
		`{"offset":-1}`,
		`{"offset":101}`,
		`{"seen":-2}`,
		`{"cursor":"` + strings.Repeat("x", 5000) + `"}`,
	}
	for i, data := range bad {
		cp := domain.Checkpoint{Stream: "datasource-db-1", Version: checkpointVersion, Data: []byte(data)}
		_, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: map[string]domain.Checkpoint{cp.Stream: cp}})
		if err == nil {
			t.Fatalf("case %d accepted malformed cursor", i)
		}
	}
	cp := domain.Checkpoint{Stream: "datasource-db-1", Version: 999, Data: []byte(`{}`)}
	if _, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: map[string]domain.Checkpoint{cp.Stream: cp}}); err == nil {
		t.Fatal("wrong checkpoint version accepted")
	}
}

func TestAuthErrorsHideToken(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", code)
		}))
		conn := testConn(server.URL, map[string]string{"databases": "db-1"})
		source := newTestSource(t, conn, testToken, server.Client())
		if err := source.Check(context.Background()); err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("code %d check leaks token: %v", code, err)
		}
		if _, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn}); err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("code %d read leaks token: %v", code, err)
		}
		server.Close()
	}
}

func TestDeterministicReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/children") {
			_, _ = w.Write([]byte(`{"object":"list","results":[{"object":"block","id":"b","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"same"}]}}],"next_cursor":null,"has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","results":[{"object":"page","id":"dddddddd-dddd-dddd-dddd-dddddddddddd","created_time":"2026-11-01T00:00:00Z","last_edited_time":"2026-11-02T00:00:00Z","properties":{"Name":{"type":"title","title":[{"plain_text":"T"}]}},"url":""}],"next_cursor":null,"has_more":false}`))
	}))
	defer server.Close()
	conn := testConn(server.URL, map[string]string{"databases": "db-1"})
	source := newTestSource(t, conn, testToken, server.Client())
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(first.Items) != len(second.Items) || first.More != second.More || len(first.Checkpoints) != len(second.Checkpoints) {
		t.Fatalf("replay diverged: %+v vs %+v", first, second)
	}
	for i := range first.Items {
		if first.Items[i].ComputeHash() != second.Items[i].ComputeHash() {
			t.Fatalf("item %d differs", i)
		}
	}
	for i := range first.Checkpoints {
		if !first.Checkpoints[i].Equal(second.Checkpoints[i]) {
			t.Fatal("checkpoint differs between identical reads")
		}
	}
}

type contractFake struct {
	mu     sync.Mutex
	pages  []fakePage
	now    time.Time
	next   int
	token  string
	db     string
	blocks map[string]string
}

type fakePage struct {
	id      string
	title   string
	created time.Time
	updated time.Time
}

func (f *contractFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/v1/users/me" {
		_, _ = w.Write([]byte(`{"object":"user","id":"user-1","type":"person"}`))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/children") {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		id := ""
		if len(parts) >= 3 {
			id = parts[2]
		}
		f.mu.Lock()
		title := ""
		for _, p := range f.pages {
			if p.id == id {
				title = p.title
			}
		}
		f.mu.Unlock()
		if title == "" {
			title = "body"
		}
		_, _ = w.Write([]byte(`{"object":"list","results":[{"object":"block","id":"b-` + id + `","type":"paragraph","paragraph":{"rich_text":[{"plain_text":` + strconv.Quote("body "+title) + `}]}}],"next_cursor":null,"has_more":false}`))
		return
	}
	if strings.Contains(r.URL.Path, "/data_sources/") {
		var body struct {
			PageSize    int     `json:"page_size"`
			StartCursor *string `json:"start_cursor"`
			Filter      *struct {
				LastEditedTime map[string]string `json:"last_edited_time"`
			} `json:"filter"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var bound *time.Time
		if body.Filter != nil {
			if raw, ok := body.Filter.LastEditedTime["on_or_after"]; ok {
				if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
					bound = &parsed
				} else if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
					bound = &parsed
				}
			}
		}
		f.mu.Lock()
		all := append([]fakePage(nil), f.pages...)
		f.mu.Unlock()
		sort.Slice(all, func(i, j int) bool {
			if !all[i].updated.Equal(all[j].updated) {
				return all[i].updated.Before(all[j].updated)
			}
			return all[i].id < all[j].id
		})
		var kept []fakePage
		for _, p := range all {
			if bound != nil && p.updated.Before(bound.UTC()) {
				continue
			}
			kept = append(kept, p)
		}
		pageSize := body.PageSize
		if pageSize <= 0 || pageSize > 100 {
			pageSize = 100
		}
		start := 0
		if body.StartCursor != nil && *body.StartCursor != "" {
			start, _ = strconv.Atoi(*body.StartCursor)
		}
		if start < 0 {
			start = 0
		}
		if start > len(kept) {
			start = len(kept)
		}
		end := start + pageSize
		if end > len(kept) {
			end = len(kept)
		}
		var out []string
		for _, p := range kept[start:end] {
			props := `{"Name":{"id":"title","type":"title","title":[{"plain_text":` + strconv.Quote(p.title) + `}]}}`
			out = append(out, `{"object":"page","id":`+strconv.Quote(p.id)+`,"created_time":`+strconv.Quote(p.created.UTC().Format(time.RFC3339Nano))+`,"last_edited_time":`+strconv.Quote(p.updated.UTC().Format(time.RFC3339Nano))+`,"last_edited_by":{"object":"user","id":"user-1","type":"person","name":"Ed"},"properties":`+props+`,"url":"https://www.notion.so/x"}`)
		}
		next := ""
		hasMore := false
		if end < len(kept) {
			next = strconv.Itoa(end)
			hasMore = true
		}
		nextJSON := "null"
		if hasMore {
			nextJSON = strconv.Quote(next)
		}
		_, _ = w.Write([]byte(`{"object":"list","results":[` + strings.Join(out, ",") + `],"next_cursor":` + nextJSON + `,"has_more":` + strconv.FormatBool(hasMore) + `}`))
		return
	}
	http.NotFound(w, r)
}

func (f *contractFake) addItem() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.now = f.now.Add(time.Second)
	id := "page-" + strconv.Itoa(f.next)
	f.pages = append(f.pages, fakePage{id: id, title: "Page " + strconv.Itoa(f.next), created: f.now, updated: f.now})
}

func (f *contractFake) updateItem() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pages) == 0 {
		f.next++
		f.now = f.now.Add(time.Second)
		f.pages = append(f.pages, fakePage{id: "page-1", title: "Page 1", created: f.now, updated: f.now})
		return
	}
	f.now = f.now.Add(time.Minute)
	f.pages[0].title += " updated"
	f.pages[0].updated = f.now
}

func TestSourceContract(t *testing.T) {
	fake := &contractFake{now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), token: testToken, db: "db-1"}
	fake.addItem()
	fake.addItem()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	_ = u
	conn := domain.Connection{ID: "notion-test", Provider: Name, BaseURL: server.URL, Projects: []string{"lv24"}, Settings: map[string]string{"databases": "db-1"}}
	bad := conn
	bad.ID = "notion-bad"
	harness := ingestiontest.Harness{
		Connection:    conn,
		BadConnection: &bad,
		PageSize:      5,
		NewSource: func(c domain.Connection) ports.Source {
			token := testToken
			if c.ID == "notion-bad" {
				token = "wrong-token"
			}
			source, err := NewSource(c, testCreds{token: token}, server.Client(), func() time.Time {
				return time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
			})
			if err != nil {
				t.Fatalf("NewSource(%s): %v", c.ID, err)
			}
			return source
		},
		AddItem:    func() { fake.addItem() },
		UpdateItem: func() { fake.updateItem() },
		AddMany: func(n int) {
			for range n {
				fake.addItem()
			}
		},
	}
	ingestiontest.RunSourceSuite(t, harness)
}
