package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const testToken = "test-token-value"

func newClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Config{BaseURL: server.URL, Token: testToken, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, payload string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, payload); err != nil {
		t.Fatalf("write response: %v", err)
	}
}

func requireAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("authorization header = %q", got)
	}
}

func TestNewValidatesBaseURL(t *testing.T) {
	for _, raw := range []string{
		"",
		"brain.example.test",
		"ftp://brain.example.test",
		"https://user:pass@brain.example.test",
		"https://brain.example.test?token=x",
		"https://brain.example.test#fragment",
	} {
		if _, err := New(Config{BaseURL: raw}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("New(%q) error = %v, want validation", raw, err)
		}
	}
	client, err := New(Config{BaseURL: "https://brain.example.test/"})
	if err != nil {
		t.Fatalf("New valid: %v", err)
	}
	if client.BaseURL() != "https://brain.example.test" {
		t.Fatalf("base URL = %q", client.BaseURL())
	}
}

func TestGetProject(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/p1" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, `{"id":"p1","name":"Alpha","kind":"app","revision":7}`)
	}))
	project, err := client.GetProject(context.Background(), "p1")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if project.ID != "p1" || project.Name != "Alpha" || project.Kind != domain.ProjectKindApp {
		t.Fatalf("project = %+v", project)
	}
	if _, err := client.GetProject(context.Background(), "  "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("blank id error = %v", err)
	}
}

func TestListProjects(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireAuth(t, r)
		writeJSON(t, w, http.StatusOK, `{"items":[{"id":"p1","name":"Alpha"},{"id":"p2","name":"Beta"}]}`)
	}))
	projects, err := client.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 2 || projects[0].ID != "p1" || projects[1].ID != "p2" {
		t.Fatalf("projects = %+v", projects)
	}
}

func TestPutProjectCreatesWhenMissing(t *testing.T) {
	var created bool
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireAuth(t, r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/p1":
			writeJSON(t, w, http.StatusNotFound, `{"error":{"code":"not_found","message":"resource not found"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects":
			created = true
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !strings.Contains(string(body), `"id":"p1"`) {
				t.Fatalf("create body = %s", body)
			}
			writeJSON(t, w, http.StatusCreated, string(body))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	err := client.PutProject(context.Background(), domain.Project{ID: "p1", Name: "Alpha", Kind: domain.ProjectKindApp})
	if err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	if !created {
		t.Fatal("project was not created")
	}
}

func TestPutProjectUpdatesExistingWithoutRevision(t *testing.T) {
	var patched map[string]any
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireAuth(t, r)
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, http.StatusOK, `{"id":"p1","name":"Alpha","kind":"other","revision":3}`)
		case http.MethodPatch:
			if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
			writeJSON(t, w, http.StatusOK, `{"id":"p1","name":"Alpha 2","kind":"app","revision":4}`)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	err := client.PutProject(context.Background(), domain.Project{ID: "p1", Name: "Alpha 2", Kind: domain.ProjectKindApp})
	if err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	if patched["name"] != "Alpha 2" || patched["kind"] != "app" {
		t.Fatalf("patch body = %v", patched)
	}
	if _, ok := patched["revision"]; ok {
		t.Fatalf("patch body must not set revision: %v", patched)
	}
	if _, ok := patched["id"]; ok {
		t.Fatalf("patch body must not set id: %v", patched)
	}
}

func TestStateRoundTrip(t *testing.T) {
	freshUntil := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireAuth(t, r)
		switch r.Method {
		case http.MethodGet:
			writeJSON(t, w, http.StatusOK, `{"projectId":"p1","goal":"ship","revision":9}`)
		case http.MethodPut:
			var body putStateRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode state: %v", err)
			}
			if body.Goal != "ship" || body.NextAction != "test" || body.FreshUntil == nil {
				t.Fatalf("state body = %+v", body)
			}
			if !body.FreshUntil.Equal(freshUntil) {
				t.Fatalf("freshUntil = %v", body.FreshUntil)
			}
			writeJSON(t, w, http.StatusOK, `{"projectId":"p1","goal":"ship","revision":10,"updatedAt":"2026-11-05T10:00:00Z"}`)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	state, err := client.GetState(context.Background(), "p1")
	if err != nil || state.Goal != "ship" {
		t.Fatalf("GetState = (%+v, %v)", state, err)
	}
	revision, err := client.PutState(context.Background(), domain.State{
		ProjectID: "p1", Goal: "ship", NextAction: "test", FreshUntil: freshUntil,
	})
	if err != nil {
		t.Fatalf("PutState: %v", err)
	}
	if revision.Kind != domain.SyncKindServer || revision.Value != "10" || revision.At.IsZero() {
		t.Fatalf("revision = %+v", revision)
	}
}

func TestAppendOperations(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireAuth(t, r)
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		switch r.URL.Path {
		case "/v1/projects/p1/notes":
			writeJSON(t, w, http.StatusCreated, `{"id":"n1","projectId":"p1","body":"b","revision":1,"createdAt":"2026-11-05T10:00:00Z"}`)
		case "/v1/projects/p1/decisions":
			writeJSON(t, w, http.StatusCreated, `{"id":"d1","projectId":"p1","title":"t","revision":2,"createdAt":"2026-11-05T10:00:01Z"}`)
		case "/v1/projects/p1/attempts":
			writeJSON(t, w, http.StatusCreated, `{"id":"a1","projectId":"p1","title":"t","revision":3,"createdAt":"2026-11-05T10:00:02Z"}`)
		case "/v1/projects/p1/handoffs":
			writeJSON(t, w, http.StatusCreated, `{"id":"h1","projectId":"p1","summary":"s","revision":4,"createdAt":"2026-11-05T10:00:03Z"}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))

	noteRevision, err := client.AppendNote(context.Background(), domain.Note{ProjectID: "p1", Body: "body"})
	if err != nil || noteRevision.Value != "1" {
		t.Fatalf("AppendNote = (%+v, %v)", noteRevision, err)
	}
	decisionRevision, err := client.AppendDecision(context.Background(), domain.Decision{ProjectID: "p1", Title: "title"})
	if err != nil || decisionRevision.Value != "2" {
		t.Fatalf("AppendDecision = (%+v, %v)", decisionRevision, err)
	}
	attemptRevision, err := client.AppendAttempt(context.Background(), domain.Attempt{ProjectID: "p1", Title: "title"})
	if err != nil || attemptRevision.Value != "3" {
		t.Fatalf("AppendAttempt = (%+v, %v)", attemptRevision, err)
	}
	handoffRevision, err := client.AppendHandoff(context.Background(), domain.Handoff{ProjectID: "p1", Summary: "summary"})
	if err != nil || handoffRevision.Value != "4" {
		t.Fatalf("AppendHandoff = (%+v, %v)", handoffRevision, err)
	}
}

func TestCurrentRevision(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireAuth(t, r)
		if r.URL.Path != "/v1/revisions" || r.URL.Query().Get("limit") != "1" {
			t.Fatalf("request = %s", r.URL.String())
		}
		writeJSON(t, w, http.StatusOK, `{"items":[{"id":42,"kind":"server","createdAt":"2026-11-05T10:00:00Z"}]}`)
	}))
	revision, err := client.CurrentRevision(context.Background())
	if err != nil || revision.Value != "42" || revision.Kind != domain.SyncKindServer {
		t.Fatalf("CurrentRevision = (%+v, %v)", revision, err)
	}

	empty := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, `{"items":[]}`)
	}))
	revision, err = empty.CurrentRevision(context.Background())
	if err != nil || revision.Value != "0" {
		t.Fatalf("empty CurrentRevision = (%+v, %v)", revision, err)
	}
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    error
		wantStatus int
		wantRev    int64
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":{"code":"unauthorized","message":"authentication required"}}`, wantErr: ErrUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":{"code":"forbidden","message":"insufficient role"}}`, wantErr: ErrForbidden},
		{name: "not found", status: http.StatusNotFound, body: `{"error":{"code":"not_found","message":"resource not found"}}`, wantErr: domain.ErrNotFound},
		{name: "validation", status: http.StatusBadRequest, body: `{"error":{"code":"invalid_argument","message":"body: invalid JSON"}}`, wantErr: domain.ErrValidation},
		{name: "conflict", status: http.StatusConflict, body: `{"error":{"code":"conflict","message":"revision conflict","currentRevision":12}}`, wantErr: domain.ErrConflict, wantRev: 12},
		{name: "internal", status: http.StatusInternalServerError, body: `{"error":{"code":"internal","message":"internal server error"}}`, wantStatus: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requireAuth(t, r)
				writeJSON(t, w, test.status, test.body)
			}))
			_, err := client.GetProject(context.Background(), "p1")
			if err == nil {
				t.Fatal("expected an error")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantRev > 0 {
				var conflict *ConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != test.wantRev {
					t.Fatalf("conflict = %+v (err %v)", conflict, err)
				}
			}
			if test.wantStatus > 0 {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != test.wantStatus {
					t.Fatalf("api error = %+v (err %v)", apiErr, err)
				}
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeJSON(t, w, http.StatusOK, `{}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Token: testToken, Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.GetProject(context.Background(), "p1")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want timeout", err)
	}
}

func TestContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.GetProject(ctx, "p1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
}

func TestErrorRedaction(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusInternalServerError,
			`{"error":{"code":"internal","message":"failed with Bearer `+testToken+` and `+testToken+`"}}`)
	}))
	_, err := client.GetProject(context.Background(), "p1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked the token: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("error did not show redaction marker: %v", err)
	}
}

func TestTokenProvider(t *testing.T) {
	provider := staticProvider{token: "provider-token"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer provider-token" {
			t.Fatalf("authorization header = %q", got)
		}
		writeJSON(t, w, http.StatusOK, `{"id":"p1","name":"Alpha"}`)
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, TokenProvider: provider, Timeout: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.GetProject(context.Background(), "p1"); err != nil {
		t.Fatalf("GetProject: %v", err)
	}
}

type staticProvider struct{ token string }

func (p staticProvider) Token(context.Context) (string, error) { return p.token, nil }
