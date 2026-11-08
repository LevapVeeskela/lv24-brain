package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// aclFixture: project "open" has no members; project "secret" is restricted
// with maintainer=alice, member=bob, member=vera (a global viewer, so capped
// to read). carol is a global member without membership.
type aclFixture struct {
	url    string
	store  *MemoryStore
	admin  string
	tokens map[string]string
	users  map[string]UserRecord
}

func newACLFixture(t *testing.T) aclFixture {
	t.Helper()
	ts, _, store := newTestServer(t, nil)
	f := aclFixture{
		url: ts.URL, store: store,
		admin:  loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken,
		tokens: map[string]string{}, users: map[string]UserRecord{},
	}
	for login, role := range map[string]domain.Role{
		"alice": domain.RoleMember, "bob": domain.RoleMember, "carol": domain.RoleMember, "vera": domain.RoleViewer,
	} {
		record, _, err := store.CreateUser(context.Background(), domain.User{Login: login, Role: role, AuthSource: "local"}, "")
		if err != nil {
			t.Fatalf("create %s: %v", login, err)
		}
		f.users[login] = record
		f.tokens[login] = createAPIToken(t, store, record.ID, role)
	}
	for _, id := range []string{"open", "secret"} {
		f.do(t, http.MethodPost, "/v1/projects", map[string]any{"id": id, "name": id}, f.admin, http.StatusCreated)
	}
	for login, role := range map[string]string{"alice": "maintainer", "bob": "member", "vera": "member"} {
		f.do(t, http.MethodPut, "/v1/projects/secret/members/"+login, map[string]any{"role": role}, f.admin, http.StatusCreated)
	}
	return f
}

func (f aclFixture) do(t *testing.T, method, path string, body any, token string, want int) []byte {
	t.Helper()
	resp, payload := jsonRequest(t, method, f.url+path, body, token)
	requireStatus(t, resp, payload, want)
	return payload
}

func itemIDs(t *testing.T, payload []byte, field string) []string {
	t.Helper()
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode items: %v; body = %s", err, payload)
	}
	ids := make([]string, 0, len(body.Items))
	for _, item := range body.Items {
		value, _ := item[field].(string)
		ids = append(ids, value)
	}
	return ids
}

func createdID(t *testing.T, payload []byte) string {
	t.Helper()
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.ID == "" {
		t.Fatalf("decode id: %v; body = %s", err, payload)
	}
	return body.ID
}

func TestProjectACLOpenProjectKeepsGlobalRoles(t *testing.T) {
	f := newACLFixture(t)
	f.do(t, http.MethodGet, "/v1/projects/open", nil, f.tokens["carol"], http.StatusOK)
	f.do(t, http.MethodPost, "/v1/projects/open/notes", map[string]any{"body": "x"}, f.tokens["carol"], http.StatusCreated)
	f.do(t, http.MethodPut, "/v1/projects/open/state", map[string]any{"goal": "g"}, f.tokens["carol"], http.StatusOK)
	f.do(t, http.MethodPatch, "/v1/projects/open", map[string]any{"name": "Open"}, f.tokens["carol"], http.StatusOK)
	f.do(t, http.MethodGet, "/v1/projects/open/notes", nil, f.tokens["vera"], http.StatusOK)
	f.do(t, http.MethodPost, "/v1/projects/open/notes", map[string]any{"body": "x"}, f.tokens["vera"], http.StatusForbidden)
}

func TestProjectACLRestrictedProjectRoles(t *testing.T) {
	f := newACLFixture(t)
	carol, bob, alice, vera := f.tokens["carol"], f.tokens["bob"], f.tokens["alice"], f.tokens["vera"]

	// Non-members get 404 everywhere, so the project's existence does not leak.
	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/projects/secret", nil},
		{http.MethodGet, "/v1/projects/secret/state", nil},
		{http.MethodPut, "/v1/projects/secret/state", map[string]any{"goal": "g"}},
		{http.MethodGet, "/v1/projects/secret/notes", nil},
		{http.MethodPost, "/v1/projects/secret/notes", map[string]any{"body": "x"}},
		{http.MethodPatch, "/v1/projects/secret", map[string]any{"name": "x"}},
	} {
		f.do(t, tc.method, tc.path, tc.body, carol, http.StatusNotFound)
	}
	if ids := itemIDs(t, f.do(t, http.MethodGet, "/v1/projects", nil, carol, http.StatusOK), "id"); slices.Contains(ids, "secret") {
		t.Fatalf("carol lists %v, want no secret", ids)
	}

	// Member: write records and state, not project metadata.
	bobNote := createdID(t, f.do(t, http.MethodPost, "/v1/projects/secret/notes", map[string]any{"body": "bob"}, bob, http.StatusCreated))
	f.do(t, http.MethodPut, "/v1/projects/secret/state", map[string]any{"goal": "g"}, bob, http.StatusOK)
	f.do(t, http.MethodPatch, "/v1/projects/secret", map[string]any{"name": "S"}, bob, http.StatusForbidden)
	aliceNote := createdID(t, f.do(t, http.MethodPost, "/v1/projects/secret/notes", map[string]any{"body": "alice"}, alice, http.StatusCreated))
	f.do(t, http.MethodDelete, "/v1/notes/"+aliceNote, nil, bob, http.StatusForbidden)

	// Global viewer with project role member stays read-only.
	f.do(t, http.MethodGet, "/v1/notes/"+bobNote, nil, vera, http.StatusOK)
	f.do(t, http.MethodPost, "/v1/projects/secret/notes", map[string]any{"body": "v"}, vera, http.StatusForbidden)

	// Non-member cannot reach records by id either.
	f.do(t, http.MethodGet, "/v1/notes/"+bobNote, nil, carol, http.StatusNotFound)
	f.do(t, http.MethodPatch, "/v1/notes/"+bobNote, map[string]any{"body": "y"}, carol, http.StatusNotFound)

	// Maintainer: project metadata and other members' records.
	f.do(t, http.MethodPatch, "/v1/projects/secret", map[string]any{"name": "Secret"}, alice, http.StatusOK)
	f.do(t, http.MethodPatch, "/v1/notes/"+bobNote, map[string]any{"body": "edited"}, alice, http.StatusOK)
	f.do(t, http.MethodDelete, "/v1/notes/"+bobNote, nil, alice, http.StatusNoContent)

	// Admin bypasses membership.
	f.do(t, http.MethodGet, "/v1/projects/secret/notes", nil, f.admin, http.StatusOK)
	f.do(t, http.MethodDelete, "/v1/notes/"+aliceNote, nil, f.admin, http.StatusNoContent)
	payload := f.do(t, http.MethodGet, "/v1/projects/secret", nil, f.admin, http.StatusOK)
	if !strings.Contains(string(payload), `"restricted":true`) {
		t.Fatalf("project payload lacks restricted flag: %s", payload)
	}
}

func TestProjectACLSearchAndRevisionsFilterUnreadableProjects(t *testing.T) {
	f := newACLFixture(t)
	f.do(t, http.MethodPost, "/v1/projects/open/notes", map[string]any{"title": "zebra open", "body": "zebra"}, f.tokens["bob"], http.StatusCreated)
	secretNote := createdID(t, f.do(t, http.MethodPost, "/v1/projects/secret/notes",
		map[string]any{"title": "zebra secret", "body": "zebra"}, f.tokens["bob"], http.StatusCreated))

	carolHits := itemIDs(t, f.do(t, http.MethodGet, "/v1/search?q=zebra", nil, f.tokens["carol"], http.StatusOK), "ProjectID")
	if !slices.Equal(carolHits, []string{"open"}) {
		t.Fatalf("carol search projects = %v, want [open]", carolHits)
	}
	if hits := itemIDs(t, f.do(t, http.MethodGet, "/v1/search?q=zebra", nil, f.tokens["bob"], http.StatusOK), "ProjectID"); len(hits) != 2 {
		t.Fatalf("bob search projects = %v, want both", hits)
	}
	f.do(t, http.MethodGet, "/v1/search?q=zebra&projectId=secret", nil, f.tokens["carol"], http.StatusNotFound)

	revisions := itemIDs(t, f.do(t, http.MethodGet, "/v1/revisions?limit=500", nil, f.tokens["carol"], http.StatusOK), "entityId")
	if slices.Contains(revisions, secretNote) || slices.Contains(revisions, "secret") {
		t.Fatalf("carol revisions leak the secret project: %v", revisions)
	}
	if revisions := itemIDs(t, f.do(t, http.MethodGet, "/v1/revisions?limit=500", nil, f.tokens["bob"], http.StatusOK), "entityId"); !slices.Contains(revisions, secretNote) {
		t.Fatalf("bob revisions miss the secret note: %v", revisions)
	}
}

func TestProjectACLEventStreamFiltersUnreadableProjects(t *testing.T) {
	f := newACLFixture(t)
	replayed := createdID(t, f.do(t, http.MethodPost, "/v1/projects/secret/notes", map[string]any{"body": "before"}, f.tokens["bob"], http.StatusCreated))

	stream := openSSE(t, f.url, f.tokens["carol"], "")
	defer stream.Close()
	live := createdID(t, f.do(t, http.MethodPost, "/v1/projects/secret/notes", map[string]any{"body": "during"}, f.tokens["bob"], http.StatusCreated))
	visible := createdID(t, f.do(t, http.MethodPost, "/v1/projects/open/notes", map[string]any{"body": "open"}, f.tokens["bob"], http.StatusCreated))

	deadline := time.After(2 * time.Second)
	for {
		frame, ok := stream.NextFrame(time.Second)
		if !ok {
			t.Fatal("stream ended before the open-project event")
		}
		if strings.Contains(frame, replayed) || strings.Contains(frame, live) || strings.Contains(frame, `"secret"`) {
			t.Fatalf("carol received a secret-project event: %s", frame)
		}
		if strings.Contains(frame, visible) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("open-project event not delivered")
		default:
		}
	}
}

func TestProjectMembershipAdminAPIAndAudit(t *testing.T) {
	f := newACLFixture(t)
	// Only admins manage memberships.
	f.do(t, http.MethodPut, "/v1/projects/secret/members/carol", map[string]any{"role": "member"}, f.tokens["alice"], http.StatusForbidden)
	f.do(t, http.MethodPut, "/v1/projects/secret/members/carol", map[string]any{"role": "owner"}, f.admin, http.StatusBadRequest)
	f.do(t, http.MethodPut, "/v1/projects/secret/members/nobody", map[string]any{"role": "member"}, f.admin, http.StatusNotFound)

	// Update by user id answers 200 and records the previous role.
	f.do(t, http.MethodPut, "/v1/projects/secret/members/"+f.users["bob"].ID, map[string]any{"role": "viewer"}, f.admin, http.StatusOK)
	f.do(t, http.MethodPost, "/v1/projects/secret/notes", map[string]any{"body": "x"}, f.tokens["bob"], http.StatusForbidden)
	members := itemIDs(t, f.do(t, http.MethodGet, "/v1/projects/secret/members", nil, f.admin, http.StatusOK), "login")
	if !slices.Equal(members, []string{"alice", "bob", "vera"}) {
		t.Fatalf("members = %v", members)
	}

	// Reopening with members is refused; removing every member keeps the
	// project restricted (admin-only) until it is reopened explicitly.
	f.do(t, http.MethodPut, "/v1/projects/secret/access", map[string]any{"restricted": false}, f.admin, http.StatusConflict)
	for _, login := range []string{"alice", "bob", "vera"} {
		f.do(t, http.MethodDelete, "/v1/projects/secret/members/"+login, nil, f.admin, http.StatusNoContent)
	}
	f.do(t, http.MethodDelete, "/v1/projects/secret/members/alice", nil, f.admin, http.StatusNotFound)
	f.do(t, http.MethodGet, "/v1/projects/secret", nil, f.tokens["alice"], http.StatusNotFound)
	f.do(t, http.MethodPut, "/v1/projects/secret/access", map[string]any{"restricted": false}, f.admin, http.StatusOK)
	f.do(t, http.MethodGet, "/v1/projects/secret", nil, f.tokens["carol"], http.StatusOK)

	actions := map[string]int{}
	for _, event := range listAuditForTest(t, f.url, f.admin, "?entityKind=project&limit=100") {
		actions[event.Action]++
	}
	want := map[string]int{
		AuditProjectMemberAdded: 3, AuditProjectMemberUpdated: 1, AuditProjectMemberRemoved: 3, AuditProjectAccessOpen: 1,
	}
	for action, count := range want {
		if actions[action] != count {
			t.Fatalf("audit %s = %d, want %d (all: %v)", action, actions[action], count, actions)
		}
	}
}

func mintToken(t *testing.T, f aclFixture, caller string, body map[string]any, want int) (string, []byte) {
	t.Helper()
	body["name"] = "scoped"
	payload := f.do(t, http.MethodPost, "/v1/tokens", body, caller, want)
	if want != http.StatusCreated {
		return "", payload
	}
	var created createTokenResponse
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	return created.Token, payload
}

func TestAPITokenScopes(t *testing.T) {
	f := newACLFixture(t)
	readOnly, payload := mintToken(t, f, f.tokens["bob"], map[string]any{"scopes": []string{"read"}}, http.StatusCreated)
	if !strings.Contains(string(payload), `"scopes":["read"]`) {
		t.Fatalf("token payload lacks scopes: %s", payload)
	}
	f.do(t, http.MethodGet, "/v1/projects/secret/notes", nil, readOnly, http.StatusOK)
	resp, body := jsonRequest(t, http.MethodPost, f.url+"/v1/projects/secret/notes", map[string]any{"body": "x"}, readOnly)
	requireStatus(t, resp, body, http.StatusForbidden)
	if code := errorCode(t, body); code != "insufficient_scope" {
		t.Fatalf("error code = %q, want insufficient_scope", code)
	}
	f.do(t, http.MethodPut, "/v1/projects/open/state", map[string]any{"goal": "g"}, readOnly, http.StatusForbidden)

	// A scoped token cannot mint a wider or unscoped token.
	mintToken(t, f, readOnly, map[string]any{"scopes": []string{"write"}}, http.StatusForbidden)
	mintToken(t, f, readOnly, map[string]any{}, http.StatusForbidden)

	// Validation: unknown scope, admin scope without admin role.
	mintToken(t, f, f.tokens["bob"], map[string]any{"scopes": []string{"delete"}}, http.StatusBadRequest)
	mintToken(t, f, f.tokens["bob"], map[string]any{"scopes": []string{"admin"}}, http.StatusBadRequest)

	// Admin endpoints need the admin scope even for admins.
	adminWrite, _ := mintToken(t, f, f.admin, map[string]any{"scopes": []string{"read", "write"}}, http.StatusCreated)
	f.do(t, http.MethodGet, "/v1/users", nil, adminWrite, http.StatusForbidden)
	f.do(t, http.MethodPost, "/v1/projects/open/notes", map[string]any{"body": "x"}, adminWrite, http.StatusCreated)
	adminScope, _ := mintToken(t, f, f.admin, map[string]any{"scopes": []string{"admin"}}, http.StatusCreated)
	f.do(t, http.MethodGet, "/v1/users", nil, adminScope, http.StatusOK)

	// Token creation is audited with its scopes.
	found := false
	for _, event := range listAuditForTest(t, f.url, f.admin, "?action="+AuditAPITokenCreated+"&limit=100") {
		if scopes, ok := event.Detail["scopes"].([]any); ok && len(scopes) == 1 && scopes[0] == "read" {
			found = true
		}
	}
	if !found {
		t.Fatal("token creation audit lacks scopes")
	}
}

func TestAPITokenProjectAllowList(t *testing.T) {
	f := newACLFixture(t)
	f.do(t, http.MethodPost, "/v1/projects/open/notes", map[string]any{"title": "zebra", "body": "zebra"}, f.tokens["alice"], http.StatusCreated)
	f.do(t, http.MethodPost, "/v1/projects/secret/notes", map[string]any{"title": "zebra", "body": "zebra"}, f.tokens["alice"], http.StatusCreated)

	openOnly, _ := mintToken(t, f, f.tokens["alice"], map[string]any{"projectIds": []string{"open"}}, http.StatusCreated)
	f.do(t, http.MethodGet, "/v1/projects/secret", nil, openOnly, http.StatusNotFound)
	f.do(t, http.MethodPost, "/v1/projects/secret/notes", map[string]any{"body": "x"}, openOnly, http.StatusNotFound)
	f.do(t, http.MethodPost, "/v1/projects/open/notes", map[string]any{"body": "x"}, openOnly, http.StatusCreated)
	f.do(t, http.MethodPost, "/v1/projects", map[string]any{"id": "other", "name": "other"}, openOnly, http.StatusForbidden)
	if ids := itemIDs(t, f.do(t, http.MethodGet, "/v1/projects", nil, openOnly, http.StatusOK), "id"); !slices.Equal(ids, []string{"open"}) {
		t.Fatalf("allow-listed projects = %v", ids)
	}
	if hits := itemIDs(t, f.do(t, http.MethodGet, "/v1/search?q=zebra", nil, openOnly, http.StatusOK), "ProjectID"); !slices.Equal(hits, []string{"open"}) {
		t.Fatalf("allow-listed search = %v", hits)
	}
	// Narrowing only: no other project, no unlimited token.
	mintToken(t, f, openOnly, map[string]any{"projectIds": []string{"secret"}}, http.StatusForbidden)
	mintToken(t, f, openOnly, map[string]any{}, http.StatusForbidden)
	mintToken(t, f, f.tokens["alice"], map[string]any{"projectIds": []string{"missing"}}, http.StatusBadRequest)

	// An admin token limited to "open" cannot manage "secret".
	adminOpen, _ := mintToken(t, f, f.admin, map[string]any{"projectIds": []string{"open"}}, http.StatusCreated)
	f.do(t, http.MethodGet, "/v1/projects/secret/members", nil, adminOpen, http.StatusNotFound)
	f.do(t, http.MethodDelete, "/v1/projects/secret", nil, adminOpen, http.StatusNotFound)
}

func TestOpenAPIDocumentsProjectAccessRoutes(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/openapi.json", nil, "")
	requireStatus(t, resp, payload, http.StatusOK)
	var spec struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(payload, &spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	for path, methods := range map[string][]string{
		"/v1/projects/{id}/members":        {"get"},
		"/v1/projects/{id}/members/{user}": {"put", "delete"},
		"/v1/projects/{id}/access":         {"put"},
	} {
		for _, method := range methods {
			if _, ok := spec.Paths[path][method]; !ok {
				t.Errorf("openapi is missing %s %s", method, path)
			}
		}
	}
	if !strings.Contains(string(payload), `"scopes"`) || !strings.Contains(string(payload), `"projectIds"`) {
		t.Error("openapi token request lacks scopes/projectIds")
	}
}
