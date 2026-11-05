package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type fakeSecrets struct {
	mu     sync.Mutex
	values map[string]string
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{values: map[string]string{}}
}

func (f *fakeSecrets) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.values[key]
	if !ok {
		return "", fmt.Errorf("secret %q: %w", key, domain.ErrNotFound)
	}
	return value, nil
}

func (f *fakeSecrets) Set(_ context.Context, key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[key] = value
	return nil
}

func (f *fakeSecrets) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values, key)
	return nil
}

func (f *fakeSecrets) Names(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.values))
	for key := range f.values {
		names = append(names, key)
	}
	return names, nil
}

func (f *fakeSecrets) lookup(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.values[key]
	return value, ok
}

// failSetSecrets fails writes to one key, simulating a partial secret-store
// failure.
type failSetSecrets struct {
	*fakeSecrets
	failKey string
}

func (f *failSetSecrets) Set(ctx context.Context, key, value string) error {
	if key == f.failKey {
		return errors.New("keychain locked")
	}
	return f.fakeSecrets.Set(ctx, key, value)
}

// failDeleteSecrets fails deletes of one key, simulating a cleanup failure.
type failDeleteSecrets struct {
	*fakeSecrets
	failKey string
}

func (f *failDeleteSecrets) Delete(ctx context.Context, key string) error {
	if key == f.failKey {
		return errors.New("keychain locked")
	}
	return f.fakeSecrets.Delete(ctx, key)
}

func bundleJSON(t *testing.T, serverURL, access, refresh string) string {
	t.Helper()
	raw, err := json.Marshal(sessionBundle{ServerURL: serverURL, AccessToken: access, RefreshToken: refresh})
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return string(raw)
}

func storeBundle(t *testing.T, store *fakeSecrets, serverURL, access, refresh string) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.values[SecretSession] = bundleJSON(t, serverURL, access, refresh)
}

func lookupBundle(t *testing.T, store *fakeSecrets) (*sessionBundle, bool) {
	t.Helper()
	raw, ok := store.lookup(SecretSession)
	if !ok {
		return nil, false
	}
	var bundle sessionBundle
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		t.Fatalf("stored bundle is not JSON: %v", err)
	}
	return &bundle, true
}

type authFixture struct {
	mu            sync.Mutex
	access        string
	refresh       string
	staticToken   string
	rotations     int
	refreshCalls  int
	logoutCalls   int
	failRefresh   bool
	refreshStatus int
	emptyRefresh  bool
	authSource    string
	logoutStatus  int
	meStatus      int
	user          domain.User
}

func newAuthFixture() *authFixture {
	return &authFixture{
		access:     "access-stale",
		refresh:    "refresh-1",
		authSource: "jwt",
		user:       domain.User{ID: "user_1", Login: "alice", Role: domain.RoleMember},
	}
}

func (f *authFixture) currentAccess() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.access
}

func (f *authFixture) handler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("POST /v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Login    string `json:"login"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "invalid"}})
			return
		}
		if body.Login != "alice" || body.Password != "secret" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "invalid_credentials"}})
			return
		}
		f.mu.Lock()
		access, refresh, user := f.access, f.refresh, f.user
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, sessionPayload{
			AccessToken: access, RefreshToken: refresh,
			ExpiresAt: time.Now().Add(time.Hour).UTC(), User: user,
		})
	})
	mux.HandleFunc("POST /v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"refreshToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "invalid_refresh"}})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.refreshCalls++
		if f.refreshStatus != 0 {
			writeJSON(w, f.refreshStatus, map[string]any{"error": map[string]string{"code": "internal"}})
			return
		}
		if f.failRefresh || body.RefreshToken != f.refresh {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "invalid_refresh"}})
			return
		}
		f.rotations++
		f.access = fmt.Sprintf("access-fresh-%d", f.rotations)
		f.refresh = fmt.Sprintf("refresh-%d", f.rotations+1)
		refresh := f.refresh
		if f.emptyRefresh {
			refresh = ""
		}
		writeJSON(w, http.StatusOK, sessionPayload{
			AccessToken: f.access, RefreshToken: refresh,
			ExpiresAt: time.Now().Add(time.Hour).UTC(), User: f.user,
		})
	})
	mux.HandleFunc("POST /v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if bearer(r) != f.access {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "unauthorized"}})
			return
		}
		if f.logoutStatus != 0 {
			writeJSON(w, f.logoutStatus, map[string]any{"error": map[string]string{"code": "internal"}})
			return
		}
		f.logoutCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/auth/me", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		access, static, user, source, status := f.access, f.staticToken, f.user, f.authSource, f.meStatus
		f.mu.Unlock()
		if status != 0 {
			writeJSON(w, status, map[string]any{"error": map[string]string{"code": "internal"}})
			return
		}
		if static != "" && bearer(r) == static {
			writeJSON(w, http.StatusOK, mePayload{User: user, AuthSource: "api_token"})
			return
		}
		if bearer(r) != access {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "unauthorized"}})
			return
		}
		if strings.HasPrefix(access, "access-fresh-") {
			source = "jwt"
		}
		writeJSON(w, http.StatusOK, mePayload{User: user, AuthSource: source})
	})
	mux.HandleFunc("GET /v1/notes", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(bearer(r), "access-fresh-") {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "expired"}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func newAuthClient(t *testing.T, server *httptest.Server, store ports.SecretStore) (*Client, *Auth) {
	t.Helper()
	client, err := New(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("New client: %v", err)
	}
	auth, err := NewAuth(client, store)
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	auth.lockPath = filepath.Join(t.TempDir(), refreshLockFileName)
	client.SetTokenProvider(auth)
	return client, auth
}

func TestAuthLoginRefreshLogout(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	client, auth := newAuthClient(t, server, store)
	store.mu.Lock()
	store.values[SecretRefreshToken] = "legacy-refresh"
	store.values[SecretAuthServerURL] = "https://legacy.example.test"
	store.mu.Unlock()

	session, err := auth.Login(context.Background(), ports.Credentials{Login: "alice", Password: "secret"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if session.User.Login != "alice" || session.User.Role != domain.RoleMember {
		t.Fatalf("session = %+v", session)
	}
	bundle, ok := lookupBundle(t, store)
	if !ok {
		t.Fatal("login did not store a session bundle")
	}
	if bundle.ServerURL != server.URL || bundle.AccessToken != "access-stale" || bundle.RefreshToken != "refresh-1" {
		t.Fatalf("stored bundle = %+v", bundle)
	}
	if _, ok := store.lookup(SecretAccessToken); ok {
		t.Fatal("login stored the legacy access key")
	}

	refreshed, err := auth.RefreshStored(context.Background())
	if err != nil {
		t.Fatalf("RefreshStored: %v", err)
	}
	if refreshed != "access-fresh-1" {
		t.Fatalf("refreshed access = %q", refreshed)
	}
	bundle, _ = lookupBundle(t, store)
	if bundle.AccessToken != "access-fresh-1" || bundle.RefreshToken != "refresh-2" {
		t.Fatalf("rotated bundle = %+v", bundle)
	}

	user, err := auth.Principal(context.Background(), "")
	if err != nil {
		t.Fatalf("Principal: %v", err)
	}
	if user.Login != "alice" {
		t.Fatalf("principal = %+v", user)
	}

	if err := auth.Logout(context.Background(), ""); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if fixture.logoutCalls != 1 {
		t.Fatalf("logout calls = %d", fixture.logoutCalls)
	}
	assertTokensCleared(t, store)
	if client == nil {
		t.Fatal("client is nil")
	}
}

func TestClientAutoRefreshOnUnauthorized(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	client, auth := newAuthClient(t, server, store)
	if _, err := auth.Login(context.Background(), ports.Credentials{Login: "alice", Password: "secret"}); err != nil {
		t.Fatalf("Login: %v", err)
	}

	var out map[string]string
	if err := client.do(context.Background(), http.MethodGet, "/v1/notes", nil, nil, &out); err != nil {
		t.Fatalf("do: %v", err)
	}
	if out["status"] != "ok" {
		t.Fatalf("response = %+v", out)
	}
	if fixture.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d", fixture.refreshCalls)
	}
	bundle, _ := lookupBundle(t, store)
	if bundle == nil || bundle.AccessToken != "access-fresh-1" {
		t.Fatalf("stored bundle = %+v", bundle)
	}
}

func TestClientRefreshCredentialFailureReturnsUnauthorized(t *testing.T) {
	fixture := newAuthFixture()
	fixture.failRefresh = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	client, auth := newAuthClient(t, server, store)
	if _, err := auth.Login(context.Background(), ports.Credentials{Login: "alice", Password: "secret"}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	err := client.do(context.Background(), http.MethodGet, "/v1/notes", nil, nil, &map[string]string{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("do error = %v, want unauthorized", err)
	}
	if strings.Contains(err.Error(), "access-stale") {
		t.Fatalf("error leaked token: %v", err)
	}
	if _, ok := lookupBundle(t, store); ok {
		t.Fatal("definitive refresh 401 left a dead bundle behind")
	}
}

func TestClientRefreshServerErrorSurfaces(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	client, auth := newAuthClient(t, server, store)
	if _, err := auth.Login(context.Background(), ports.Credentials{Login: "alice", Password: "secret"}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	fixture.mu.Lock()
	fixture.refreshStatus = http.StatusInternalServerError
	fixture.mu.Unlock()

	err := client.do(context.Background(), http.MethodGet, "/v1/notes", nil, nil, &map[string]string{})
	if err == nil {
		t.Fatal("do succeeded while the refresh endpoint failed")
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("do error = %v, want the refresh failure instead of the original 401", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("do error = %v, want the refresh 500", err)
	}
	if _, ok := lookupBundle(t, store); !ok {
		t.Fatal("transient refresh failure deleted the bundle")
	}
}

func TestClientRefreshTransportErrorSurfaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client, err := New(Config{
		BaseURL: server.URL,
		Token:   "stale-token",
		Refresh: func(context.Context) (string, error) {
			return "", errors.New("network down")
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = client.do(context.Background(), http.MethodGet, "/v1/notes", nil, nil, &map[string]string{})
	if err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("do error = %v, want the transport failure wrapped", err)
	}
	if !strings.Contains(err.Error(), "network down") {
		t.Fatalf("do error = %v, want the refresh failure", err)
	}
}

func TestClientLegacyStaticTokenHasNoRefresh(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != "static-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	store := newFakeSecrets()
	store.values[SecretAccessToken] = "static-token"
	client, auth := newAuthClient(t, server, store)

	var out map[string]string
	if err := client.do(context.Background(), http.MethodGet, "/v1/notes", nil, nil, &out); err != nil {
		t.Fatalf("do with a legacy static token: %v", err)
	}
	if err := client.do(context.Background(), http.MethodGet, "/v1/notes", nil, nil, &out); err != nil {
		t.Fatalf("repeat do: %v", err)
	}
	if _, err := auth.RefreshStored(context.Background()); !errors.Is(err, errNoRefreshToken) {
		t.Fatalf("RefreshStored error = %v, want errNoRefreshToken", err)
	}
	if token, _ := store.lookup(SecretAccessToken); token != "static-token" {
		t.Fatalf("static token = %q", token)
	}

	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()
	badClient, _ := newAuthClient(t, unauthorized, store)
	err := badClient.do(context.Background(), http.MethodGet, "/v1/notes", nil, nil, &out)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401 with a legacy static token = %v, want the original 401", err)
	}
}

func TestClientConcurrentUnauthorizedRefreshesOnce(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	client, auth := newAuthClient(t, server, store)
	if _, err := auth.Login(context.Background(), ports.Credentials{Login: "alice", Password: "secret"}); err != nil {
		t.Fatalf("Login: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out map[string]string
			errs <- client.do(context.Background(), http.MethodGet, "/v1/notes", nil, nil, &out)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent do: %v", err)
		}
	}
	if fixture.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", fixture.refreshCalls)
	}
}

func TestAuthLoginValidation(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	_, auth := newAuthClient(t, server, newFakeSecrets())

	if _, err := auth.Login(context.Background(), ports.Credentials{Password: "secret"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty login error = %v", err)
	}
	if _, err := auth.Login(context.Background(), ports.Credentials{Login: "alice"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty password error = %v", err)
	}
	if _, err := auth.Refresh(context.Background(), "  "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty refresh error = %v", err)
	}
}

func TestAuthStoresEmptyAccessTokenFails(t *testing.T) {
	fixture := newAuthFixture()
	fixture.access = ""
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	_, auth := newAuthClient(t, server, store)
	if _, err := auth.Login(context.Background(), ports.Credentials{Login: "alice", Password: "secret"}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("empty access token error = %v", err)
	}
	if _, ok := store.lookup(SecretSession); ok {
		t.Fatal("empty access token stored a bundle")
	}
}

func TestRefreshRejectsEmptyRefreshTokenAndClearsBundle(t *testing.T) {
	fixture := newAuthFixture()
	fixture.emptyRefresh = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	storeBundle(t, store, server.URL, "access-stale", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	_, err := auth.RefreshStored(context.Background())
	if err == nil || !strings.Contains(err.Error(), "empty refresh token") {
		t.Fatalf("RefreshStored error = %v, want an empty refresh rejection", err)
	}
	if _, ok := store.lookup(SecretSession); ok {
		t.Fatal("consumed bundle survived an empty refresh response")
	}
}

func TestLoginBundleWriteFailureFailsClosed(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := &failSetSecrets{fakeSecrets: newFakeSecrets(), failKey: SecretSession}
	_, auth := newAuthClient(t, server, store)

	_, err := auth.Login(context.Background(), ports.Credentials{Login: "alice", Password: "secret"})
	if err == nil || !strings.Contains(err.Error(), "brain login again") {
		t.Fatalf("Login error = %v, want a re-login instruction", err)
	}
	if _, ok := store.lookup(SecretSession); ok {
		t.Fatal("failed bundle write left a bundle behind")
	}
}

func TestRefreshBundleWriteFailureDeletesBundle(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := &failSetSecrets{fakeSecrets: newFakeSecrets(), failKey: SecretSession}
	storeBundle(t, store.fakeSecrets, server.URL, "access-stale", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	_, err := auth.RefreshStored(context.Background())
	if err == nil || !strings.Contains(err.Error(), "brain login again") {
		t.Fatalf("RefreshStored error = %v, want a fail-closed error", err)
	}
	if _, ok := store.lookup(SecretSession); ok {
		t.Fatal("failed bundle write kept the consumed refresh token")
	}
}

func TestRefreshUnauthorizedClearsBundle(t *testing.T) {
	fixture := newAuthFixture()
	fixture.failRefresh = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	storeBundle(t, store, server.URL, "access-stale", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	if _, err := auth.RefreshStored(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("RefreshStored error = %v, want unauthorized", err)
	}
	if _, ok := store.lookup(SecretSession); ok {
		t.Fatal("definitive refresh 401 kept the dead bundle")
	}
}

func assertTokensCleared(t *testing.T, store *fakeSecrets) {
	t.Helper()
	for _, key := range []string{SecretSession, SecretAccessToken, SecretRefreshToken, SecretAuthServerURL} {
		if _, ok := store.lookup(key); ok {
			t.Fatalf("%s was not cleared", key)
		}
	}
}

func TestLogoutNoTokensIsIdempotent(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	_, auth := newAuthClient(t, server, store)

	if err := auth.Logout(context.Background(), ""); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if fixture.logoutCalls != 0 {
		t.Fatalf("logout calls = %d, want 0", fixture.logoutCalls)
	}
}

func TestLogoutStaticTokenReportsNotRevoked(t *testing.T) {
	fixture := newAuthFixture()
	fixture.access = "static-api-token"
	fixture.authSource = "api_token"
	fixture.staticToken = "static-api-token"
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	store.values[SecretAccessToken] = "static-api-token"
	store.values[SecretRefreshToken] = "legacy-refresh"
	_, auth := newAuthClient(t, server, store)

	err := auth.Logout(context.Background(), "")
	if !errors.Is(err, ErrStaticTokenNotRevoked) {
		t.Fatalf("Logout error = %v, want ErrStaticTokenNotRevoked", err)
	}
	if fixture.logoutCalls != 0 {
		t.Fatalf("logout endpoint called %d times for a static token", fixture.logoutCalls)
	}
	if fixture.refreshCalls != 0 {
		t.Fatalf("legacy refresh key was used %d times", fixture.refreshCalls)
	}
	assertTokensCleared(t, store)
}

func TestLogoutMeUnauthorizedRefreshUnauthorizedClears(t *testing.T) {
	fixture := newAuthFixture()
	fixture.failRefresh = true
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	storeBundle(t, store, server.URL, "access-expired", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	if err := auth.Logout(context.Background(), ""); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if fixture.refreshCalls == 0 {
		t.Fatal("expired access token did not attempt a refresh")
	}
	assertTokensCleared(t, store)
}

func TestLogoutExpiredAccessTransientRefreshKeepsBundle(t *testing.T) {
	fixture := newAuthFixture()
	fixture.refreshStatus = http.StatusServiceUnavailable
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	storeBundle(t, store, server.URL, "access-expired", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	err := auth.Logout(context.Background(), "")
	if err == nil || errors.Is(err, ErrStaticTokenNotRevoked) {
		t.Fatalf("Logout error = %v, want a transient refresh failure", err)
	}
	bundle, ok := lookupBundle(t, store)
	if !ok || bundle.RefreshToken != "refresh-1" {
		t.Fatalf("bundle after a transient refresh failure = %+v, %v", bundle, ok)
	}
}

func TestLogoutServerErrorKeepsBundle(t *testing.T) {
	fixture := newAuthFixture()
	fixture.logoutStatus = http.StatusInternalServerError
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	storeBundle(t, store, server.URL, "access-stale", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	if err := auth.Logout(context.Background(), ""); err == nil {
		t.Fatal("Logout succeeded when the server rejected revocation")
	}
	if _, ok := lookupBundle(t, store); !ok {
		t.Fatal("bundle was cleared after a 5xx logout")
	}
}

func TestLogoutUnauthorizedRevocationStillClears(t *testing.T) {
	fixture := newAuthFixture()
	fixture.logoutStatus = http.StatusUnauthorized
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	storeBundle(t, store, server.URL, "access-stale", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	if err := auth.Logout(context.Background(), ""); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	assertTokensCleared(t, store)
}

func TestLogoutTransientMeFailureKeepsBundle(t *testing.T) {
	fixture := newAuthFixture()
	fixture.meStatus = http.StatusServiceUnavailable
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	storeBundle(t, store, server.URL, "access-stale", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	err := auth.Logout(context.Background(), "")
	if err == nil {
		t.Fatal("Logout succeeded when /me failed transiently")
	}
	if fixture.logoutCalls != 0 {
		t.Fatalf("logout endpoint called %d times", fixture.logoutCalls)
	}
	if _, ok := lookupBundle(t, store); !ok {
		t.Fatal("bundle was cleared after a transient /me failure")
	}
	if fixture.refreshCalls != 0 {
		t.Fatalf("transient /me failure rotated the refresh token %d times", fixture.refreshCalls)
	}
}

func TestLogoutMixedStaticAndRefreshRevokesFamilyAndWarns(t *testing.T) {
	fixture := newAuthFixture()
	fixture.staticToken = "static-api-token"
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	store.values[SecretAccessToken] = "static-api-token"
	storeBundle(t, store, server.URL, "access-stale", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	err := auth.Logout(context.Background(), "")
	if !errors.Is(err, ErrStaticTokenNotRevoked) {
		t.Fatalf("Logout error = %v, want ErrStaticTokenNotRevoked", err)
	}
	if fixture.refreshCalls != 1 || fixture.rotations != 1 {
		t.Fatalf("refresh calls = %d, rotations = %d, want one rotation", fixture.refreshCalls, fixture.rotations)
	}
	if fixture.logoutCalls != 1 {
		t.Fatalf("logout calls = %d, want the rotated family revoked", fixture.logoutCalls)
	}
	assertTokensCleared(t, store)
}

func TestLogoutMixedTransientRotationKeepsBundle(t *testing.T) {
	fixture := newAuthFixture()
	fixture.staticToken = "static-api-token"
	fixture.refreshStatus = http.StatusServiceUnavailable
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	store.values[SecretAccessToken] = "static-api-token"
	storeBundle(t, store, server.URL, "access-stale", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	err := auth.Logout(context.Background(), "")
	if err == nil || errors.Is(err, ErrStaticTokenNotRevoked) {
		t.Fatalf("Logout error = %v, want the transient rotation failure", err)
	}
	if fixture.logoutCalls != 0 {
		t.Fatalf("logout calls = %d, want 0", fixture.logoutCalls)
	}
	if _, ok := lookupBundle(t, store); !ok {
		t.Fatal("bundle was destroyed when the rotation failed transiently")
	}
	if got, ok := store.lookup(SecretAccessToken); !ok || got != "static-api-token" {
		t.Fatalf("static token after a transient rotation failure = %q, %v", got, ok)
	}
}

// barrierSecrets blocks the first two bundle reads until both arrive, so two
// Auth instances observe the same bundle before either takes the lock.
type barrierSecrets struct {
	*fakeSecrets
	mu    sync.Mutex
	reads int
	gate  chan struct{}
}

func newBarrierSecrets() *barrierSecrets {
	return &barrierSecrets{fakeSecrets: newFakeSecrets(), gate: make(chan struct{})}
}

func (b *barrierSecrets) Get(ctx context.Context, key string) (string, error) {
	if key == SecretSession {
		b.mu.Lock()
		b.reads++
		if b.reads == 2 {
			close(b.gate)
		}
		b.mu.Unlock()
		<-b.gate
	}
	return b.fakeSecrets.Get(ctx, key)
}

func TestRefreshStoredConcurrentInstancesRotateOnce(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newBarrierSecrets()
	storeBundle(t, store.fakeSecrets, server.URL, "access-stale", "refresh-1")

	lockPath := filepath.Join(t.TempDir(), refreshLockFileName)
	_, first := newAuthClient(t, server, store)
	_, second := newAuthClient(t, server, store)
	first.lockPath = lockPath
	second.lockPath = lockPath

	var wg sync.WaitGroup
	results := make(chan string, 2)
	errs := make(chan error, 2)
	for _, auth := range []*Auth{first, second} {
		wg.Add(1)
		go func(a *Auth) {
			defer wg.Done()
			token, err := a.RefreshStored(context.Background())
			results <- token
			errs <- err
		}(auth)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("RefreshStored: %v", err)
		}
	}
	for token := range results {
		if token == "" {
			t.Fatal("RefreshStored returned an empty access token")
		}
	}
	if fixture.rotations != 1 || fixture.refreshCalls != 1 {
		t.Fatalf("rotations = %d, refresh calls = %d, want exactly one rotation", fixture.rotations, fixture.refreshCalls)
	}
}

func TestRefreshStoredTakesOverStaleLock(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	storeBundle(t, store, server.URL, "access-stale", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	if err := os.WriteFile(auth.lockPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
	stale := time.Now().Add(-time.Minute)
	if err := os.Chtimes(auth.lockPath, stale, stale); err != nil {
		t.Fatalf("age stale lock: %v", err)
	}
	auth.lockStale = 10 * time.Millisecond
	auth.lockWait = 500 * time.Millisecond
	auth.lockPoll = 5 * time.Millisecond

	token, err := auth.RefreshStored(context.Background())
	if err != nil {
		t.Fatalf("RefreshStored: %v", err)
	}
	if token != "access-fresh-1" {
		t.Fatalf("access token = %q", token)
	}
	if _, statErr := os.Stat(auth.lockPath); !os.IsNotExist(statErr) {
		t.Fatalf("lock file still exists after release: %v", statErr)
	}
}

func TestRefreshStoredTimesOutOnFreshLock(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	storeBundle(t, store, server.URL, "access-stale", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	if err := os.WriteFile(auth.lockPath, []byte("held"), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	auth.lockStale = time.Minute
	auth.lockWait = 100 * time.Millisecond
	auth.lockPoll = 10 * time.Millisecond

	started := time.Now()
	_, err := auth.RefreshStored(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("RefreshStored error = %v, want a lock timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("lock wait took %s, want under one second", elapsed)
	}
	if fixture.rotations != 0 {
		t.Fatalf("rotations = %d, want 0", fixture.rotations)
	}
}

func TestRefreshLockWaitExceedsHTTPTimeout(t *testing.T) {
	if defaultRefreshLockWait <= DefaultTimeout {
		t.Fatalf("default refresh lock wait %s does not exceed the HTTP timeout %s", defaultRefreshLockWait, DefaultTimeout)
	}
	auth := &Auth{}
	if auth.refreshLockWait() != defaultRefreshLockWait {
		t.Fatalf("lock wait = %s, want %s", auth.refreshLockWait(), defaultRefreshLockWait)
	}
	auth.lockWait = 42 * time.Second
	if auth.refreshLockWait() != 42*time.Second {
		t.Fatalf("overridden lock wait = %s", auth.refreshLockWait())
	}
}

func TestRefreshLockReleaseDoesNotRemoveNewOwner(t *testing.T) {
	store := newFakeSecrets()
	server := httptest.NewServer(newAuthFixture().handler())
	defer server.Close()
	_, first := newAuthClient(t, server, store)
	_, second := newAuthClient(t, server, store)
	path := filepath.Join(t.TempDir(), refreshLockFileName)
	first.lockPath = path
	second.lockPath = path
	second.lockStale = time.Millisecond
	second.lockWait = time.Second
	second.lockPoll = time.Millisecond

	releaseFirst, err := first.acquireRefreshLock(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatalf("age lock: %v", err)
	}
	releaseSecond, err := second.acquireRefreshLock(context.Background())
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	releaseFirst()
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("preempted owner removed the new owner lock: %v", statErr)
	}
	releaseSecond()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("owner release left the lock behind: %v", statErr)
	}
}

func TestTokenRefusesWrongServer(t *testing.T) {
	store := newFakeSecrets()
	storeBundle(t, store, "https://server-a.example.test", "access-for-a", "refresh-for-a")

	var requests int
	var mu sync.Mutex
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer serverB.Close()
	clientB, authB := newAuthClient(t, serverB, store)

	token, err := authB.Token(context.Background())
	if !errors.Is(err, ErrWrongServer) {
		t.Fatalf("Token error = %v, want ErrWrongServer", err)
	}
	if token != "" {
		t.Fatalf("Token returned %q for the wrong server", token)
	}
	if !strings.Contains(err.Error(), "https://server-a.example.test") || !strings.Contains(err.Error(), serverB.URL) {
		t.Fatalf("error does not name both servers: %v", err)
	}
	if err := clientB.do(context.Background(), http.MethodGet, "/v1/notes", nil, nil, &map[string]string{}); !errors.Is(err, ErrWrongServer) {
		t.Fatalf("do error = %v, want ErrWrongServer", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Fatalf("server B received %d requests with A's credentials", requests)
	}
}

func TestLogoutWrongServerKeepsBundle(t *testing.T) {
	store := newFakeSecrets()
	storeBundle(t, store, "https://server-a.example.test", "access-for-a", "refresh-for-a")
	serverB := httptest.NewServer(newAuthFixture().handler())
	defer serverB.Close()
	_, authB := newAuthClient(t, serverB, store)

	err := authB.Logout(context.Background(), "")
	if !errors.Is(err, ErrWrongServer) {
		t.Fatalf("Logout error = %v, want ErrWrongServer", err)
	}
	if _, ok := lookupBundle(t, store); !ok {
		t.Fatal("logout at server B cleared server A's bundle")
	}
}

func TestTokenPrefersBundleOverLegacyStaticToken(t *testing.T) {
	server := httptest.NewServer(newAuthFixture().handler())
	defer server.Close()
	store := newFakeSecrets()
	store.values[SecretAccessToken] = "static-token"
	storeBundle(t, store, server.URL, "bundle-access", "refresh-1")
	_, auth := newAuthClient(t, server, store)

	token, err := auth.Token(context.Background())
	if err != nil || token != "bundle-access" {
		t.Fatalf("Token = %q, %v; want the bundle access token", token, err)
	}
}

func TestLegacyUnboundPairIsNotRefreshed(t *testing.T) {
	fixture := newAuthFixture()
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := newFakeSecrets()
	store.values[SecretAccessToken] = "access-stale"
	store.values[SecretRefreshToken] = "refresh-1"
	store.values[SecretAuthServerURL] = "https://old.example.test"
	_, auth := newAuthClient(t, server, store)

	token, err := auth.Token(context.Background())
	if err != nil || token != "access-stale" {
		t.Fatalf("Token = %q, %v; legacy server-token is a static fallback", token, err)
	}
	if _, err := auth.RefreshStored(context.Background()); !errors.Is(err, errNoRefreshToken) {
		t.Fatalf("RefreshStored error = %v, want refresh unavailable", err)
	}
	if fixture.refreshCalls != 0 {
		t.Fatalf("legacy refresh token was used %d times", fixture.refreshCalls)
	}
	if _, ok := store.lookup(SecretAuthServerURL); !ok {
		t.Fatal("legacy binding was rewritten")
	}
}

func TestTokenDuringLoginSeesCompleteBundle(t *testing.T) {
	fixture := newAuthFixture()
	fixture.access = "access-new"
	fixture.refresh = "refresh-new"
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := &blockingSessionSet{
		fakeSecrets: newFakeSecrets(),
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	storeBundle(t, store.fakeSecrets, server.URL, "access-old", "refresh-old")
	_, auth := newAuthClient(t, server, store)

	loginDone := make(chan error, 1)
	go func() {
		_, err := auth.Login(context.Background(), ports.Credentials{Login: "alice", Password: "secret"})
		loginDone <- err
	}()
	<-store.entered

	token, err := auth.Token(context.Background())
	if err != nil || token != "access-old" {
		t.Fatalf("Token during login = %q, %v; want the complete old bundle", token, err)
	}
	old, ok := lookupBundle(t, store.fakeSecrets)
	if !ok || old.AccessToken != "access-old" || old.RefreshToken != "refresh-old" {
		t.Fatalf("bundle during login = %+v, %v; want the complete old pair", old, ok)
	}

	close(store.release)
	if err := <-loginDone; err != nil {
		t.Fatalf("Login: %v", err)
	}
	token, err = auth.Token(context.Background())
	if err != nil || token != "access-new" {
		t.Fatalf("Token after login = %q, %v; want the new bundle", token, err)
	}
	current, ok := lookupBundle(t, store.fakeSecrets)
	if !ok || current.AccessToken != "access-new" || current.RefreshToken != "refresh-new" {
		t.Fatalf("bundle after login = %+v, %v", current, ok)
	}
}

type blockingSessionSet struct {
	*fakeSecrets
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *blockingSessionSet) Set(ctx context.Context, key, value string) error {
	if key == SecretSession {
		b.once.Do(func() { close(b.entered) })
		<-b.release
	}
	return b.fakeSecrets.Set(ctx, key, value)
}

func TestStaticTokenLogoutReportsCleanupFailure(t *testing.T) {
	fixture := newAuthFixture()
	fixture.access = "static-api-token"
	fixture.authSource = "api_token"
	fixture.staticToken = "static-api-token"
	server := httptest.NewServer(fixture.handler())
	defer server.Close()
	store := &failDeleteSecrets{fakeSecrets: newFakeSecrets(), failKey: SecretAccessToken}
	store.values[SecretAccessToken] = "static-api-token"
	_, auth := newAuthClient(t, server, store)

	err := auth.Logout(context.Background(), "")
	if !errors.Is(err, ErrStaticTokenNotRevoked) {
		t.Fatalf("Logout error = %v, want ErrStaticTokenNotRevoked", err)
	}
	if err == ErrStaticTokenNotRevoked {
		t.Fatal("cleanup failure was discarded")
	}
	if !strings.Contains(err.Error(), "clear server-token") {
		t.Fatalf("error does not surface the cleanup failure: %v", err)
	}
}

func TestCorruptBundleDoesNotFallBackToStaticToken(t *testing.T) {
	server := httptest.NewServer(newAuthFixture().handler())
	defer server.Close()
	store := newFakeSecrets()
	store.values[SecretSession] = "{not-json"
	store.values[SecretAccessToken] = "static-token"
	_, auth := newAuthClient(t, server, store)

	if _, err := auth.Token(context.Background()); !errors.Is(err, errInvalidBundle) {
		t.Fatalf("Token error = %v, want errInvalidBundle", err)
	}
}
