package sourceoauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthorizePKCEFullFlow(t *testing.T) {
	var challengeSeen atomic.Value
	var redirectSeen atomic.Value
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept header = %q", r.Header.Get("Accept"))
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != "desktop-client" {
			t.Errorf("unexpected token form: %v", r.Form)
		}
		if r.Form.Get("code") != "one-time-code" || !strings.HasPrefix(r.Form.Get("redirect_uri"), "http://127.0.0.1:") {
			t.Errorf("unexpected code/redirect: %v", r.Form)
		}
		if r.Form.Get("redirect_uri") != redirectSeen.Load().(string) {
			t.Errorf("token exchange redirect URI did not match authorization request")
		}
		verifierHash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(verifierHash[:]) != challengeSeen.Load().(string) {
			t.Errorf("code verifier did not match authorization challenge")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"access-value","refresh_token":"refresh-value","token_type":"Bearer","scope":"read_api","expires_in":3600}`)
	}))
	defer tokenServer.Close()
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("code_challenge_method") != "S256" || len(query.Get("state")) != 64 || len(query.Get("code_challenge")) != 43 {
			t.Errorf("invalid authorize params: %v", query)
		}
		if query.Get("response_type") != "code" || query.Get("scope") != "read_api" {
			t.Errorf("missing authorization parameters: %v", query)
		}
		if verifierFromChallenge(query.Get("code_challenge")) == "" {
			t.Errorf("challenge is not a valid S256 challenge")
		}
		challengeSeen.Store(query.Get("code_challenge"))
		redirectSeen.Store(query.Get("redirect_uri"))
		redirect, _ := url.Parse(query.Get("redirect_uri"))
		params := url.Values{"state": {query.Get("state")}, "code": {"one-time-code"}}
		http.Redirect(w, r, redirect.String()+"?"+params.Encode(), http.StatusFound)
	}))
	defer authServer.Close()
	provider := Provider{Name: "test", AuthorizeURL: authServer.URL + "/authorize", TokenURL: tokenServer.URL + "/token", PKCE: true, TokenAuthStyle: TokenAuthForm}
	flow := Flow{Provider: provider, ClientID: "desktop-client", Scopes: []string{"read_api"}, Port: 0}
	callbackAddr := ""
	bundle, err := flow.Authorize(context.Background(), func(raw string) error {
		parsed, err := url.Parse(raw)
		if err != nil {
			return err
		}
		redirect, err := url.Parse(parsed.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		callbackAddr = redirect.Host
		response, err := http.Get(raw)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("callback status = %d", response.StatusCode)
		}
		if strings.Contains(string(body), "access-value") || strings.Contains(string(body), "refresh-value") {
			t.Error("callback response exposed tokens")
		}
		if strings.HasSuffix(redirect.Host, ":0") {
			t.Error("listener did not select an ephemeral port")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.AccessToken != "access-value" || bundle.RefreshToken != "refresh-value" || bundle.TokenType != "Bearer" || len(bundle.Scopes) != 1 || bundle.Expiry.IsZero() {
		t.Fatalf("unexpected bundle: %+v", bundle)
	}
	// The callback listener has been closed before Authorize returns.
	if conn, err := net.DialTimeout("tcp", callbackAddr, 10*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("loopback listener is still open")
	}
}

// A challenge cannot reveal the verifier; this helper only checks that the
// challenge has the expected unpadded base64url shape.
func verifierFromChallenge(value string) string {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return ""
	}
	return "valid"
}

func TestAuthorizeRejectsBadStateAndSanitizes(t *testing.T) {
	secretCode := "top-secret-code"
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redirect, _ := url.Parse(q.Get("redirect_uri"))
		params := url.Values{"state": {"wrong-state"}, "code": {secretCode}}
		http.Redirect(w, r, redirect.String()+"?"+params.Encode(), http.StatusFound)
	}))
	defer auth.Close()
	provider := Provider{Name: "test", AuthorizeURL: auth.URL + "/authorize", TokenURL: auth.URL + "/token", PKCE: true}
	flow := Flow{Provider: provider, ClientID: "client", Port: 0}
	_, err := flow.Authorize(context.Background(), func(raw string) error {
		resp, err := http.Get(raw)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	})
	if err == nil || strings.Contains(err.Error(), secretCode) || strings.Contains(err.Error(), auth.URL) {
		t.Fatalf("state error was missing or leaked data: %v", err)
	}
}

func TestAuthorizeSanitizesProviderErrors(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redirect, _ := url.Parse(q.Get("redirect_uri"))
		params := url.Values{"state": {q.Get("state")}, "code": {"private-code"}}
		http.Redirect(w, r, redirect.String()+"?"+params.Encode(), http.StatusFound)
	}))
	defer auth.Close()
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"private-token-url-and-secret"}`)
	}))
	defer token.Close()
	flow := Flow{Provider: Provider{Name: "test", AuthorizeURL: auth.URL, TokenURL: token.URL, PKCE: true}, ClientID: "client", Port: 0}
	_, err := flow.Authorize(context.Background(), func(raw string) error {
		resp, err := http.Get(raw)
		if err == nil {
			resp.Body.Close()
		}
		return err
	})
	if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), token.URL) {
		t.Fatalf("provider error leaked response data: %v", err)
	}
}

func TestDevicePendingSlowDownSuccess(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/device") {
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("client_id") != "client" {
				t.Errorf("missing client id: %v", r.Form)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"device_code":"hidden-device-code","user_code":"ABCD-EFGH","verification_uri":"https://example.test/device","verification_uri_complete":"https://example.test/device?user_code=ABCD-EFGH","expires_in":120,"interval":1}`)
			return
		}
		count := polls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch count {
		case 1:
			io.WriteString(w, `{"error":"authorization_pending"}`)
		case 2:
			io.WriteString(w, `{"error":"slow_down"}`)
		default:
			io.WriteString(w, `{"access_token":"device-access","expires_in":3600}`)
		}
	}))
	defer server.Close()
	var nowMu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var sleeps []time.Duration
	flow := DeviceFlow{
		Provider: Provider{Name: "github-test", DeviceURL: server.URL + "/device", TokenURL: server.URL + "/token", Device: true},
		ClientID: "client", Now: func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now },
		Sleep: func(_ context.Context, delay time.Duration) error {
			sleeps = append(sleeps, delay)
			nowMu.Lock()
			now = now.Add(delay)
			nowMu.Unlock()
			return nil
		},
	}
	promptCount := 0
	bundle, err := flow.Device(context.Background(), func(p DevicePrompt) error {
		promptCount++
		if p.UserCode != "ABCD-EFGH" || p.ExpiresIn != 2*time.Minute || p.VerificationURIComplete == "" {
			t.Errorf("unexpected prompt: %+v", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if promptCount != 1 || polls.Load() != 3 || bundle.AccessToken != "device-access" {
		t.Fatalf("prompt=%d polls=%d bundle=%+v", promptCount, polls.Load(), bundle)
	}
	if len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != 6*time.Second {
		t.Fatalf("unexpected polling waits: %v", sleeps)
	}
}

func TestDeviceDeniedAndExpired(t *testing.T) {
	for _, test := range []struct {
		name, errorCode string
		want            string
	}{
		{name: "denied", errorCode: "access_denied", want: "denied"},
		{name: "expired", errorCode: "expired_token", want: "expired"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/device" {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"device_code":"hidden","user_code":"CODE","verification_uri":"https://example.test","expires_in":60,"interval":1}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"error":"`+test.errorCode+`"}`)
			}))
			defer server.Close()
			flow := DeviceFlow{Provider: Provider{Name: "test", DeviceURL: server.URL + "/device", TokenURL: server.URL + "/token", Device: true}, ClientID: "client"}
			_, err := flow.Device(context.Background(), func(DevicePrompt) error { return nil })
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "hidden") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDeviceLifetimeExpiresWithoutBusyPolling(t *testing.T) {
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/device" {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"device_code":"hidden","user_code":"CODE","verification_uri":"https://example.test","expires_in":1,"interval":1}`)
			return
		}
		polls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"error":"authorization_pending"}`)
	}))
	defer server.Close()
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	flow := DeviceFlow{
		Provider: Provider{Name: "test", DeviceURL: server.URL + "/device", TokenURL: server.URL + "/token", Device: true},
		ClientID: "client", Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		Sleep: func(_ context.Context, d time.Duration) error { mu.Lock(); now = now.Add(d); mu.Unlock(); return nil },
	}
	_, err := flow.Device(context.Background(), func(DevicePrompt) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "expired") || polls.Load() != 1 {
		t.Fatalf("expiry=%v polls=%d", err, polls.Load())
	}
}

type memorySecrets struct {
	mu       sync.Mutex
	values   map[string]string
	setCount int
	setHook  func(string, string)
}

func (m *memorySecrets) Get(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.values[key]
	if !ok {
		return "", errors.New("not found")
	}
	return value, nil
}
func (m *memorySecrets) Set(_ context.Context, key, value string) error {
	m.mu.Lock()
	m.values[key] = value
	m.setCount++
	hook := m.setHook
	m.mu.Unlock()
	if hook != nil {
		hook(key, value)
	}
	return nil
}
func (m *memorySecrets) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, key)
	return nil
}
func (m *memorySecrets) Names(context.Context) ([]string, error) { return nil, nil }

func newMemoryStore(t *testing.T, bundle Bundle) (*memorySecrets, TokenStore) {
	t.Helper()
	secrets := &memorySecrets{values: make(map[string]string)}
	store := TokenStore{Secrets: secrets, Key: "conn/work-oauth/oauth"}
	if err := store.Save(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	secrets.mu.Lock()
	secrets.setCount = 0
	secrets.mu.Unlock()
	return secrets, store
}

func TestTokenStoreRoundTripAndMalformed(t *testing.T) {
	expiry := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	secrets, store := newMemoryStore(t, Bundle{AccessToken: "a", RefreshToken: "r", TokenType: "Bearer", Scopes: []string{"read"}, Expiry: expiry})
	loaded, err := store.Load(context.Background())
	if err != nil || loaded.AccessToken != "a" || loaded.RefreshToken != "r" || !loaded.Expiry.Equal(expiry) {
		t.Fatalf("roundtrip: %+v, %v", loaded, err)
	}
	secrets.mu.Lock()
	if _, ok := secrets.values["conn/work-oauth/oauth"]; !ok {
		t.Fatal("bundle was not saved under caller key")
	}
	secrets.values[store.Key] = `{"access_token":""}`
	secrets.mu.Unlock()
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("malformed bundle accepted")
	}
	if err := store.Save(context.Background(), Bundle{}); err == nil {
		t.Fatal("invalid bundle was saved")
	}
}

func TestRefreshRotatesPersistsBeforeReturnAndKeepsOmittedRefresh(t *testing.T) {
	secrets, store := newMemoryStore(t, Bundle{AccessToken: "expired", RefreshToken: "old-refresh", Expiry: time.Now().Add(-time.Hour).UTC()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
			t.Errorf("unexpected refresh request: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"fresh","expires_in":3600}`)
	}))
	defer server.Close()
	secrets.setHook = func(key, raw string) {
		var got Bundle
		if key != store.Key || json.Unmarshal([]byte(raw), &got) != nil || got.RefreshToken != "old-refresh" || got.AccessToken != "fresh" {
			t.Errorf("rotated bundle not persisted before return: %s %s", key, raw)
		}
	}
	refresher := Refresher{Provider: Provider{Name: "test", TokenURL: server.URL}, ClientID: "client", Store: store}
	access, err := refresher.Token(context.Background())
	if err != nil || access != "fresh" {
		t.Fatalf("Token() = %q, %v", access, err)
	}
	secrets.mu.Lock()
	sets := secrets.setCount
	secrets.mu.Unlock()
	if sets != 1 {
		t.Fatalf("store Set count = %d", sets)
	}
}

func TestRefreshRotationAndInvalidGrant(t *testing.T) {
	secrets, store := newMemoryStore(t, Bundle{AccessToken: "expired", RefreshToken: "old", Expiry: time.Now().Add(-time.Hour).UTC()})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`)
	}))
	defer server.Close()
	refresher := Refresher{Provider: Provider{Name: "test", TokenURL: server.URL}, ClientID: "client", Store: store}
	if _, err := refresher.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Load(context.Background())
	if err != nil || stored.RefreshToken != "rotated" {
		t.Fatalf("rotated refresh token not saved: %+v, %v", stored, err)
	}
	_ = secrets
	invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"secret"}`)
	}))
	defer invalid.Close()
	secrets2, store2 := newMemoryStore(t, Bundle{AccessToken: "expired", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour).UTC()})
	_ = secrets2
	r2 := Refresher{Provider: Provider{Name: "test", TokenURL: invalid.URL}, ClientID: "client", Store: store2}
	if _, err := r2.Token(context.Background()); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("invalid_grant error = %v", err)
	}
}

func TestRefreshConcurrentSingleRequest(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"fresh","refresh_token":"rotated","expires_in":3600}`)
	}))
	defer server.Close()
	_, store := newMemoryStore(t, Bundle{AccessToken: "expired", RefreshToken: "refresh", Expiry: time.Now().Add(-time.Hour).UTC()})
	refresher := &Refresher{Provider: Provider{Name: "test", TokenURL: server.URL}, ClientID: "client", Store: store}
	const callers = 12
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			access, err := refresher.Token(context.Background())
			if err == nil && access != "fresh" {
				err = errors.New("unexpected access token")
			}
			results <- err
		}()
	}
	<-started
	close(release)
	for i := 0; i < callers; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh requests = %d", calls.Load())
	}
}

func TestRefreshWithoutRefreshTokenRequiresReauth(t *testing.T) {
	_, store := newMemoryStore(t, Bundle{AccessToken: "expired", Expiry: time.Now().Add(-time.Hour).UTC()})
	r := Refresher{Provider: Provider{Name: "test", TokenURL: "https://example.test/token"}, ClientID: "client", Store: store}
	if _, err := r.Token(context.Background()); !errors.Is(err, ErrReauthRequired) {
		t.Fatalf("missing refresh token error = %v", err)
	}
}

func TestProviderURLValidation(t *testing.T) {
	valid := Provider{Name: "custom", AuthorizeURL: "https://auth.example/authorize", TokenURL: "https://auth.example/token", PKCE: true}
	if err := ValidateProvider(valid); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{
		"http://example.test/token", "https://user:pass@example.test/token", "https://example.test/token?secret=x", "https://example.test/token#fragment",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if err := ValidateProvider(Provider{Name: "test", TokenURL: endpoint}); err == nil {
				t.Fatalf("accepted endpoint %q", endpoint)
			}
		})
	}
	for _, provider := range Providers() {
		if err := ValidateProvider(provider); err != nil {
			t.Errorf("default %s invalid: %v", provider.Name, err)
		}
	}
}

func TestFixedLoopbackPortBusy(t *testing.T) {
	probe, err := net.Listen("tcp4", "127.0.0.1:47615")
	if err != nil {
		t.Skip("default OAuth port is already occupied")
	}
	probe.Close()
	blocker, err := net.Listen("tcp4", "127.0.0.1:47615")
	if err != nil {
		t.Skip("default OAuth port became occupied")
	}
	defer blocker.Close()
	flow := Flow{Provider: Provider{Name: "test", AuthorizeURL: "https://example.test/authorize", TokenURL: "https://example.test/token", PKCE: true}, ClientID: "client", Port: DefaultLoopbackPort}
	opened := false
	_, err = flow.Authorize(context.Background(), func(string) error { opened = true; return nil })
	if err == nil || !strings.Contains(err.Error(), "port may be busy") || opened {
		t.Fatalf("busy-port result: opened=%v err=%v", opened, err)
	}
	if NewFlow(flow.Provider, flow.ClientID).Port != DefaultLoopbackPort {
		t.Fatal("NewFlow did not select the default callback port")
	}
}

func TestBundleValidationAndExpiryFormat(t *testing.T) {
	for _, bundle := range []Bundle{
		{}, {AccessToken: strings.Repeat("x", 16<<10+1)},
		{AccessToken: "a", Expiry: time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("offset", 3600))},
	} {
		if err := bundle.Validate(); err == nil {
			t.Errorf("accepted invalid bundle: %+v", bundle)
		}
	}
	if _, err := json.Marshal(Bundle{AccessToken: "a", Expiry: time.Now().In(time.FixedZone("utc", 0))}); err == nil {
		t.Fatal("non-UTC location accepted")
	}
	if err := json.Unmarshal([]byte(`{"access_token":"a","expiry":"2026-01-01T00:00:00+00:00"}`), new(Bundle)); err == nil {
		t.Fatal("non-Z UTC timestamp accepted")
	}
}
