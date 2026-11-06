package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	adminLogin     = "admin@example.test"
	adminPassword  = "admin-password-1"
	viewerLogin    = "viewer@example.test"
	viewerPassword = "viewer-password-1"
)

var fastArgon = auth.Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}

func testConfig() Config {
	return Config{
		Secret:         []byte(strings.Repeat("k", 32)),
		Issuer:         "test-issuer",
		Audience:       "test-audience",
		AccessTTL:      5 * time.Minute,
		RefreshTTL:     time.Hour,
		TokenPrefix:    "brain_",
		MaxBodyBytes:   4096,
		SSEHeartbeat:   20 * time.Millisecond,
		SSERetry:       10 * time.Millisecond,
		LoginRateLimit: 0,
		PasswordParams: fastArgon,
		Now:            time.Now,
	}
}

func newTestServer(t *testing.T, mutate func(*Config)) (*httptest.Server, *API, *MemoryStore) {
	t.Helper()
	cfg := testConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	store := NewMemoryStore()
	hash, err := auth.HashPassword(adminPassword, fastArgon)
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	if _, _, err := store.CreateUser(context.Background(), domain.User{
		Login:       adminLogin,
		DisplayName: "Administrator",
		Role:        domain.RoleAdmin,
		AuthSource:  "local",
	}, hash); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	api, err := New(cfg, store)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		ts.Close()
		api.Close()
	})
	return ts, api, store
}

func jsonRequest(t *testing.T, method, target string, body any, token string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return resp, payload
}

func requireStatus(t *testing.T, resp *http.Response, payload []byte, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, want, payload)
	}
}

func decodeSession(t *testing.T, payload []byte) sessionResponse {
	t.Helper()
	var session sessionResponse
	if err := json.Unmarshal(payload, &session); err != nil {
		t.Fatalf("decode session: %v; body = %s", err, payload)
	}
	if session.AccessToken == "" || session.RefreshToken == "" {
		t.Fatalf("session tokens are empty: %s", payload)
	}
	return session
}

func loginAs(t *testing.T, baseURL, login, password string) sessionResponse {
	t.Helper()
	resp, payload := jsonRequest(t, http.MethodPost, baseURL+"/v1/auth/login", map[string]string{
		"login":    login,
		"password": password,
	}, "")
	requireStatus(t, resp, payload, http.StatusOK)
	return decodeSession(t, payload)
}

func TestHealthVersionAndOpenAPI(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	for _, path := range []string{"/health", "/ready", "/version", "/openapi.json"} {
		resp, payload := jsonRequest(t, http.MethodGet, ts.URL+path, nil, "")
		requireStatus(t, resp, payload, http.StatusOK)
		if path == "/version" && !strings.Contains(string(payload), "\"version\"") {
			t.Fatalf("version payload = %s", payload)
		}
	}
	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/openapi.json", nil, "")
	requireStatus(t, resp, payload, http.StatusOK)
	var spec map[string]any
	if err := json.Unmarshal(payload, &spec); err != nil {
		t.Fatalf("openapi is not JSON: %v", err)
	}
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("openapi has no paths object")
	}
	if _, ok := paths["/v1/events"]; !ok {
		t.Fatal("openapi is missing /v1/events")
	}
	for _, path := range []string{
		"/v1/connectors/run",
		"/v1/connectors/runs",
		"/v1/connectors/runs/{id}",
		"/v1/connectors/runs/{id}/markdown",
	} {
		if _, ok := paths[path]; !ok {
			t.Fatalf("openapi is missing %s", path)
		}
	}
}

func TestUnauthenticatedRequests(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, "")
	requireStatus(t, resp, payload, http.StatusUnauthorized)
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate header")
	}
}

func TestLoginMeRefreshRotationAndLogout(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	first := loginAs(t, ts.URL, adminLogin, adminPassword)

	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/auth/me", nil, first.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	if !strings.Contains(string(payload), adminLogin) {
		t.Fatalf("me payload = %s", payload)
	}

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/refresh", map[string]string{
		"refreshToken": first.RefreshToken,
	}, "")
	requireStatus(t, resp, payload, http.StatusOK)
	second := decodeSession(t, payload)
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token did not rotate")
	}

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/refresh", map[string]string{
		"refreshToken": first.RefreshToken,
	}, "")
	requireStatus(t, resp, payload, http.StatusUnauthorized)

	// Reuse detection revokes the whole family, including the rotated token.
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/refresh", map[string]string{
		"refreshToken": second.RefreshToken,
	}, "")
	requireStatus(t, resp, payload, http.StatusUnauthorized)

	third := loginAs(t, ts.URL, adminLogin, adminPassword)
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/logout", nil, third.AccessToken)
	requireStatus(t, resp, payload, http.StatusNoContent)
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/refresh", map[string]string{
		"refreshToken": third.RefreshToken,
	}, "")
	requireStatus(t, resp, payload, http.StatusUnauthorized)
}

func TestRoleDenialAndAPIViews(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/users", map[string]any{
		"login":    viewerLogin,
		"password": viewerPassword,
		"role":     "viewer",
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)

	viewer := loginAs(t, ts.URL, viewerLogin, viewerPassword)

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, viewer.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{
		"id":   "denied-project",
		"name": "Denied",
	}, viewer.AccessToken)
	requireStatus(t, resp, payload, http.StatusForbidden)

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/users", nil, viewer.AccessToken)
	requireStatus(t, resp, payload, http.StatusForbidden)

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/tokens", map[string]any{
		"name": "escalate",
		"role": "admin",
	}, viewer.AccessToken)
	requireStatus(t, resp, payload, http.StatusForbidden)

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/tokens", map[string]any{
		"name": "cli",
		"role": "viewer",
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)
	var tokenResponse createTokenResponse
	if err := json.Unmarshal(payload, &tokenResponse); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if !strings.HasPrefix(tokenResponse.Token, "brain_") {
		t.Fatalf("token = %q", tokenResponse.Token)
	}

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, tokenResponse.Token)
	requireStatus(t, resp, payload, http.StatusOK)
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{
		"id":   "api-token-denied",
		"name": "Denied",
	}, tokenResponse.Token)
	requireStatus(t, resp, payload, http.StatusForbidden)

	resp, payload = jsonRequest(t, http.MethodDelete, ts.URL+"/v1/tokens/"+tokenResponse.APIToken.ID, nil, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusNoContent)
	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, tokenResponse.Token)
	requireStatus(t, resp, payload, http.StatusUnauthorized)
}

func TestOptimisticConflictReturnsCurrentRevision(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{
		"id":   "conflict-project",
		"name": "Alpha",
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)
	var created ProjectRecord
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	firstRevision := created.Revision

	resp, payload = jsonRequest(t, http.MethodPatch, ts.URL+"/v1/projects/conflict-project", map[string]any{
		"name":     "Beta",
		"revision": firstRevision,
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)
	var updated ProjectRecord
	if err := json.Unmarshal(payload, &updated); err != nil {
		t.Fatalf("decode updated project: %v", err)
	}
	if updated.Revision <= firstRevision {
		t.Fatalf("revision did not advance: %d -> %d", firstRevision, updated.Revision)
	}

	resp, payload = jsonRequest(t, http.MethodPatch, ts.URL+"/v1/projects/conflict-project", map[string]any{
		"name":     "Gamma",
		"revision": firstRevision,
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusConflict)
	var envelope errorEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("decode conflict: %v", err)
	}
	if envelope.Error.Code != "conflict" || envelope.Error.CurrentRevision == nil {
		t.Fatalf("conflict payload = %s", payload)
	}
	if *envelope.Error.CurrentRevision != updated.Revision {
		t.Fatalf("currentRevision = %d, want %d", *envelope.Error.CurrentRevision, updated.Revision)
	}
	if envelope.Error.MergeHint == "" {
		t.Fatal("conflict is missing a merge hint")
	}
}

func TestBodyLimit(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	oversized := strings.Repeat("x", 8000)
	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/login", map[string]string{
		"login":    adminLogin,
		"password": oversized,
	}, "")
	requireStatus(t, resp, payload, http.StatusRequestEntityTooLarge)
}

func TestKnowledgeCRUDSearchAndState(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)
	authHeader := admin.AccessToken

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{
		"id":   "knowledge-project",
		"name": "Knowledge",
	}, authHeader)
	requireStatus(t, resp, payload, http.StatusCreated)

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/knowledge-project/notes", map[string]any{
		"title": "Release checklist",
		"body":  "remember the deploy freeze",
		"tags":  []string{"release"},
	}, authHeader)
	requireStatus(t, resp, payload, http.StatusCreated)
	var note NoteRecord
	if err := json.Unmarshal(payload, &note); err != nil {
		t.Fatalf("decode note: %v", err)
	}

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects/knowledge-project/notes", nil, authHeader)
	requireStatus(t, resp, payload, http.StatusOK)
	if !strings.Contains(string(payload), "Release checklist") {
		t.Fatalf("notes list = %s", payload)
	}

	resp, payload = jsonRequest(t, http.MethodPatch, ts.URL+"/v1/notes/"+note.ID, map[string]any{
		"title":    "Release checklist v2",
		"body":     "remember the deploy freeze and the rollback drill",
		"revision": note.Revision,
	}, authHeader)
	requireStatus(t, resp, payload, http.StatusOK)
	var updated NoteRecord
	if err := json.Unmarshal(payload, &updated); err != nil {
		t.Fatalf("decode note update: %v", err)
	}
	if updated.Revision <= note.Revision {
		t.Fatalf("note revision did not advance: %d -> %d", note.Revision, updated.Revision)
	}

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/search?q=rollback", nil, authHeader)
	requireStatus(t, resp, payload, http.StatusOK)
	if !strings.Contains(string(payload), note.ID) {
		t.Fatalf("search did not find the note: %s", payload)
	}

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects/knowledge-project/state", nil, authHeader)
	requireStatus(t, resp, payload, http.StatusNotFound)

	resp, payload = jsonRequest(t, http.MethodPut, ts.URL+"/v1/projects/knowledge-project/state", map[string]any{
		"goal":       "ship the server",
		"active":     []string{"auth"},
		"nextAction": "soak the SSE stream",
	}, authHeader)
	requireStatus(t, resp, payload, http.StatusOK)
	if !strings.Contains(string(payload), "ship the server") {
		t.Fatalf("state payload = %s", payload)
	}

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/knowledge-project/decisions", map[string]any{
		"title":   "Single node first",
		"status":  "accepted",
		"context": "Postgres is wired in a later work package",
	}, authHeader)
	requireStatus(t, resp, payload, http.StatusCreated)

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/knowledge-project/attempts", map[string]any{
		"title":      "Try in-memory store",
		"hypothesis": "enough for the HTTP contract",
		"result":     "works",
	}, authHeader)
	requireStatus(t, resp, payload, http.StatusCreated)

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/knowledge-project/handoffs", map[string]any{
		"summary":    "HTTP surface landed",
		"nextAction": "wire Postgres",
	}, authHeader)
	requireStatus(t, resp, payload, http.StatusCreated)
}

func TestSSEHeadersHeartbeatAndReconnect(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{
		"id":   "sse-project-1",
		"name": "First",
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)

	stream := openSSE(t, ts.URL, admin.AccessToken, "0")
	defer stream.Close()
	if contentType := stream.Response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	if cache := stream.Response.Header.Get("Cache-Control"); !strings.Contains(cache, "no-cache") {
		t.Fatalf("Cache-Control = %q", cache)
	}
	if buffering := stream.Response.Header.Get("X-Accel-Buffering"); buffering != "no" {
		t.Fatalf("X-Accel-Buffering = %q", buffering)
	}

	firstMax := stream.MaxRevision(time.Second)
	if firstMax <= 0 {
		t.Fatal("no replayed revisions received")
	}

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{
		"id":   "sse-project-2",
		"name": "Second",
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)

	live := stream.WaitRevisionAbove(firstMax, 3*time.Second)
	if live <= firstMax {
		t.Fatalf("live revision = %d, want > %d", live, firstMax)
	}
	stream.WaitHeartbeat(3 * time.Second)

	reconnected := openSSE(t, ts.URL, admin.AccessToken, strconv.FormatInt(firstMax, 10))
	defer reconnected.Close()
	replayed := reconnected.WaitRevisionAbove(firstMax, 3*time.Second)
	if replayed <= firstMax {
		t.Fatalf("replayed revision = %d, want > %d", replayed, firstMax)
	}
	reconnected.WaitHeartbeat(3 * time.Second)
}

func TestOIDCFlowWithFakeProvider(t *testing.T) {
	provider := &fakeOIDCProvider{claims: auth.OIDCClaims{
		Issuer:      "https://idp.example",
		Subject:     "subject-1",
		Login:       "oidc-user@example.test",
		DisplayName: "OIDC User",
		Role:        domain.RoleMember,
		Groups:      []string{"team"},
	}}
	ts, _, _ := newTestServer(t, func(cfg *Config) {
		cfg.OIDC = provider
		cfg.OIDCAllowSignup = true
	})

	resp, payload := completeOIDCLogin(t, ts.URL, true)
	requireStatus(t, resp, payload, http.StatusOK)
	session := decodeSession(t, payload)
	if session.User.Login != "oidc-user@example.test" || session.User.Role != domain.RoleMember {
		t.Fatalf("user = %+v", session.User)
	}

	meResp, mePayload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/auth/me", nil, session.AccessToken)
	requireStatus(t, meResp, mePayload, http.StatusOK)

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/auth/oidc/callback?code=good-code&state=unknown", nil, "")
	requireStatus(t, resp, payload, http.StatusBadRequest)
}

func TestOIDCUnconfigured(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/auth/oidc/login", nil, "")
	requireStatus(t, resp, payload, http.StatusNotImplemented)
}

type fakeOIDCProvider struct {
	mu       sync.Mutex
	nonce    string
	verifier string
	claims   auth.OIDCClaims
}

func (f *fakeOIDCProvider) AuthCodeURL(state, nonce, codeVerifier string) string {
	f.mu.Lock()
	f.nonce = nonce
	f.verifier = codeVerifier
	f.mu.Unlock()
	values := url.Values{}
	values.Set("state", state)
	values.Set("nonce", nonce)
	values.Set("code_challenge", "challenge")
	return "https://idp.example/authorize?" + values.Encode()
}

func (f *fakeOIDCProvider) Exchange(_ context.Context, code, codeVerifier, nonce string) (auth.OIDCClaims, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code != "good-code" || codeVerifier != f.verifier || nonce != f.nonce {
		return auth.OIDCClaims{}, errors.New("bad code, verifier or nonce")
	}
	return f.claims, nil
}

type sseStream struct {
	Response *http.Response
	cancel   context.CancelFunc
	frames   chan string
}

func openSSE(t *testing.T, baseURL, token, lastEventID string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("new SSE request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open SSE: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("SSE status = %d; body = %s", resp.StatusCode, payload)
	}
	stream := &sseStream{Response: resp, cancel: cancel, frames: make(chan string, 32)}
	go func() {
		defer close(stream.frames)
		scanner := bufio.NewScanner(resp.Body)
		var frame strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				if frame.Len() > 0 {
					select {
					case stream.frames <- frame.String():
					case <-ctx.Done():
						return
					}
					frame.Reset()
				}
				continue
			}
			if frame.Len() > 0 {
				frame.WriteByte('\n')
			}
			frame.WriteString(line)
		}
	}()
	return stream
}

func (s *sseStream) Close() {
	s.cancel()
	_ = s.Response.Body.Close()
}

func (s *sseStream) NextFrame(timeout time.Duration) (string, bool) {
	select {
	case frame, ok := <-s.frames:
		return frame, ok
	case <-time.After(timeout):
		return "", false
	}
}

func (s *sseStream) MaxRevision(window time.Duration) int64 {
	deadline := time.After(window)
	var max int64
	for {
		select {
		case frame, ok := <-s.frames:
			if !ok {
				return max
			}
			if id := frameRevision(frame); id > max {
				max = id
			}
		case <-deadline:
			return max
		}
	}
}

func (s *sseStream) WaitRevisionAbove(min int64, timeout time.Duration) int64 {
	deadline := time.After(timeout)
	for {
		select {
		case frame, ok := <-s.frames:
			if !ok {
				return 0
			}
			if id := frameRevision(frame); id > min {
				return id
			}
		case <-deadline:
			return 0
		}
	}
}

func (s *sseStream) WaitHeartbeat(timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case frame, ok := <-s.frames:
			if !ok {
				return false
			}
			if strings.HasPrefix(frame, ": heartbeat") {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func frameRevision(frame string) int64 {
	for _, line := range strings.Split(frame, "\n") {
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id: ")), 10, 64)
		if err != nil {
			return 0
		}
		return value
	}
	return 0
}

type failingAPITokenAuthenticator struct {
	err error
}

func (f failingAPITokenAuthenticator) AuthenticateAPIToken(context.Context, string) (domain.User, error) {
	return domain.User{}, f.err
}

func TestAuthInfrastructureFailureReturnsInternalServerError(t *testing.T) {
	ts, api, store := newTestServer(t, nil)
	signer, err := auth.NewSigner(auth.SignerConfig{
		Secret:   []byte(strings.Repeat("k", 32)),
		Issuer:   "test-issuer",
		Audience: "test-audience",
		TTL:      5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	service, err := auth.NewService(auth.ServiceConfig{
		Signer:         signer,
		Users:          authBridge{store: store},
		Sessions:       store,
		APITokens:      failingAPITokenAuthenticator{err: errors.New("database unavailable")},
		RefreshTTL:     time.Hour,
		PasswordParams: fastArgon,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	api.auth = service

	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, "brain_infra_token")
	requireStatus(t, resp, payload, http.StatusInternalServerError)
	if strings.Contains(string(payload), "brain_infra_token") {
		t.Fatalf("error response leaked the token: %s", payload)
	}
}

// failingUserStore lets a test fail one lookup while embedding the rest of the
// Store interface.
type failingUserStore struct {
	Store
	findErr error
	userErr error
}

func (f failingUserStore) FindAPITokenByHash(context.Context, string, time.Time) (APITokenRecord, error) {
	if f.findErr != nil {
		return APITokenRecord{}, f.findErr
	}
	return APITokenRecord{ID: "token_1", UserID: "user_1", Role: domain.RoleMember}, nil
}

func (f failingUserStore) GetUser(context.Context, string) (UserRecord, error) {
	return UserRecord{}, f.userErr
}

func TestAuthBridgePropagatesInfrastructureErrors(t *testing.T) {
	infra := errors.New("database unavailable")
	for name, store := range map[string]failingUserStore{
		"token lookup": {findErr: infra},
		"user lookup":  {userErr: infra},
	} {
		t.Run(name, func(t *testing.T) {
			bridge := authBridge{store: store}
			_, err := bridge.AuthenticateAPIToken(context.Background(), "brain_token")
			if err == nil || errors.Is(err, auth.ErrInvalidToken) {
				t.Fatalf("error = %v, want the infrastructure error", err)
			}
			if !errors.Is(err, infra) {
				t.Fatalf("error = %v, want the wrapped infrastructure error", err)
			}
		})
	}
}

func TestAuthBridgeUnknownTokenIsInvalid(t *testing.T) {
	bridge := authBridge{store: failingUserStore{findErr: domain.ErrNotFound}}
	if _, err := bridge.AuthenticateAPIToken(context.Background(), "brain_token"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("error = %v, want ErrInvalidToken", err)
	}
}

type failingSessionStore struct {
	*MemoryStore
	consumeErr error
	revokeErr  error
}

func (s *failingSessionStore) ConsumeRefresh(ctx context.Context, tokenHash string, now time.Time) (auth.RefreshRecord, error) {
	if s.consumeErr != nil {
		return auth.RefreshRecord{}, s.consumeErr
	}
	return s.MemoryStore.ConsumeRefresh(ctx, tokenHash, now)
}

func (s *failingSessionStore) RevokeFamily(ctx context.Context, familyID string, now time.Time) error {
	if s.revokeErr != nil {
		return s.revokeErr
	}
	return s.MemoryStore.RevokeFamily(ctx, familyID, now)
}

func newServerWithStore(t *testing.T, store Store, logger *slog.Logger) *httptest.Server {
	t.Helper()
	cfg := testConfig()
	if logger != nil {
		cfg.Logger = logger
	}
	api, err := New(cfg, store)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		ts.Close()
		api.Close()
	})
	return ts
}

func seedAdmin(t *testing.T, store Store) {
	t.Helper()
	hash, err := auth.HashPassword(adminPassword, fastArgon)
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	if _, _, err := store.CreateUser(context.Background(), domain.User{
		Login:       adminLogin,
		DisplayName: "Administrator",
		Role:        domain.RoleAdmin,
		AuthSource:  "local",
	}, hash); err != nil {
		t.Fatalf("create admin: %v", err)
	}
}

func TestRefreshInfrastructureErrorIsInternal(t *testing.T) {
	store := &failingSessionStore{MemoryStore: NewMemoryStore(), consumeErr: errors.New("db down")}
	ts := newServerWithStore(t, store, nil)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/refresh", map[string]string{
		"refreshToken": "whatever",
	}, "")
	requireStatus(t, resp, payload, http.StatusInternalServerError)
	if strings.Contains(string(payload), "db down") {
		t.Fatalf("infrastructure detail leaked to the client: %s", payload)
	}
}

func TestRefreshReuseRevocationFailureIsInternalAndLogged(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	store := &failingSessionStore{MemoryStore: NewMemoryStore(), revokeErr: errors.New("db down")}
	seedAdmin(t, store)
	ts := newServerWithStore(t, store, logger)

	first := loginAs(t, ts.URL, adminLogin, adminPassword)
	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/refresh", map[string]string{
		"refreshToken": first.RefreshToken,
	}, "")
	requireStatus(t, resp, payload, http.StatusOK)

	// Reusing the rotated token triggers family revocation, which fails.
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/refresh", map[string]string{
		"refreshToken": first.RefreshToken,
	}, "")
	requireStatus(t, resp, payload, http.StatusInternalServerError)
	if strings.Contains(string(payload), "db down") {
		t.Fatalf("infrastructure detail leaked to the client: %s", payload)
	}
	logged := logs.String()
	if !strings.Contains(logged, "request_id") {
		t.Fatalf("log has no request id: %s", logged)
	}
	if !strings.Contains(logged, "reuse") {
		t.Fatalf("log does not preserve the reuse signal: %s", logged)
	}
}
