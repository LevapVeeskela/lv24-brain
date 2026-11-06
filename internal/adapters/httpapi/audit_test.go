package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func listAuditForTest(t *testing.T, baseURL, token, query string) []AuditEvent {
	t.Helper()
	resp, payload := jsonRequest(t, http.MethodGet, baseURL+"/v1/audit"+query, nil, token)
	requireStatus(t, resp, payload, http.StatusOK)
	var body struct {
		Items []AuditEvent `json:"items"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode audit: %v; body = %s", err, payload)
	}
	return body.Items
}

func TestSecurityEventsAreAuditedForAdminsOnly(t *testing.T) {
	ts, _, store := newTestServer(t, nil)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/login",
		map[string]string{"login": adminLogin, "password": "wrong-password-123"}, "")
	requireStatus(t, resp, payload, http.StatusUnauthorized)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/tokens",
		map[string]any{"name": "ci"}, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)
	var created createTokenResponse
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode token: %v", err)
	}

	events := listAuditForTest(t, ts.URL, admin.AccessToken, "")
	byAction := map[string]AuditEvent{}
	for _, event := range events {
		byAction[event.Action] = event
	}
	failed, ok := byAction[AuditLoginFailed]
	if !ok || failed.Actor != adminLogin || failed.Detail[auditDetailClientField] == "" {
		t.Fatalf("login failure event = %+v (all %+v)", failed, events)
	}
	if success := byAction[AuditLoginSucceeded]; success.Actor != adminLogin || success.EntityID == "" {
		t.Fatalf("login success event = %+v", success)
	}
	if token := byAction[AuditAPITokenCreated]; token.EntityID != created.APIToken.ID || token.Actor != adminLogin {
		t.Fatalf("token event = %+v", token)
	}

	// Nothing secret may end up in audit detail.
	raw, _ := json.Marshal(events)
	for _, secret := range []string{"wrong-password-123", adminPassword, created.Token, admin.AccessToken, admin.RefreshToken} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("audit leaks a secret: %s", raw)
		}
	}

	// Filters work.
	if only := listAuditForTest(t, ts.URL, admin.AccessToken, "?action="+AuditLoginFailed); len(only) != 1 {
		t.Fatalf("filtered audit = %+v", only)
	}

	// Security events never reach the viewer-readable revision stream.
	revisions, err := store.ListRevisions(t.Context(), RevisionFilter{})
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	for _, revision := range revisions {
		if strings.HasPrefix(string(revision.Kind), "auth.") || revision.EntityKind == auditEntityAPIToken {
			t.Fatalf("security event leaked into revisions: %+v", revision)
		}
	}

	// Members cannot read the audit log.
	if _, _, err := store.CreateUser(t.Context(), domain.User{Login: "member@example.test", Role: domain.RoleMember, AuthSource: "local"}, mustHash(t, "member-password-1")); err != nil {
		t.Fatalf("create member: %v", err)
	}
	member := loginAs(t, ts.URL, "member@example.test", "member-password-1")
	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/audit", nil, member.AccessToken)
	requireStatus(t, resp, payload, http.StatusForbidden)

	resp, payload = jsonRequest(t, http.MethodGet, ts.URL+"/v1/audit?since=yesterday", nil, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusBadRequest)
}

func TestMemoryAuditIsBounded(t *testing.T) {
	store := NewMemoryStore()
	for i := 0; i < maxMemoryAuditEvents+5; i++ {
		if err := store.RecordSecurityEvent(t.Context(), SecurityEvent{Action: AuditLogout}); err != nil {
			t.Fatal(err)
		}
	}
	store.mu.RLock()
	size := len(store.audit)
	store.mu.RUnlock()
	if size != maxMemoryAuditEvents {
		t.Fatalf("audit size = %d, want %d", size, maxMemoryAuditEvents)
	}
	if err := store.RecordSecurityEvent(t.Context(), SecurityEvent{}); err == nil {
		t.Fatal("empty action accepted")
	}
}

func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := auth.HashPassword(password, fastArgon)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return hash
}
