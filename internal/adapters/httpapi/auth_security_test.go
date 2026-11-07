package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	memberLogin    = "member@example.test"
	memberPassword = "member-password-1"
	newPassword    = "member-password-2"
)

// seedMember creates a local member and returns its record.
func seedMember(t *testing.T, store *MemoryStore) UserRecord {
	t.Helper()
	record, _, err := store.CreateUser(context.Background(),
		domain.User{Login: memberLogin, Role: domain.RoleMember, AuthSource: "local"},
		mustHash(t, memberPassword))
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	return record
}

func refreshWith(t *testing.T, baseURL, refreshToken string) int {
	t.Helper()
	resp, _ := jsonRequest(t, http.MethodPost, baseURL+"/v1/auth/refresh", map[string]any{"refreshToken": refreshToken}, "")
	return resp.StatusCode
}

func meStatus(t *testing.T, baseURL, token string) int {
	t.Helper()
	resp, _ := jsonRequest(t, http.MethodGet, baseURL+"/v1/auth/me", nil, token)
	return resp.StatusCode
}

func TestLastAdminGuardOverHTTPAndStore(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	ctx := context.Background()
	second, _, err := store.CreateUser(ctx, domain.User{Login: "admin2@example.test", Role: domain.RoleAdmin, AuthSource: "local"}, mustHash(t, "admin-password-2"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.GetUserByLogin(ctx, adminLogin)
	if err != nil {
		t.Fatal(err)
	}
	session := loginAs(t, ts.URL, "admin2@example.test", "admin-password-2")

	// Demoting one of two admins is allowed.
	resp, payload := jsonRequest(t, http.MethodPatch, ts.URL+"/v1/users/"+first.ID, map[string]any{"role": "member"}, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusOK)

	// The caller is now the last admin: demotion and deletion are refused.
	resp, payload = jsonRequest(t, http.MethodPatch, ts.URL+"/v1/users/"+second.ID, map[string]any{"role": "viewer"}, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusConflict)
	if code := errorCode(t, payload); code != "last_admin" {
		t.Fatalf("demote code = %q, want last_admin", code)
	}
	resp, payload = jsonRequest(t, http.MethodDelete, ts.URL+"/v1/users/"+second.ID, nil, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusConflict)

	if _, _, err := store.UpdateUser(ctx, domain.User{ID: second.ID, Login: second.Login, Role: domain.RoleMember, AuthSource: "local"}, 0); !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("store demote = %v, want ErrLastAdmin", err)
	}
	if _, err := store.DeleteUser(ctx, second.ID, 0); !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("store delete = %v, want ErrLastAdmin", err)
	}
	// A non-admin role change is never blocked by the guard.
	member := seedMember(t, store)
	if _, _, err := store.UpdateUser(ctx, domain.User{ID: member.ID, Login: member.Login, Role: domain.RoleViewer, AuthSource: "local"}, 0); err != nil {
		t.Fatalf("viewer change = %v", err)
	}
}

func errorCode(t *testing.T, payload []byte) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Error.Code
}

func TestChangePasswordHappyAndRejections(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	seedMember(t, store)
	session := loginAs(t, ts.URL, memberLogin, memberPassword)
	path := ts.URL + "/v1/auth/password"

	resp, payload := jsonRequest(t, http.MethodPost, path, map[string]any{"current_password": "not-the-password", "new_password": newPassword}, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusForbidden)
	if code := errorCode(t, payload); code != "invalid_current_password" {
		t.Fatalf("wrong current code = %q", code)
	}

	resp, payload = jsonRequest(t, http.MethodPost, path, map[string]any{"current_password": memberPassword, "new_password": "short"}, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusBadRequest)

	resp, payload = jsonRequest(t, http.MethodPost, path, map[string]any{"current_password": memberPassword, "new_password": memberPassword}, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusBadRequest)

	resp, payload = jsonRequest(t, http.MethodPost, path, map[string]any{"current_password": memberPassword, "new_password": newPassword}, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusNoContent)

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/login", map[string]any{"login": memberLogin, "password": memberPassword}, "")
	requireStatus(t, resp, payload, http.StatusUnauthorized)
	loginAs(t, ts.URL, memberLogin, newPassword)
}

func TestChangePasswordRevokesOtherSessionsOnly(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	seedMember(t, store)
	other := loginAs(t, ts.URL, memberLogin, memberPassword)
	current := loginAs(t, ts.URL, memberLogin, memberPassword)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/password",
		map[string]any{"current_password": memberPassword, "new_password": newPassword}, current.AccessToken)
	requireStatus(t, resp, payload, http.StatusNoContent)

	if status := refreshWith(t, ts.URL, other.RefreshToken); status != http.StatusUnauthorized {
		t.Fatalf("other session refresh = %d, want 401", status)
	}
	if status := meStatus(t, ts.URL, other.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("other session access = %d, want 401", status)
	}
	if status := refreshWith(t, ts.URL, current.RefreshToken); status != http.StatusOK {
		t.Fatalf("current session refresh = %d, want 200", status)
	}
}

func TestChangePasswordRejectsOIDCAccount(t *testing.T) {
	ts, api, store := newTestServer(t, nil)
	ctx := context.Background()
	record, _, err := store.CreateUser(ctx, domain.User{Login: "sso@example.test", Role: domain.RoleMember, AuthSource: "oidc"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// An OIDC account has no local password; issue its session directly, as
	// the OIDC callback does.
	session, err := api.auth.IssueSessionForUser(ctx, record.User)
	if err != nil {
		t.Fatal(err)
	}
	access := session.AccessToken
	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/password",
		map[string]any{"current_password": "anything-here-1", "new_password": newPassword}, access)
	requireStatus(t, resp, payload, http.StatusForbidden)
}

func TestSelfServiceActionsRejectAPITokens(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	member := seedMember(t, store)
	raw := createAPIToken(t, store, member.ID, domain.RoleMember)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/password",
		map[string]any{"current_password": memberPassword, "new_password": newPassword}, raw)
	requireStatus(t, resp, payload, http.StatusForbidden)
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/sessions/revoke-all", nil, raw)
	requireStatus(t, resp, payload, http.StatusForbidden)

	// The rejected password change must not have altered the stored hash.
	loginAs(t, ts.URL, memberLogin, memberPassword)
}

func TestRevokeOwnSessionsKeepsAPITokens(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	member := seedMember(t, store)
	s1 := loginAs(t, ts.URL, memberLogin, memberPassword)
	s2 := loginAs(t, ts.URL, memberLogin, memberPassword)
	raw := createAPIToken(t, store, member.ID, domain.RoleMember)

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/sessions/revoke-all", nil, s1.AccessToken)
	requireStatus(t, resp, payload, http.StatusNoContent)

	if status := meStatus(t, ts.URL, s1.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("revoked caller access = %d, want 401", status)
	}
	if status := refreshWith(t, ts.URL, s1.RefreshToken); status != http.StatusUnauthorized {
		t.Fatalf("s1 refresh = %d, want 401", status)
	}
	if status := refreshWith(t, ts.URL, s2.RefreshToken); status != http.StatusUnauthorized {
		t.Fatalf("s2 refresh = %d, want 401", status)
	}
	if status := meStatus(t, ts.URL, raw); status != http.StatusOK {
		t.Fatalf("API token after revoke-all = %d, want 200", status)
	}
}

func TestAdminRevokesUserSessions(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	member := seedMember(t, store)
	memberSession := loginAs(t, ts.URL, memberLogin, memberPassword)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)
	path := ts.URL + "/v1/users/" + member.ID + "/sessions/revoke"

	resp, payload := jsonRequest(t, http.MethodPost, path, nil, memberSession.AccessToken)
	requireStatus(t, resp, payload, http.StatusForbidden)

	resp, payload = jsonRequest(t, http.MethodPost, path, nil, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusNoContent)
	if status := refreshWith(t, ts.URL, memberSession.RefreshToken); status != http.StatusUnauthorized {
		t.Fatalf("member refresh after admin revoke = %d, want 401", status)
	}

	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/users/missing/sessions/revoke", nil, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusNotFound)
}

// createAPIToken stores an active member-level API token for userID and
// returns its raw value.
func createAPIToken(t *testing.T, store *MemoryStore, userID string, role domain.Role) string {
	t.Helper()
	raw, hash, display, err := auth.GenerateToken(testConfig().TokenPrefix, 32)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAPIToken(context.Background(), APITokenRecord{
		UserID: userID, Name: "test", Role: role, Prefix: display, TokenHash: hash, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestOpenAPIDocumentsSessionRoutes keeps the published spec in step with the
// routes added for password change and session revocation.
func TestOpenAPIDocumentsSessionRoutes(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/openapi.json", nil, "")
	requireStatus(t, resp, payload, http.StatusOK)
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(payload, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	for path, method := range map[string]string{
		"/v1/auth/password":              "post",
		"/v1/auth/sessions/revoke-all":   "post",
		"/v1/users/{id}/sessions/revoke": "post",
	} {
		if _, ok := spec.Paths[path][method]; !ok {
			t.Errorf("openapi is missing %s %s", method, path)
		}
	}
}
