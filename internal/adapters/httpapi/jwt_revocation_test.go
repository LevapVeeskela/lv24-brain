package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestAccessTokenStopsWorkingAfterLogoutRoleChangeOrDeletion covers the gap
// where an access JWT stayed valid for its whole TTL after logout, a role
// change, or user deletion.
func TestAccessTokenStopsWorkingAfterLogoutRoleChangeOrDeletion(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	ctx := context.Background()
	member, _, err := store.CreateUser(ctx, domain.User{Login: "m@example.test", Role: domain.RoleMember, AuthSource: "local"}, mustHash(t, "member-password-1"))
	if err != nil {
		t.Fatal(err)
	}
	me := func(token string) int {
		resp, _ := jsonRequest(t, http.MethodGet, ts.URL+"/v1/auth/me", nil, token)
		return resp.StatusCode
	}

	// Logout revokes the session: the same access token is rejected at once.
	session := loginAs(t, ts.URL, "m@example.test", "member-password-1")
	if status := me(session.AccessToken); status != http.StatusOK {
		t.Fatalf("before logout = %d", status)
	}
	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/auth/logout", nil, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusNoContent)
	if status := me(session.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("after logout = %d, want 401", status)
	}

	// A demotion applies immediately: a member token cannot write after the
	// user becomes a viewer.
	session = loginAs(t, ts.URL, "m@example.test", "member-password-1")
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{"id": "p1", "name": "P1", "kind": "app"}, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)
	demoted := member.User
	demoted.Role = domain.RoleViewer
	if _, _, err := store.UpdateUser(ctx, demoted, 0); err != nil {
		t.Fatalf("demote: %v", err)
	}
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{"id": "p2", "name": "P2", "kind": "app"}, session.AccessToken)
	requireStatus(t, resp, payload, http.StatusForbidden)

	// A deleted user's token is rejected.
	if _, err := store.DeleteUser(ctx, member.ID, 0); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if status := me(session.AccessToken); status != http.StatusUnauthorized {
		t.Fatalf("after delete = %d, want 401", status)
	}
}
