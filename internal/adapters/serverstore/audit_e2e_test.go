package serverstore

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestServerAuditActorAndSecurityEvents runs against PostgreSQL: the
// authenticated login must reach revision rows (it was empty before the actor
// context key was shared), and security events must be stored without a
// revision so viewers never see them.
func TestServerAuditActorAndSecurityEvents(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)

	params := auth.Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}
	hash, err := auth.HashPassword("admin-password-1", params)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, _, err := store.CreateUser(ctx, domain.User{
		Login: "auditor@example.test", Role: domain.RoleAdmin, AuthSource: "local",
	}, hash); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	api, err := httpapi.New(httpapi.Config{
		Secret:         []byte("serverstore-audit-signing-key-123456"),
		AccessTTL:      5 * time.Minute,
		RefreshTTL:     time.Hour,
		PasswordParams: params,
	}, store)
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		server.Close()
		api.Close()
	})

	// A failed login is a security event attributed to the attempted login.
	raw, _ := json.Marshal(map[string]string{"login": "auditor@example.test", "password": "nope-nope-nope"})
	resp, err := http.Post(server.URL+"/v1/auth/login", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("failed login: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("failed login status = %d", resp.StatusCode)
	}

	token := login(t, server.URL, "auditor@example.test", "admin-password-1")
	client, err := httpclient.New(httpclient.Config{BaseURL: server.URL, Token: token})
	if err != nil {
		t.Fatalf("httpclient.New: %v", err)
	}
	if err := client.PutProject(ctx, domain.Project{ID: "audited", Name: "Audited", Kind: domain.ProjectKindApp}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}

	revisions, err := store.ListRevisions(ctx, httpapi.RevisionFilter{EntityID: "audited"})
	if err != nil || len(revisions) == 0 {
		t.Fatalf("revisions = %+v, %v", revisions, err)
	}
	for _, revision := range revisions {
		if revision.Actor != "auditor@example.test" {
			t.Fatalf("revision actor = %q, want the authenticated login", revision.Actor)
		}
	}

	events, err := store.ListAuditEvents(ctx, httpapi.AuditFilter{Action: httpapi.AuditLoginFailed})
	if err != nil || len(events) != 1 {
		t.Fatalf("failed-login audit = %+v, %v", events, err)
	}
	if events[0].Actor != "auditor@example.test" || events[0].Revision != 0 {
		t.Fatalf("failed-login event = %+v", events[0])
	}
	all, err := store.ListRevisions(ctx, httpapi.RevisionFilter{})
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	for _, revision := range all {
		if revision.EntityKind == "user" && revision.Actor == "" && revision.Detail["action"] == httpapi.AuditLoginFailed {
			t.Fatalf("security event produced a revision: %+v", revision)
		}
	}
	succeeded, err := store.ListAuditEvents(ctx, httpapi.AuditFilter{Action: httpapi.AuditLoginSucceeded})
	if err != nil || len(succeeded) != 1 || succeeded[0].EntityID == "" {
		t.Fatalf("login-success audit = %+v, %v", succeeded, err)
	}
}
