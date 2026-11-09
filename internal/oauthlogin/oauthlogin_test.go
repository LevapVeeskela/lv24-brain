package oauthlogin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/sourceoauth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const testConnectionID = "github-main"

type fakeSecrets struct {
	mu       sync.Mutex
	values   map[string]string
	order    []string
	setError map[string]error
	delError map[string]error
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{values: make(map[string]string), setError: make(map[string]error), delError: make(map[string]error)}
}

func (s *fakeSecrets) Get(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[key]
	if !ok {
		return "", domain.ErrNotFound
	}
	return value, nil
}

func (s *fakeSecrets) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "set:"+key)
	if err := s.setError[key]; err != nil {
		return err
	}
	s.values[key] = value
	return nil
}

func (s *fakeSecrets) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "delete:"+key)
	if err := s.delError[key]; err != nil {
		return err
	}
	delete(s.values, key)
	return nil
}

func (s *fakeSecrets) Names(context.Context) ([]string, error) { return nil, nil }

func (s *fakeSecrets) put(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
}

func (s *fakeSecrets) value(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[key]
}

type fakeCredentials struct {
	mu    sync.Mutex
	value string
	calls []string
	err   error
}

func (c *fakeCredentials) Get(_ context.Context, connectionID, field string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, connectionID+"/"+field)
	return c.value, c.err
}

func testSession(expiry time.Time) Session {
	return Session{
		Provider: "github", ClientID: "public-client", Scopes: []string{"repo", "read:user"},
		Bundle: sourceoauth.Bundle{AccessToken: "old-access", RefreshToken: "old-refresh", TokenType: "bearer", Scopes: []string{"repo"}, Expiry: expiry.UTC()},
	}
}

func providerFor(server *httptest.Server) sourceoauth.Provider {
	return sourceoauth.Provider{Name: "github", TokenURL: server.URL, TokenAuthStyle: sourceoauth.TokenAuthForm}
}

func newTokenServer(t *testing.T, status int, response string, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "old-refresh" {
			t.Errorf("refresh_token = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server
}

func putSession(t *testing.T, secrets *fakeSecrets, id string, session Session) {
	t.Helper()
	store := SessionStore{Secrets: secrets}
	if err := store.Save(context.Background(), id, session); err != nil {
		t.Fatalf("save test session: %v", err)
	}
	secrets.mu.Lock()
	secrets.order = nil
	secrets.mu.Unlock()
}

func TestCredentialsFallbackAndOAuthToken(t *testing.T) {
	ctx := context.Background()
	secrets := newFakeSecrets()
	base := &fakeCredentials{value: "pat-value"}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	credentials := &Credentials{Base: base, Sessions: &SessionStore{Secrets: secrets}, Now: func() time.Time { return now }}

	token, err := credentials.Get(ctx, testConnectionID, "token")
	if err != nil || token != "pat-value" {
		t.Fatalf("missing-session Get = (%q, %v), want PAT", token, err)
	}
	if len(base.calls) != 1 || base.calls[0] != testConnectionID+"/token" {
		t.Fatalf("base calls = %v", base.calls)
	}

	var requests atomic.Int32
	server := newTokenServer(t, http.StatusOK, `{"access_token":"fresh-access","refresh_token":"new-refresh","expires_in":3600}`, &requests)
	putSession(t, secrets, testConnectionID, testSession(now.Add(time.Hour)))
	credentials.Providers = map[string]sourceoauth.Provider{"github": providerFor(server)}
	token, err = credentials.Get(ctx, testConnectionID, "token")
	if err != nil || token != "old-access" {
		t.Fatalf("session Get = (%q, %v), want stored access token", token, err)
	}
	if requests.Load() != 0 {
		t.Fatalf("refresh requests = %d, want 0", requests.Load())
	}
	if len(base.calls) != 1 {
		t.Fatalf("base calls after OAuth lookup = %v", base.calls)
	}
	if got, err := credentials.Get(ctx, testConnectionID, "password"); err != nil || got != "pat-value" {
		t.Fatalf("non-token Get = (%q, %v), want delegated credential", got, err)
	}
}

func TestExpiredBundleRefreshPersistsRotationAndLeavesMetadata(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int32
	server := newTokenServer(t, http.StatusOK, `{"access_token":"fresh-access","refresh_token":"new-refresh","token_type":"bearer","expires_in":3600}`, &requests)
	secrets := newFakeSecrets()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	putSession(t, secrets, testConnectionID, testSession(now.Add(-time.Hour)))
	metadataBefore := secrets.value("conn/" + testConnectionID + "/oauthmeta")
	credentials := &Credentials{
		Sessions: &SessionStore{Secrets: secrets}, Now: func() time.Time { return now },
		Providers: map[string]sourceoauth.Provider{"github": providerFor(server)},
	}
	token, err := credentials.Get(ctx, testConnectionID, "token")
	if err != nil || token != "fresh-access" {
		t.Fatalf("Get = (%q, %v), want refreshed token", token, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", requests.Load())
	}
	var bundle sourceoauth.Bundle
	if err := json.Unmarshal([]byte(secrets.value("conn/"+testConnectionID+"/oauth")), &bundle); err != nil {
		t.Fatalf("decode persisted bundle: %v", err)
	}
	if bundle.AccessToken != token || bundle.RefreshToken != "new-refresh" || !bundle.Expiry.After(now) {
		t.Fatalf("persisted bundle = %#v", bundle)
	}
	if got := secrets.value("conn/" + testConnectionID + "/oauthmeta"); got != metadataBefore {
		t.Fatalf("metadata changed: before %q, after %q", metadataBefore, got)
	}
}

func TestConcurrentGetSharesOneRefresh(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.Form.Get("grant_type"); got != "refresh_token" {
			t.Errorf("grant_type = %q", got)
		}
		if got := r.Form.Get("refresh_token"); got != "old-refresh" {
			t.Errorf("refresh_token = %q", got)
		}
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"shared-access","expires_in":3600}`))
	}))
	t.Cleanup(server.Close)
	secrets := newFakeSecrets()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	putSession(t, secrets, testConnectionID, testSession(now.Add(-time.Hour)))
	credentials := &Credentials{Sessions: &SessionStore{Secrets: secrets}, Now: func() time.Time { return now }, Providers: map[string]sourceoauth.Provider{"github": providerFor(server)}}
	const count = 12
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := credentials.Get(ctx, testConnectionID, "token")
			if err == nil && token != "shared-access" {
				err = fmt.Errorf("unexpected token %q", token)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("refresh requests = %d, want exactly 1", requests.Load())
	}
}

func TestInvalidGrantIsReauthRequiredWithoutToken(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int32
	server := newTokenServer(t, http.StatusBadRequest, `{"error":"invalid_grant","access_token":"must-not-leak"}`, &requests)
	secrets := newFakeSecrets()
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	putSession(t, secrets, testConnectionID, testSession(now.Add(-time.Hour)))
	credentials := &Credentials{Sessions: &SessionStore{Secrets: secrets}, Now: func() time.Time { return now }, Providers: map[string]sourceoauth.Provider{"github": providerFor(server)}}
	token, err := credentials.Get(ctx, testConnectionID, "token")
	if token != "" || !errors.Is(err, ErrReauthRequired) || strings.Contains(err.Error(), "must-not-leak") {
		t.Fatalf("Get = (%q, %v), want sanitized reauth error and no token", token, err)
	}
}

func TestSessionStorePartialMalformedAndOversizedValues(t *testing.T) {
	ctx := context.Background()
	secrets := newFakeSecrets()
	base := &fakeCredentials{value: "safe-pat"}
	credentials := &Credentials{Base: base, Sessions: &SessionStore{Secrets: secrets}}

	// Bundle without metadata is a partial save and is treated as no session.
	secrets.put("conn/"+testConnectionID+"/oauth", `{"access_token":"partial-secret"}`)
	if got, err := credentials.Get(ctx, testConnectionID, "token"); err != nil || got != "safe-pat" {
		t.Fatalf("partial session fallback = (%q, %v)", got, err)
	}

	secrets.put("conn/"+testConnectionID+"/oauthmeta", `{"provider":"github","clientId":"public-client","scopes":[],"token":"metadata-secret"}`)
	if got, err := credentials.Get(ctx, testConnectionID, "token"); err == nil || got != "" || strings.Contains(err.Error(), "metadata-secret") {
		t.Fatalf("malformed metadata = (%q, %v), want sanitized error", got, err)
	}

	secrets.put("conn/"+testConnectionID+"/oauthmeta", strings.Repeat("x", MaxMetadataBytes+1))
	if got, err := credentials.Get(ctx, testConnectionID, "token"); err == nil || got != "" {
		t.Fatalf("oversized metadata = (%q, %v), want error", got, err)
	}

	secrets.put("conn/"+testConnectionID+"/oauthmeta", `{"provider":"github","clientId":"public-client","scopes":[]}`)
	secrets.put("conn/"+testConnectionID+"/oauth", strings.Repeat("x", MaxBundleBytes+1))
	if got, err := credentials.Get(ctx, testConnectionID, "token"); err == nil || got != "" {
		t.Fatalf("oversized bundle = (%q, %v), want error", got, err)
	}

	if got, err := (SessionStore{Secrets: secrets}).Load(ctx, "absent"); got.Bundle.AccessToken != "" || !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing Load = (%#v, %v), want typed not-found", got, err)
	}
}

func TestSessionStoreSaveOrderDeleteAndValidation(t *testing.T) {
	ctx := context.Background()
	secrets := newFakeSecrets()
	store := SessionStore{Secrets: secrets}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	session := testSession(now.Add(time.Hour))
	if err := store.Save(ctx, testConnectionID, session); err != nil {
		t.Fatalf("Save: %v", err)
	}
	wantOrder := []string{"set:conn/" + testConnectionID + "/oauth", "set:conn/" + testConnectionID + "/oauthmeta"}
	if fmt.Sprint(secrets.order) != fmt.Sprint(wantOrder) {
		t.Fatalf("save order = %v, want %v", secrets.order, wantOrder)
	}
	if strings.Contains(secrets.value("conn/"+testConnectionID+"/oauthmeta"), "old-access") || strings.Contains(secrets.value("conn/"+testConnectionID+"/oauthmeta"), "old-refresh") {
		t.Fatal("metadata contains token values")
	}
	if err := store.Delete(ctx, testConnectionID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load(ctx, testConnectionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Load after Delete error = %v", err)
	}
	if err := store.Save(ctx, "../bad", session); err == nil {
		t.Fatal("Save accepted invalid connection ID")
	}
	if _, err := BundleKey("valid-id"); err != nil {
		t.Fatalf("BundleKey rejected valid ID: %v", err)
	}
	if _, err := domain.CredentialKey(testConnectionID, "bad-field"); err == nil {
		t.Fatal("CredentialKey accepted invalid field")
	}
	credentials := &Credentials{Base: &fakeCredentials{value: "base"}}
	if _, err := credentials.Get(ctx, "bad/id", "token"); err == nil {
		t.Fatal("Credentials.Get accepted invalid connection ID")
	}
	if _, err := credentials.Get(ctx, testConnectionID, "bad-field"); err == nil {
		t.Fatal("Credentials.Get accepted invalid field")
	}
}

func TestSaveFailureLeavesMissingMetadataAsNoSession(t *testing.T) {
	ctx := context.Background()
	secrets := newFakeSecrets()
	metaKey := "conn/" + testConnectionID + "/oauthmeta"
	secrets.setError[metaKey] = errors.New("secret-bearing backend failure")
	store := SessionStore{Secrets: secrets}
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, testConnectionID, testSession(now.Add(time.Hour))); err == nil || strings.Contains(err.Error(), "secret-bearing") {
		t.Fatalf("Save error = %v, want sanitized error", err)
	}
	if _, err := store.Load(ctx, testConnectionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("partial Save Load error = %v, want not-found", err)
	}
}
