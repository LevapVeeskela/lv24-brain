package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestMembersChangeOnlyTheirOwnKnowledge covers the role rule "member: write
// own/shared knowledge": a member can update and delete what they created,
// not another member's records nor records with an unknown owner; admins can
// change anything.
func TestMembersChangeOnlyTheirOwnKnowledge(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword)
	for _, login := range []string{"alice@example.test", "bob@example.test"} {
		if _, _, err := store.CreateUser(context.Background(), domain.User{Login: login, Role: domain.RoleMember, AuthSource: "local"}, mustHash(t, "member-password-1")); err != nil {
			t.Fatalf("create %s: %v", login, err)
		}
	}
	alice := loginAs(t, ts.URL, "alice@example.test", "member-password-1")
	bob := loginAs(t, ts.URL, "bob@example.test", "member-password-1")

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects", map[string]any{"id": "shared", "name": "Shared", "kind": "app"}, alice.AccessToken)
	requireStatus(t, resp, payload, http.StatusCreated)

	create := func(token, kind string, body map[string]any) string {
		t.Helper()
		resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/projects/shared/"+kind, body, token)
		requireStatus(t, resp, payload, http.StatusCreated)
		var record struct {
			ID        string `json:"id"`
			CreatedBy string `json:"createdBy"`
		}
		if err := json.Unmarshal(payload, &record); err != nil || record.ID == "" || record.CreatedBy == "" {
			t.Fatalf("create %s = %s, %v (owner must be recorded)", kind, payload, err)
		}
		return record.ID
	}
	kinds := []struct {
		path   string
		create map[string]any
		update map[string]any
	}{
		{"notes", map[string]any{"title": "n", "body": "note body"}, map[string]any{"title": "n2", "body": "edited"}},
		{"decisions", map[string]any{"title": "d"}, map[string]any{"title": "d2", "status": "accepted"}},
		{"attempts", map[string]any{"title": "a"}, map[string]any{"title": "a2"}},
		{"handoffs", map[string]any{"summary": "h"}, nil},
	}
	for _, kind := range kinds {
		t.Run(kind.path, func(t *testing.T) {
			id := create(alice.AccessToken, kind.path, kind.create)
			item := ts.URL + "/v1/" + kind.path + "/" + id

			if kind.update != nil {
				resp, payload := jsonRequest(t, http.MethodPatch, item, kind.update, bob.AccessToken)
				requireStatus(t, resp, payload, http.StatusForbidden)
				resp, payload = jsonRequest(t, http.MethodPatch, item, kind.update, alice.AccessToken)
				requireStatus(t, resp, payload, http.StatusOK)
			}
			resp, payload := jsonRequest(t, http.MethodDelete, item, nil, bob.AccessToken)
			requireStatus(t, resp, payload, http.StatusForbidden)
			resp, payload = jsonRequest(t, http.MethodDelete, item, nil, alice.AccessToken)
			requireStatus(t, resp, payload, http.StatusNoContent)

			// Admins may delete anyone's record.
			other := create(bob.AccessToken, kind.path, kind.create)
			resp, payload = jsonRequest(t, http.MethodDelete, ts.URL+"/v1/"+kind.path+"/"+other, nil, admin.AccessToken)
			requireStatus(t, resp, payload, http.StatusNoContent)
		})
	}

	// A record with an unknown owner (pre-ownership data) is admin-only.
	legacy, _, err := store.CreateNote(context.Background(), domain.Note{ProjectID: "shared", Body: "legacy"})
	if err != nil || legacy.CreatedBy != "" {
		t.Fatalf("legacy note = %+v, %v", legacy, err)
	}
	resp, payload = jsonRequest(t, http.MethodDelete, ts.URL+"/v1/notes/"+legacy.ID, nil, alice.AccessToken)
	requireStatus(t, resp, payload, http.StatusForbidden)
	resp, payload = jsonRequest(t, http.MethodDelete, ts.URL+"/v1/notes/"+legacy.ID, nil, admin.AccessToken)
	requireStatus(t, resp, payload, http.StatusNoContent)

	// Missing records still answer 404, not 403.
	resp, payload = jsonRequest(t, http.MethodDelete, ts.URL+"/v1/notes/missing", nil, alice.AccessToken)
	requireStatus(t, resp, payload, http.StatusNotFound)
}
