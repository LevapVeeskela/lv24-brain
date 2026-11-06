package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// completeOIDCLogin starts an OIDC login and follows the callback with the
// state from the redirect. withCookie controls whether the callback carries the
// browser-binding cookie set by the login step.
func completeOIDCLogin(t *testing.T, baseURL string, withCookie bool) (*http.Response, []byte) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(baseURL + "/v1/auth/oidc/login")
	if err != nil {
		t.Fatalf("oidc login: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", resp.StatusCode)
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("redirect has no state")
	}
	if !withCookie {
		client.Jar = nil
	}
	resp, err = client.Get(baseURL + "/v1/auth/oidc/callback?code=good-code&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatalf("oidc callback: %v", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read callback: %v", err)
	}
	return resp, payload
}

func TestOIDCCallbackRequiresBrowserBoundState(t *testing.T) {
	provider := &fakeOIDCProvider{claims: auth.OIDCClaims{
		Issuer: "https://idp.example", Subject: "subject-csrf", Login: "csrf@example.test", Role: domain.RoleViewer,
	}}
	ts, _, _ := newTestServer(t, func(cfg *Config) {
		cfg.OIDC = provider
		cfg.OIDCAllowSignup = true
	})
	resp, payload := completeOIDCLogin(t, ts.URL, false)
	requireStatus(t, resp, payload, http.StatusBadRequest)

	// The state was not consumed by the rejected callback, but a fresh login
	// from a browser holding the cookie still works.
	resp, payload = completeOIDCLogin(t, ts.URL, true)
	requireStatus(t, resp, payload, http.StatusOK)
}

func TestOIDCLinkingExistingAccounts(t *testing.T) {
	cases := []struct {
		name     string
		existing domain.User
		claims   auth.OIDCClaims
		want     int
	}{
		{
			name:     "verified email links member",
			existing: domain.User{Login: "member@example.test", Role: domain.RoleMember},
			claims:   auth.OIDCClaims{Login: "member@example.test", Email: "member@example.test", EmailVerified: true},
			want:     http.StatusOK,
		},
		{
			name:     "unverified email is refused",
			existing: domain.User{Login: "member@example.test", Role: domain.RoleMember},
			claims:   auth.OIDCClaims{Login: "member@example.test", Email: "member@example.test"},
			want:     http.StatusForbidden,
		},
		{
			name:     "preferred username collision is refused",
			existing: domain.User{Login: "alice", Role: domain.RoleMember},
			claims:   auth.OIDCClaims{Login: "alice"},
			want:     http.StatusForbidden,
		},
		{
			name:     "admin is never linked implicitly",
			existing: domain.User{Login: "boss@example.test", Role: domain.RoleAdmin},
			claims:   auth.OIDCClaims{Login: "boss@example.test", Email: "boss@example.test", EmailVerified: true},
			want:     http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := tc.claims
			claims.Issuer = "https://idp.example"
			claims.Subject = "subject-" + tc.name
			claims.Role = domain.RoleMember
			provider := &fakeOIDCProvider{claims: claims}
			ts, _, store := newTestServer(t, func(cfg *Config) {
				cfg.OIDC = provider
				cfg.OIDCAllowSignup = true
			})
			existing := tc.existing
			existing.AuthSource = "local"
			created, _, err := store.CreateUser(context.Background(), existing, "")
			if err != nil {
				t.Fatalf("create user: %v", err)
			}
			resp, payload := completeOIDCLogin(t, ts.URL, true)
			requireStatus(t, resp, payload, tc.want)

			record, err := store.GetUserByOIDC(context.Background(), claims.Issuer, claims.Subject)
			linked := err == nil
			if linked != (tc.want == http.StatusOK) {
				t.Fatalf("linked = %v (err %v), want %v", linked, err, tc.want == http.StatusOK)
			}
			if linked && record.ID != created.ID {
				t.Fatalf("linked user = %s, want %s", record.ID, created.ID)
			}
		})
	}
}
