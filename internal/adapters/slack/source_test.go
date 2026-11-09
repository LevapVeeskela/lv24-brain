package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ingestiontest"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const testToken = "xoxb-test-secret"

type testCredentials struct{ token string }

func (c testCredentials) Get(_ context.Context, connectionID, field string) (string, error) {
	if connectionID != "slack-test" || field != "token" || c.token == "" {
		return "", domain.ErrNotFound
	}
	return c.token, nil
}

func testNow() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) }

func testConnection(serverURL, settings string) domain.Connection {
	return domain.Connection{
		ID: "slack-test", Provider: Name, BaseURL: serverURL,
		Settings: map[string]string{"channels": settings},
	}
}

func makeSource(t *testing.T, conn domain.Connection, client *http.Client, now func() time.Time) *Source {
	t.Helper()
	source, err := NewSource(conn, testCredentials{token: testToken}, client, now)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	return source
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(err)
	}
}

func slackMessageAt(ts, text string) slackMessage {
	return slackMessage{Type: "message", TS: ts, Text: text, User: "U123", UserProfile: profile{DisplayName: "Alice"}}
}

func checkpointMap(batch ports.ReadBatch) map[string]domain.Checkpoint {
	out := make(map[string]domain.Checkpoint, len(batch.Checkpoints))
	for _, checkpoint := range batch.Checkpoints {
		out[checkpoint.Stream] = checkpoint
	}
	return out
}

func TestDescriptorAndExplicitChannelOptIn(t *testing.T) {
	desc := Descriptor()
	if err := desc.Validate(); err != nil {
		t.Fatalf("descriptor invalid: %v", err)
	}
	if desc.Provider != "slack" || desc.DisplayName != "Slack" || desc.DefaultPublish != domain.PublishLocalOnly || !desc.PersonalData {
		t.Fatalf("unexpected descriptor: %+v", desc)
	}
	if !desc.Capabilities.Backfill || !desc.Capabilities.Incremental || desc.Capabilities.Deletes || len(desc.Capabilities.AuthModes) != 1 || desc.Capabilities.AuthModes[0] != domain.AuthToken {
		t.Fatalf("unexpected capabilities: %+v", desc.Capabilities)
	}
	field, ok := desc.Field("channels")
	if !ok || !field.Required || !strings.Contains(field.Help, "explicit operator opt-in") {
		t.Fatalf("channels setting does not document opt-in: %+v", field)
	}

	var channels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q", got)
		}
		if strings.Contains(r.URL.RawQuery, testToken) {
			t.Error("token appeared in request URL")
		}
		if r.URL.Path != "/api/conversations.history" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		channels = append(channels, r.URL.Query().Get("channel"))
		writeJSON(w, slackResponse{OK: true, Messages: []slackMessage{slackMessageAt("1791547140.000001", "one")}})
	}))
	defer server.Close()
	conn := testConnection(server.URL+"/api", "C123")
	source := makeSource(t, conn, server.Client(), testNow)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Items) != 1 || len(channels) != 1 || channels[0] != "C123" {
		t.Fatalf("read was not restricted to the explicitly listed channel: items=%d channels=%v", len(batch.Items), channels)
	}
}

func TestHistoryLimitAndTwoPageReadBound(t *testing.T) {
	var requests int
	var gotLimits []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/conversations.history" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotLimits = append(gotLimits, r.URL.Query().Get("limit"))
		if r.URL.Query().Get("limit") != "15" {
			t.Errorf("Slack history limit = %q; want 15", r.URL.Query().Get("limit"))
		}
		page := requests
		messages := make([]slackMessage, 15)
		for i := range messages {
			messages[i] = slackMessageAt(fmt.Sprintf("%d.000001", int64(1791547200-page*15-i)), fmt.Sprintf("message %d", page*15+i))
		}
		response := slackResponse{OK: true, Messages: messages}
		if page == 1 {
			response.HasMore = true
			response.Metadata.NextCursor = "page-two"
		}
		writeJSON(w, response)
	}))
	defer server.Close()
	conn := testConnection(server.URL+"/api", "C123")
	conn.Settings["limit"] = "30"
	source := makeSource(t, conn, server.Client(), testNow)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 30})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(gotLimits) != 2 || len(batch.Items) != 30 || batch.More {
		t.Fatalf("read bound/results: requests=%d limits=%v items=%d more=%v", requests, gotLimits, len(batch.Items), batch.More)
	}
}

func TestThreadReplyCapAndUTF8Bounds(t *testing.T) {
	var replyLimit string
	var replyCalls int
	long := strings.Repeat("🙂", 6000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/conversations.history":
			root := slackMessageAt("1791547140.000001", "Decision\nMore detail")
			root.ReplyCount = 8
			writeJSON(w, slackResponse{OK: true, Messages: []slackMessage{root}})
		case "/api/conversations.replies":
			replyCalls++
			replyLimit = r.URL.Query().Get("limit")
			replies := []slackMessage{{TS: "1791547140.000001", Text: "root"}}
			for i := 0; i < 8; i++ {
				replies = append(replies, slackMessage{TS: fmt.Sprintf("179154714%d.00000%d", 1+i, i), Text: long, User: "Ureply"})
			}
			writeJSON(w, slackResponse{OK: true, Messages: replies})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	conn := testConnection(server.URL+"/api", "C123")
	source := makeSource(t, conn, server.Client(), testNow)
	batch, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if replyCalls != 1 || replyLimit != "5" {
		t.Fatalf("reply request calls=%d limit=%q; want one page capped at five", replyCalls, replyLimit)
	}
	if len(batch.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(batch.Items))
	}
	item := batch.Items[0]
	if item.Title != "Decision" || !utf8.ValidString(item.Body) || len(item.Body) > maxBodyBytes {
		t.Fatalf("title/body bounds invalid: title=%q bytes=%d utf8=%v", item.Title, len(item.Body), utf8.ValidString(item.Body))
	}
	if strings.Count(item.Body, "Ureply") < maxReplies-1 || strings.Count(item.Body, "Ureply") > maxReplies {
		t.Fatalf("included %d replies, want no more than %d", strings.Count(item.Body, "Ureply"), maxReplies)
	}
	if strings.Count(item.Body, "🙂") > maxReplies*(maxReplyBytes/4) {
		t.Fatal("reply text exceeded the per-reply UTF-8 byte cap")
	}
}

func TestCursorSinceOverlapOffsetAndNextCursor(t *testing.T) {
	now := testNow()
	since := now.Add(-time.Hour)
	base := now.Unix()
	var mu sync.Mutex
	var oldest, pageCursor []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		oldest = append(oldest, r.URL.Query().Get("oldest"))
		pageCursor = append(pageCursor, r.URL.Query().Get("cursor"))
		call := len(oldest)
		mu.Unlock()
		switch call {
		case 1, 2:
			writeJSON(w, slackResponse{OK: true, Metadata: responseMetadata{NextCursor: "page-two"}, Messages: []slackMessage{
				slackMessageAt(fmt.Sprintf("%d.000001", base-60), "first"), slackMessageAt(fmt.Sprintf("%d.000002", base-70), "second"),
			}})
		default:
			writeJSON(w, slackResponse{OK: true, Messages: []slackMessage{slackMessageAt(fmt.Sprintf("%d.000003", base-80), "third")}})
		}
	}))
	defer server.Close()
	conn := testConnection(server.URL+"/api", "C123")
	source := makeSource(t, conn, server.Client(), func() time.Time { return now })
	first, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Since: &since, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !first.More || len(first.Items) != 1 {
		t.Fatalf("first batch items=%d more=%v", len(first.Items), first.More)
	}
	firstCursor := checkpointMap(first)
	var saved channelCursor
	if err := json.Unmarshal(firstCursor[streamName("C123")].Data, &saved); err != nil {
		t.Fatal(err)
	}
	wantOldest := slackTS(since.Add(-pageOverlap))
	if oldest[0] != wantOldest || saved.Offset != 1 || saved.NextCursor != "" {
		t.Fatalf("initial cursor: oldest=%q saved=%+v; want oldest %q offset 1", oldest[0], saved, wantOldest)
	}
	second, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: firstCursor, Since: &since, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].Ref.ExternalID != fmt.Sprintf("C123/%d.000002", base-70) || !second.More {
		t.Fatalf("second batch: items=%+v more=%v", second.Items, second.More)
	}
	third, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn, Checkpoints: checkpointMap(second), Since: &since, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Items) != 1 || third.More || pageCursor[2] != "page-two" {
		t.Fatalf("next cursor was not used: items=%d more=%v cursors=%v", len(third.Items), third.More, pageCursor)
	}
}

func TestMalformedCursorRejected(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	conn := testConnection(server.URL+"/api", "C123")
	source := makeSource(t, conn, server.Client(), testNow)
	stream := streamName("C123")
	for _, data := range [][]byte{[]byte("{"), []byte(`{"oldest":"1","latest":"2","lastScan":"2026-11-09T12:00:00Z","nextCursor":"","offset":99,"seen":[]}`)} {
		_, err := source.Read(context.Background(), ports.ReadRequest{
			Connection:  conn,
			Checkpoints: map[string]domain.Checkpoint{stream: {Stream: stream, Version: checkpointVersion, Data: data}},
		})
		if err == nil || !strings.Contains(err.Error(), "checkpoint") {
			t.Fatalf("malformed cursor error = %v", err)
		}
	}
}

func TestUnauthorizedAndRateLimitedErrorsAreSanitizedAndDoNotRetry(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
					t.Errorf("authorization header = %q", got)
				}
				if status == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "60")
				}
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"error":"echo %s"}`, testToken)
			}))
			defer server.Close()
			conn := testConnection(server.URL+"/api", "C123")
			source := makeSource(t, conn, server.Client(), testNow)
			_, err := source.Read(context.Background(), ports.ReadRequest{Connection: conn})
			if err == nil || strings.Contains(err.Error(), testToken) {
				t.Fatalf("error must fail closed without leaking token: %v", err)
			}
			if status == http.StatusTooManyRequests && !strings.Contains(err.Error(), "60s") {
				t.Fatalf("429 error omitted Retry-After: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("HTTP calls = %d, want exactly one (no busy retry)", calls.Load())
			}
		})
	}
}

func TestDeterministicReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, slackResponse{OK: true, Messages: []slackMessage{slackMessageAt(fmt.Sprintf("%d.123456", testNow().Unix()-60), "A stable message")}})
	}))
	defer server.Close()
	conn := testConnection(server.URL+"/api", "C123")
	source := makeSource(t, conn, server.Client(), testNow)
	req := ports.ReadRequest{Connection: conn}
	a, err := source.Read(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := source.Read(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if a.More != b.More || len(a.Items) != len(b.Items) || len(a.Checkpoints) != len(b.Checkpoints) {
		t.Fatalf("replay shape differs: a=%+v b=%+v", a, b)
	}
	if a.Items[0].ComputeHash() != b.Items[0].ComputeHash() || a.Items[0].CreatedAt != b.Items[0].CreatedAt {
		t.Fatal("replay item differs")
	}
	if string(a.Checkpoints[0].Data) != string(b.Checkpoints[0].Data) {
		t.Fatalf("replay checkpoint differs: %s != %s", a.Checkpoints[0].Data, b.Checkpoints[0].Data)
	}
}

func TestSourceContract(t *testing.T) {
	messageText := atomic.Value{}
	messageText.Store("Initial contract message")
	stamp := fmt.Sprintf("%d.123456", testNow().Unix()-60)
	var nowTick atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/auth.test":
			writeJSON(w, map[string]any{"ok": true, "team": "Example workspace", "user": "contract-user"})
		case "/api/conversations.history":
			writeJSON(w, slackResponse{OK: true, Messages: []slackMessage{slackMessageAt(stamp, messageText.Load().(string))}})
		case "/api/conversations.replies":
			writeJSON(w, slackResponse{OK: true, Messages: []slackMessage{slackMessageAt(r.URL.Query().Get("ts"), "root")}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	conn := testConnection(server.URL+"/api", "Ccontract")
	h := ingestiontest.Harness{
		Connection: conn,
		NewSource: func(connection domain.Connection) ports.Source {
			scanTime := testNow().Add(time.Duration(nowTick.Add(1)) * time.Second)
			source, err := NewSource(connection, testCredentials{token: testToken}, server.Client(), func() time.Time { return scanTime })
			if err != nil {
				t.Fatalf("NewSource: %v", err)
			}
			return source
		},
		UpdateItem: func() { messageText.Store("Updated contract message") },
		PageSize:   1,
	}
	ingestiontest.RunSourceSuite(t, h)
	if Descriptor().Capabilities.Deletes {
		t.Fatal("Slack must not infer or emit deletes")
	}
}

func TestSourceRejectsInsecureNonLoopbackBaseURL(t *testing.T) {
	conn := testConnection("http://slack.example.test/api", "C123")
	if _, err := NewSource(conn, testCredentials{token: testToken}, nil, testNow); err == nil {
		t.Fatal("non-loopback HTTP BaseURL was accepted")
	}
}

func TestConfigParsing(t *testing.T) {
	for _, channels := range []string{"", "C123 bad", "C123, "} {
		conn := testConnection("", channels)
		if _, err := NewSource(conn, testCredentials{token: testToken}, nil, testNow); err == nil {
			t.Errorf("invalid channels %q were accepted", channels)
		}
	}
	conn := testConnection("", "C123\nC456,C123")
	source, err := NewSource(conn, testCredentials{token: testToken}, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.channels) != 2 || source.channels[0] != "C123" || source.channels[1] != "C456" {
		t.Fatalf("channel parsing/deduplication = %v", source.channels)
	}
}

func TestMalformedCredentialIsNotReturned(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	conn := testConnection(server.URL+"/api", "C123")
	source := makeSource(t, conn, server.Client(), testNow)
	source.credentials = testCredentials{token: "not-a-token-secret"}
	err := source.Check(context.Background())
	if err == nil || strings.Contains(err.Error(), "not-a-token-secret") {
		t.Fatalf("invalid token error leaked credential: %v", err)
	}
}

func TestRequestQueryDoesNotContainBearerToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parsed, _ := url.ParseRequestURI(r.RequestURI)
		if strings.Contains(parsed.RawQuery, testToken) || r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("credential not isolated to Authorization header: %s %q", r.RequestURI, r.Header.Get("Authorization"))
		}
		writeJSON(w, map[string]any{"ok": true, "team": "Test workspace", "user": "test-user"})
	}))
	defer server.Close()
	conn := testConnection(server.URL+"/api", "C123")
	if err := makeSource(t, conn, server.Client(), testNow).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}
