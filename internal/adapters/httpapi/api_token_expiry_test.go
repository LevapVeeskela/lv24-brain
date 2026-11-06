package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// manualClock is a test clock that moves only when Advance is called. It is
// injected through Config.Now, which the auth bridge uses for API-token checks.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newExpiryTestServer(t *testing.T) (*httptest.Server, *manualClock) {
	t.Helper()
	clock := &manualClock{now: time.Now().UTC().Truncate(time.Second)}
	ts, _, _ := newTestServer(t, func(cfg *Config) { cfg.Now = clock.Now })
	return ts, clock
}

func TestAPITokenExpiresInDaysStopsAuthenticating(t *testing.T) {
	ts, clock := newExpiryTestServer(t)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/tokens", map[string]any{
		"name": "ci", "role": "viewer", "expiresInDays": 1,
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)
	var created createTokenResponse
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if created.APIToken.ExpiresAt == nil {
		t.Fatalf("expiresAt missing from response: %s", payload)
	}
	if want := clock.Now().AddDate(0, 0, 1); !created.APIToken.ExpiresAt.Equal(want) {
		t.Fatalf("expiresAt = %s, want %s", created.APIToken.ExpiresAt, want)
	}

	// Read the audit trail while the admin access token is still valid; the
	// clock is advanced below.
	events := listAuditForTest(t, ts.URL, admin.AccessToken, "")
	foundAudit := false
	for _, event := range events {
		if event.Action == AuditAPITokenCreated && event.EntityID == created.APIToken.ID {
			if s, _ := event.Detail["expires_at"].(string); s == "" {
				t.Fatalf("token audit detail expires_at = %v", event.Detail["expires_at"])
			}
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatal("token created audit event not found")
	}

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, created.Token)
	requireStatus(t, resp, payload, http.StatusOK)

	clock.Advance(23 * time.Hour)
	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, created.Token)
	requireStatus(t, resp, payload, http.StatusOK)

	clock.Advance(2 * time.Hour)
	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, created.Token)
	requireStatus(t, resp, payload, http.StatusUnauthorized)

}

func TestAPITokenWithoutExpiryNeverExpires(t *testing.T) {
	ts, clock := newExpiryTestServer(t)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/tokens", map[string]any{
		"name": "forever", "role": "viewer",
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)
	var created createTokenResponse
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if created.APIToken.ExpiresAt != nil {
		t.Fatalf("expiresAt = %s, want absent", created.APIToken.ExpiresAt)
	}

	clock.Advance(10 * 365 * 24 * time.Hour)
	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, created.Token)
	requireStatus(t, resp, payload, http.StatusOK)
}

func TestAPITokenExpiresAtAcceptsFutureInstant(t *testing.T) {
	ts, clock := newExpiryTestServer(t)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)

	until := clock.Now().Add(48 * time.Hour)
	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/tokens", map[string]any{
		"name": "dated", "role": "viewer", "expiresAt": until.Format(time.RFC3339),
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)
	var created createTokenResponse
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if created.APIToken.ExpiresAt == nil || !created.APIToken.ExpiresAt.Equal(until) {
		t.Fatalf("expiresAt = %v, want %s", created.APIToken.ExpiresAt, until)
	}

	clock.Advance(47 * time.Hour)
	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, created.Token)
	requireStatus(t, resp, payload, http.StatusOK)

	clock.Advance(2 * time.Hour)
	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/projects", nil, created.Token)
	requireStatus(t, resp, payload, http.StatusUnauthorized)
}

func TestAPITokenExpiryValidation(t *testing.T) {
	ts, clock := newExpiryTestServer(t)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)
	now := clock.Now()

	cases := []struct {
		name string
		body map[string]any
	}{
		{"zero days", map[string]any{"expiresInDays": 0}},
		{"negative days", map[string]any{"expiresInDays": -1}},
		{"days above maximum", map[string]any{"expiresInDays": 3651}},
		{"past expiresAt", map[string]any{"expiresAt": now.Add(-time.Hour).Format(time.RFC3339)}},
		{"expiresAt equal to now", map[string]any{"expiresAt": now.Format(time.RFC3339)}},
		{"expiresAt beyond ten years", map[string]any{"expiresAt": now.AddDate(10, 0, 1).Format(time.RFC3339)}},
		{"malformed expiresAt", map[string]any{"expiresAt": "next tuesday"}},
		{"both expiry fields", map[string]any{"expiresInDays": 1, "expiresAt": now.Add(time.Hour).Format(time.RFC3339)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"name": "bad", "role": "viewer"}
			for k, v := range tc.body {
				body[k] = v
			}
			resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/tokens", body, admin.AccessToken)
			requireStatus(t, resp, payload, http.StatusBadRequest)
		})
	}

	// The upper bound itself is allowed.
	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/tokens", map[string]any{
		"name": "max", "role": "viewer", "expiresInDays": 3650,
	}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)
}
