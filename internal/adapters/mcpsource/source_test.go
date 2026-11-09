package mcpsource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ingestiontest"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDescriptorAndSettingsValidation(t *testing.T) {
	desc := Descriptor()
	if err := desc.Validate(); err != nil {
		t.Fatalf("Descriptor.Validate: %v", err)
	}
	if desc.Provider != "mcp" || desc.DisplayName != "MCP server" || desc.DefaultPublish != domain.PublishLocalOnly || !desc.PersonalData {
		t.Fatalf("descriptor identity/policy mismatch: %+v", desc)
	}
	if !desc.Capabilities.Backfill || !desc.Capabilities.Incremental || desc.Capabilities.Deletes || !reflect.DeepEqual(desc.Capabilities.AuthModes, []domain.AuthMode{domain.AuthNone, domain.AuthToken}) {
		t.Fatalf("descriptor capabilities mismatch: %+v", desc.Capabilities)
	}
	for _, field := range desc.ConfigFields {
		if err := domain.ValidateConfigKey("field", field.Key); err != nil {
			t.Errorf("invalid config key %q: %v", field.Key, err)
		}
	}
	if field, ok := desc.Field("token"); !ok || !field.Secret {
		t.Fatal("token field must be secret")
	}

	valid := stdioConnection(map[string]string{"tools": "search_docs,find_docs", "timeout": "5m", "maxitems": "500"})
	if _, err := NewSource(valid, nil, nil); err != nil {
		t.Fatalf("valid stdio settings: %v", err)
	}
	for name, mutate := range map[string]func(domain.Connection){
		"missing transport":   func(c domain.Connection) { delete(c.Settings, "transport") },
		"unknown transport":   func(c domain.Connection) { c.Settings["transport"] = "socket" },
		"command in settings": func(c domain.Connection) { c.Settings["command"] = "node" },
		"args in settings":    func(c domain.Connection) { c.Settings["args"] = "one" },
		"too many tools": func(c domain.Connection) {
			names := make([]string, maximumTools+1)
			for i := range names {
				names[i] = fmt.Sprintf("tool%d", i)
			}
			c.Settings["tools"] = strings.Join(names, ",")
		},
		"maxitems low":    func(c domain.Connection) { c.Settings["maxitems"] = "0" },
		"maxitems high":   func(c domain.Connection) { c.Settings["maxitems"] = "501" },
		"timeout invalid": func(c domain.Connection) { c.Settings["timeout"] = "forever" },
		"timeout high":    func(c domain.Connection) { c.Settings["timeout"] = "5m1s" },
		"stdio url":       func(c domain.Connection) { c.Settings["url"] = "https://example.org/mcp" },
	} {
		t.Run(name, func(t *testing.T) {
			conn := stdioConnection(nil)
			mutate(conn)
			if _, err := NewSource(conn, nil, nil); err == nil {
				t.Fatal("NewSource succeeded; want validation error")
			}
		})
	}
	for name, raw := range map[string]string{
		"https":         "https://example.org/mcp",
		"loopback http": "http://127.0.0.1:1234/mcp",
	} {
		t.Run(name, func(t *testing.T) {
			conn := httpConnection(raw)
			if _, err := NewSource(conn, nil, nil); err != nil {
				t.Fatalf("NewSource: %v", err)
			}
		})
	}
	for name, raw := range map[string]string{
		"plain http":  "http://example.org/mcp",
		"userinfo":    "https://user:pass@example.org/mcp",
		"fragment":    "https://example.org/mcp#part",
		"query":       "https://example.org/mcp?token=x",
		"empty query": "https://example.org/mcp?",
		"file scheme": "file:///tmp/mcp",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSource(httpConnection(raw), nil, nil); err == nil {
				t.Fatal("NewSource succeeded; want URL validation error")
			}
		})
	}
}

func TestToolSelectionAndSchemaArguments(t *testing.T) {
	for _, name := range []string{"search_docs", "LIST_recent", "recent_query", "find_by_name", "search_update_log"} {
		got := defaultReadTool(name)
		want := !strings.Contains(strings.ToLower(name), "update")
		if got != want {
			t.Errorf("defaultReadTool(%q) = %v, want %v", name, got, want)
		}
	}
	for _, name := range []string{"create_search", "list_updates", "delete_item", "write_search", "send_query", "execute_search", "run_list", "call_search", "set_search"} {
		if !deniedTool(name) || defaultReadTool(name) {
			t.Errorf("deny-list did not reject %q", name)
		}
	}
	schema := map[string]any{
		"type":     "object",
		"required": []any{"q", "limit", "token"},
		"properties": map[string]any{
			"q":     map[string]any{"type": "string"},
			"limit": map[string]any{"type": "integer", "default": float64(4)},
			"token": map[string]any{"type": "integer"},
		},
	}
	if _, ok := deriveArguments(schema, "*", 100); ok {
		t.Fatal("schema with required non-string/no-default fields was synthesized")
	}
	schema["required"] = []any{"q"}
	args, ok := deriveArguments(schema, "term", 100)
	if !ok || args["q"] != "term" {
		t.Fatalf("derived args = %#v, %v", args, ok)
	}
	if _, exists := args["limit"]; exists {
		t.Fatalf("optional default was injected: %#v", args)
	}
	if _, ok := deriveArguments(objectSchema(map[string]any{"api_token": map[string]any{"type": "string"}}, "api_token"), "*", 100); ok {
		t.Fatal("secret-like required string was synthesized from the query")
	}
}

func TestItemFieldAndSizeBounds(t *testing.T) {
	conn := stdioConnection(nil)
	source, err := NewSource(conn, nil, nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	updated := "2026-11-09T10:00:00Z"
	item := source.item(conn.ID, "list_docs", map[string]any{
		"title": strings.Repeat("T", 700), "content": strings.Repeat("x", 600<<10),
		"url": "https://example.org/" + strings.Repeat("p", 3000), "author": strings.Repeat("A", 300),
		"updated_at": updated,
	})
	if len(item.Title) > 512 || len(item.Author) > 256 || len(item.URL) > 2048 || len(item.Body) > domain.MaxItemBodyBytes {
		t.Fatalf("field cap exceeded: title=%d author=%d url=%d body=%d", len(item.Title), len(item.Author), len(item.URL), len(item.Body))
	}
	if item.URL != "" {
		t.Fatalf("overlong URL should be omitted, got length %d", len(item.URL))
	}
	if !item.UpdatedAt.Equal(time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("UpdatedAt = %v", item.UpdatedAt)
	}
}

func TestReadFiltersExtractsAndPaginates(t *testing.T) {
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	var callsMu sync.Mutex
	var calls []string
	var gotArgs map[string]any
	toolBuilder := func(server *mcp.Server) {
		addServerTool(server, "search_docs", objectSchema(map[string]any{
			"q": map[string]any{"type": "string"}, "page": map[string]any{"type": "integer", "default": float64(2)},
		}, "q"), func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			callsMu.Lock()
			calls = append(calls, request.Params.Name)
			_ = json.Unmarshal(request.Params.Arguments, &gotArgs)
			callsMu.Unlock()
			return &mcp.CallToolResult{StructuredContent: map[string]any{"results": []any{
				map[string]any{"id": "one", "title": "First title", "content": "First body", "url": "https://example.org/one", "author": "Alice", "created_at": base.Format(time.RFC3339), "updated_at": base.Format(time.RFC3339)},
				map[string]any{"id": "two", "name": "Second title", "summary": "Second body", "url": "javascript:alert(1)", "updated_at": base.Add(4 * time.Minute).Format(time.RFC3339)},
				map[string]any{"id": "three", "subject": "Third title", "body": strings.Repeat("x", 500<<10), "updated_at": base.Add(10 * time.Minute).Format(time.RFC3339)},
			}}}, nil
		})
		addServerTool(server, "search_and_delete", objectSchema(nil), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			t.Fatalf("deny-listed tool was called")
			return nil, nil
		})
		addServerTool(server, "find_by_number", objectSchema(map[string]any{"number": map[string]any{"type": "integer"}}, "number"), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			t.Fatalf("unsynthesizable tool was called")
			return nil, nil
		})
		addServerTool(server, "query_not_enabled", objectSchema(nil), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			t.Fatalf("tool outside allow-list was called")
			return nil, nil
		})
	}
	conn := stdioConnection(map[string]string{"tools": "search_docs,search_and_delete,find_by_number", "maxitems": "3"})
	source := mustMemorySource(t, conn, toolBuilder, func() time.Time { return base.Add(time.Hour) })
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 1})
	if err != nil {
		t.Fatalf("Read first: %v", err)
	}
	if len(first.Items) != 1 || !first.More {
		t.Fatalf("first batch len=%d more=%v", len(first.Items), first.More)
	}
	item := first.Items[0]
	if item.Title != "First title" || !strings.Contains(item.Body, "First body") || !strings.HasPrefix(item.Body, "MCP tool search_docs (recent results only)") {
		t.Fatalf("item extraction mismatch: %+v", item)
	}
	if item.URL != "https://example.org/one" || item.Author != "Alice" || !item.CreatedAt.Equal(base) || !item.UpdatedAt.Equal(base) {
		t.Fatalf("item metadata mismatch: %+v", item)
	}
	if !strings.Contains(item.Ref.ExternalID, "/one") || !strings.HasPrefix(item.Ref.ExternalID, "mcp/") {
		t.Fatalf("external ID should use the stable provider id and be namespaced: %q", item.Ref.ExternalID)
	}
	if gotArgs["q"] != "*" {
		t.Fatalf("schema args = %#v", gotArgs)
	}
	if _, ok := gotArgs["page"]; ok {
		t.Fatalf("optional schema default was injected: %#v", gotArgs)
	}
	if got := source.SkippedTools(); !reflect.DeepEqual(got, []string{"search_and_delete", "find_by_number"}) {
		t.Fatalf("skipped tools = %v", got)
	}

	checkpoint := first.Checkpoints[0]
	var firstCursor cursor
	if err := json.Unmarshal(checkpoint.Data, &firstCursor); err != nil || firstCursor.LastScan != 1 || firstCursor.Watermark != nil {
		t.Fatalf("first checkpoint = %s (%v)", checkpoint.Data, err)
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: map[string]domain.Checkpoint{checkpoint.Stream: checkpoint}, Limit: 1})
	if err != nil || len(second.Items) != 1 || !second.More {
		t.Fatalf("second batch = %d items, more=%v, err=%v", len(second.Items), second.More, err)
	}
	third, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: map[string]domain.Checkpoint{second.Checkpoints[0].Stream: second.Checkpoints[0]}, Limit: 1})
	if err != nil || len(third.Items) != 1 || third.More {
		t.Fatalf("third batch = %d items, more=%v, err=%v", len(third.Items), third.More, err)
	}
	if len(third.Items[0].Body) > domain.MaxItemBodyBytes {
		t.Fatalf("body length %d exceeds domain cap", len(third.Items[0].Body))
	}
	var final cursor
	if err := json.Unmarshal(third.Checkpoints[0].Data, &final); err != nil || final.LastScan != 0 || final.Watermark == nil || !final.Watermark.Equal(base.Add(10*time.Minute)) {
		t.Fatalf("final checkpoint = %s (%v)", third.Checkpoints[0].Data, err)
	}
	if third.Items[0].URL != "" {
		t.Fatalf("non-http URL was accepted: %q", third.Items[0].URL)
	}

	// Identical configured server identity and record JSON yields a stable ID.
	source2 := mustMemorySource(t, conn, toolBuilder, func() time.Time { return base.Add(time.Hour) })
	repeat, err := source2.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 1})
	if err != nil || repeat.Items[0].Ref.ExternalID != first.Items[0].Ref.ExternalID {
		t.Fatalf("unstable ID: %v %v", repeat, err)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if !reflect.DeepEqual(calls, []string{"search_docs", "search_docs", "search_docs", "search_docs"}) {
		t.Fatalf("called tools = %v", calls)
	}
}

func TestReadWatermarkOverlapAndSince(t *testing.T) {
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	toolBuilder := func(server *mcp.Server) {
		addServerTool(server, "list_records", objectSchema(nil), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{StructuredContent: []any{
				map[string]any{"id": "old", "updated_at": base.Format(time.RFC3339)},
				map[string]any{"id": "edge", "updated_at": base.Add(5 * time.Minute).Format(time.RFC3339)},
				map[string]any{"id": "new", "updated_at": base.Add(10 * time.Minute).Format(time.RFC3339)},
			}}, nil
		})
	}
	conn := stdioConnection(nil)
	source := mustMemorySource(t, conn, toolBuilder, func() time.Time { return base.Add(time.Hour) })
	full, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil || len(full.Items) != 3 || full.More {
		t.Fatalf("initial read = %d items, %v, err=%v", len(full.Items), full.More, err)
	}
	watermark := full.Checkpoints[0]
	pass, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: map[string]domain.Checkpoint{watermark.Stream: watermark}})
	if err != nil || len(pass.Items) != 2 {
		t.Fatalf("overlap read = %d items, err=%v", len(pass.Items), err)
	}
	if pass.Items[0].Title != "edge" || pass.Items[1].Title != "new" {
		t.Fatalf("overlap results = %q, %q", pass.Items[0].Title, pass.Items[1].Title)
	}
	since := base.Add(7 * time.Minute)
	narrow, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: map[string]domain.Checkpoint{watermark.Stream: watermark}, Since: &since})
	if err != nil || len(narrow.Items) != 1 || narrow.Items[0].Title != "new" {
		t.Fatalf("Since read = %+v, err=%v", narrow.Items, err)
	}
}

func TestMalformedCursorAndCanceledRead(t *testing.T) {
	conn := stdioConnection(nil)
	source := mustMemorySource(t, conn, func(server *mcp.Server) {
		addServerTool(server, "list_records", objectSchema(nil), func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}, time.Now)
	stream := source.stream
	for _, data := range []string{"{", `{"lastScan":-1}`, `{"watermark":null,"lastScan":0} {}`} {
		cp := domain.Checkpoint{Stream: stream, Version: streamVersion, Data: []byte(data)}
		if _, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: map[string]domain.Checkpoint{stream: cp}}); err == nil {
			t.Errorf("accepted malformed cursor %q", data)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := source.Read(ctx, ports.ReadRequest{Connection: conn})
	if err == nil {
		t.Fatal("Read ignored context deadline")
	}
}

func TestHTTPTransportBearerAndRedirectRefusal(t *testing.T) {
	var authMu sync.Mutex
	var gotAuth []string
	server := testMCPServer(t, func(server *mcp.Server) {
		addServerTool(server, "list_docs", objectSchema(nil), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{StructuredContent: []any{map[string]any{"id": "http1", "text": "from http"}}}, nil
		})
	}, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authMu.Lock()
			gotAuth = append(gotAuth, r.Header.Get("Authorization"))
			authMu.Unlock()
			if r.Header.Get("Authorization") != "Bearer secret-token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	t.Cleanup(server.Close)
	conn := httpConnection(server.URL)
	source, err := NewSource(conn, staticCredentials{"secret-token"}, nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if err := source.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil || len(batch.Items) != 1 {
		t.Fatalf("Read = %+v, err=%v", batch, err)
	}
	authMu.Lock()
	for _, value := range gotAuth {
		if value != "Bearer secret-token" {
			t.Errorf("Authorization = %q", value)
		}
	}
	authMu.Unlock()

	var targetCalls int
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	redirectSource, err := NewSource(httpConnection(redirect.URL), nil, nil)
	if err != nil {
		t.Fatalf("redirect source: %v", err)
	}
	if err := redirectSource.Check(context.Background()); err == nil {
		t.Fatal("Check followed or accepted redirect")
	}
	if targetCalls != 0 {
		t.Fatalf("redirect target received %d requests", targetCalls)
	}
}

func TestStdioCommandTransport(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stdio test helper process is skipped on Windows")
	}
	t.Setenv("LV24_MCP_STDIO_HELPER", "1")
	conn := stdioConnection(map[string]string{"tools": "stdio_search"})
	credentials := stdioCredentials{command: os.Args[0], args: "-test.run=^TestMCPStdioHelper$"}
	source, err := NewSource(conn, credentials, nil)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatalf("Read via stdio: %v", err)
	}
	if len(batch.Items) != 1 || batch.Items[0].Title != "stdio item" {
		t.Fatalf("stdio items = %+v", batch.Items)
	}
}

// TestMCPStdioHelper is invoked only by TestStdioCommandTransport's child.
func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("LV24_MCP_STDIO_HELPER") != "1" {
		return
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
	addServerTool(server, "stdio_search", objectSchema(nil), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: []any{map[string]any{"id": "stdio-1", "title": "stdio item"}}}, nil
	})
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestSourceContract(t *testing.T) {
	fixture := &contractFixture{now: time.Now().UTC().Truncate(time.Second)}
	fixture.add(2)
	connection := stdioConnection(nil)
	harness := ingestiontest.Harness{
		Connection: connection,
		NewSource: func(conn domain.Connection) ports.Source {
			source, err := NewSource(conn, nil, func() time.Time { return fixture.now })
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			source.transportFactory = func(ctx context.Context, _ string) (mcp.Transport, func(), error) {
				clientTransport, serverTransport := mcp.NewInMemoryTransports()
				server := mcp.NewServer(&mcp.Implementation{Name: "contract", Version: "1"}, nil)
				addServerTool(server, "list_records", objectSchema(map[string]any{"query": map[string]any{"type": "string"}}, "query"), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{StructuredContent: map[string]any{"records": fixture.snapshot()}}, nil
				})
				serverSession, err := server.Connect(ctx, serverTransport, nil)
				if err != nil {
					return nil, nil, err
				}
				return clientTransport, func() { _ = serverSession.Close() }, nil
			}
			return source
		},
		AddItem:    func() { fixture.add(1) },
		AddMany:    func(n int) { fixture.add(n) },
		UpdateItem: fixture.update,
		PageSize:   2,
	}
	ingestiontest.RunSourceSuite(t, harness)
}

type contractFixture struct {
	mu    sync.Mutex
	items []map[string]any
	next  int
	now   time.Time
}

func (f *contractFixture) add(count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for range count {
		f.next++
		when := f.now.Add(-time.Minute).Add(time.Duration(f.next) * time.Second)
		f.items = append(f.items, map[string]any{
			"id": fmt.Sprintf("item-%04d", f.next), "title": fmt.Sprintf("Item %04d", f.next),
			"text": fmt.Sprintf("body %04d", f.next), "updated_at": when.Format(time.RFC3339Nano),
		})
	}
}

func (f *contractFixture) update() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.items) == 0 {
		return
	}
	f.items[0] = cloneRecord(f.items[0])
	f.items[0]["text"] = "updated body"
	f.items[0]["updated_at"] = f.now.Add(time.Minute).Format(time.RFC3339Nano)
}

func (f *contractFixture) snapshot() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]any, 0, len(f.items))
	for _, record := range f.items {
		result = append(result, cloneRecord(record))
	}
	return result
}

func cloneRecord(source map[string]any) map[string]any {
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func mustMemorySource(t *testing.T, conn domain.Connection, builder func(*mcp.Server), now func() time.Time) *Source {
	t.Helper()
	source, err := NewSource(conn, nil, now)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	source.transportFactory = func(ctx context.Context, _ string) (mcp.Transport, func(), error) {
		clientTransport, serverTransport := mcp.NewInMemoryTransports()
		server := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
		builder(server)
		serverSession, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			return nil, nil, err
		}
		return clientTransport, func() { _ = serverSession.Close() }, nil
	}
	return source
}

func addServerTool(server *mcp.Server, name string, schema any, handler mcp.ToolHandler) {
	server.AddTool(&mcp.Tool{Name: name, InputSchema: schema}, handler)
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		requiredValues := make([]any, len(required))
		for i, name := range required {
			requiredValues[i] = name
		}
		schema["required"] = requiredValues
	}
	return schema
}

func testMCPServer(t *testing.T, build func(*mcp.Server), wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "http-test", Version: "1"}, nil)
	build(server)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	var httpHandler http.Handler = handler
	if wrap != nil {
		httpHandler = wrap(httpHandler)
	}
	return httptest.NewServer(httpHandler)
}

func stdioConnection(extra map[string]string) domain.Connection {
	settings := map[string]string{"transport": "stdio"}
	for key, value := range extra {
		settings[key] = value
	}
	return domain.Connection{ID: "mcp-test", Provider: Name, Settings: settings}
}

func httpConnection(endpoint string) domain.Connection {
	return domain.Connection{ID: "mcp-http", Provider: Name, Settings: map[string]string{"transport": "http", "url": endpoint}}
}

type staticCredentials struct{ token string }

func (c staticCredentials) Get(context.Context, string, string) (string, error) { return c.token, nil }

type countingCredentials struct{ calls int }

func (c *countingCredentials) Get(context.Context, string, string) (string, error) {
	c.calls++
	return "should-not-be-read", nil
}

func TestDenyListAndToolNameValidation(t *testing.T) {
	for _, name := range []string{"search_and_remove", "LIST_and_purge", "find_and_archive", "query_delete", "search_and_archive", "recent_uploads"} {
		if !deniedTool(name) {
			t.Fatalf("deniedTool(%q) = false", name)
		}
	}
	for _, name := range []string{"search_pages", "list_recent", "find_items"} {
		if deniedTool(name) {
			t.Fatalf("deniedTool(%q) = true", name)
		}
	}
	for _, name := range []string{"search pages", "\uff53\uff45\uff41\uff52\uff43\uff48", "search/pages", "search\npages"} {
		if validToolName(name) {
			t.Fatalf("validToolName(%q) = true", name)
		}
	}
	if !validToolName("search_pages.v2") || !validToolName("search-pages") {
		t.Fatal("valid ASCII tool names were rejected")
	}
	destructive := true
	tool := &mcp.Tool{Name: "search_pages", Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive}}
	if !destructiveTool(tool) {
		t.Fatal("destructive annotation was ignored")
	}
	if destructiveTool(&mcp.Tool{Name: "search_pages"}) {
		t.Fatal("plain read tool was denied")
	}
}

func TestStdioCommandComesFromCredentials(t *testing.T) {
	cases := []struct {
		name    string
		command string
		args    string
		wantErr bool
	}{
		{"valid", "/usr/local/bin/server", "--root\n/notes", false},
		{"missing", "", "", true},
		{"relative path", "./server", "", true},
		{"newline", "/usr/bin/a\nb", "", true},
		{"args nul", "/usr/bin/server", "a\x00b", true},
		{"too many args", "/usr/bin/server", strings.Repeat("x\n", 65), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := stdioConnection(nil)
			source, err := NewSource(conn, stdioCredentials{command: tc.command, args: tc.args}, nil)
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			command, args, err := source.stdioConfig(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatalf("stdioConfig = %q, %q; want error", command, args)
				}
				return
			}
			if err != nil || command != tc.command || len(args) != 2 || args[0] != "--root" || args[1] != "/notes" {
				t.Fatalf("stdioConfig = %q, %q, %v", command, args, err)
			}
		})
	}
}

type stdioCredentials struct{ command, args string }

func (c stdioCredentials) Get(_ context.Context, _ string, field string) (string, error) {
	switch field {
	case "command":
		if c.command == "" {
			return "", domain.ErrNotFound
		}
		return c.command, nil
	case "args":
		return c.args, nil
	default:
		return "", domain.ErrNotFound
	}
}

func TestWatermarkDoesNotAdvanceOnEmptyOrFutureScans(t *testing.T) {
	base := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	calls := 0
	source := mustMemorySource(t, stdioConnection(nil), func(server *mcp.Server) {
		addServerTool(server, "list_records", objectSchema(nil), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls++
			if calls == 1 {
				return &mcp.CallToolResult{StructuredContent: map[string]any{"records": []any{}}}, nil
			}
			return &mcp.CallToolResult{StructuredContent: map[string]any{"records": []any{
				map[string]any{"id": "future", "title": "Future", "updated_at": base.Add(48 * time.Hour).Format(time.RFC3339)},
			}}}, nil
		})
	}, func() time.Time { return base })
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: stdioConnection(nil)})
	if err != nil {
		t.Fatalf("first Read: %v", err)
	}
	checkpoints := map[string]domain.Checkpoint{}
	for _, checkpoint := range first.Checkpoints {
		checkpoints[checkpoint.Stream] = checkpoint
	}
	var cur cursor
	if err := json.Unmarshal(first.Checkpoints[0].Data, &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Watermark != nil {
		t.Fatalf("empty scan advanced the watermark: %s", cur.Watermark)
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: stdioConnection(nil), Checkpoints: checkpoints})
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if len(second.Items) != 0 {
		t.Fatalf("future record was emitted: %+v", second.Items)
	}
	if err := json.Unmarshal(second.Checkpoints[0].Data, &cur); err != nil {
		t.Fatal(err)
	}
	if cur.Watermark != nil {
		t.Fatalf("future record advanced the watermark: %s", cur.Watermark)
	}
}

func TestOneFailingToolDoesNotAbortTheRead(t *testing.T) {
	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	source := mustMemorySource(t, stdioConnection(nil), func(server *mcp.Server) {
		addServerTool(server, "list_broken", objectSchema(nil), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "boom"}}}, nil
		})
		addServerTool(server, "list_good", objectSchema(nil), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{StructuredContent: []any{map[string]any{"id": "good", "title": "Good", "updated_at": base.Format(time.RFC3339)}}}, nil
		})
	}, func() time.Time { return base.Add(time.Hour) })
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: stdioConnection(nil)})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Items) != 1 || batch.Items[0].Title != "Good" {
		t.Fatalf("items = %+v", batch.Items)
	}
	if got := source.SkippedTools(); !reflect.DeepEqual(got, []string{"list_broken"}) {
		t.Fatalf("skipped tools = %v", got)
	}
}
