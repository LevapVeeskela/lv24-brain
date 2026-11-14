package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// itemSearchStore returns one raw item hit for every search, ignoring the
// exclude filter on purpose: the handler must drop it for project readers even
// when the store does not.
type itemSearchStore struct {
	*MemoryStore
}

func (s *itemSearchStore) SearchScoped(context.Context, ports.Query, ProjectScope) ([]ports.Hit, error) {
	return []ports.Hit{
		{ID: "note-1", ProjectID: "demo", Kind: "note", Title: "Curated", Snippet: "rollout"},
		{ID: "item:tracker/" + "0123456789abcdef0123456789abcdef01234567", ProjectID: "demo", Kind: searchKindItem, Title: "Raw", Snippet: "private mail"},
	}, nil
}

func memberToken(t *testing.T, baseURL, adminToken string) string {
	t.Helper()
	resp, payload := jsonRequest(t, http.MethodPost, baseURL+"/v1/users", map[string]any{
		"login": "member@example.test", "password": "member-password-1", "role": "member",
	}, adminToken)
	requireStatus(t, resp, payload, http.StatusCreated)
	return loginAs(t, baseURL, "member@example.test", "member-password-1").AccessToken
}

func auditEvents(t *testing.T, baseURL, adminToken, action string) []AuditEvent {
	t.Helper()
	resp, payload := jsonRequest(t, http.MethodGet, baseURL+"/v1/audit?action="+action, nil, adminToken)
	requireStatus(t, resp, payload, http.StatusOK)
	var body struct {
		Items []AuditEvent `json:"items"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode audit: %v (%s)", err, payload)
	}
	return body.Items
}

// TestConnectorRunLocalOnlyRequiresOptIn is the WB-0.8 admission check: a
// local-only run is refused with 403 and an audit event until an admin opts in.
func TestConnectorRunLocalOnlyRequiresOptIn(t *testing.T) {
	runner := &fakeConnectorRunner{run: domain.ConnectorRun{ID: "run-1", Source: "imap", Status: domain.ConnectorSuccess}}
	ts, _, _ := newTestServer(t, func(cfg *Config) { cfg.ConnectorRunner = runner })
	admin := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/connectors/run", map[string]any{
		"name": "imap", "projectId": "demo", "publish": "local-only",
	}, admin)
	requireStatus(t, resp, payload, http.StatusForbidden)
	if runner.got.Name != "" {
		t.Fatalf("refused run reached the runner: %+v", runner.got)
	}
	events := auditEvents(t, ts.URL, admin, AuditConnectorLocalOnlyDenied)
	if len(events) != 1 || events[0].EntityID != "imap" {
		t.Fatalf("audit events = %+v", events)
	}

	// Explicit opt-in starts the run and forwards the acknowledged policy.
	resp, payload = jsonRequest(t, http.MethodPost, ts.URL+"/v1/connectors/run", map[string]any{
		"name": "imap", "projectId": "demo", "publish": "local-only", "allowLocalOnly": true,
	}, admin)
	requireStatus(t, resp, payload, http.StatusOK)
	if runner.got.Name != "imap" || !runner.got.AllowLocalOnly || runner.got.Publish != "local-only" {
		t.Fatalf("runner request = %+v", runner.got)
	}
}

// TestConnectorRunImplicitLocalOnlyIsAudited proves a runner-side refusal (for
// a provider that defaults to local-only) also produces 403 and an audit event.
func TestConnectorRunImplicitLocalOnlyIsAudited(t *testing.T) {
	runner := &fakeConnectorRunner{err: fmt.Errorf("%w: provider defaults to local-only", domain.ErrForbidden)}
	ts, _, _ := newTestServer(t, func(cfg *Config) { cfg.ConnectorRunner = runner })
	admin := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken

	resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/connectors/run", map[string]any{
		"name": "imap", "projectId": "demo",
	}, admin)
	requireStatus(t, resp, payload, http.StatusForbidden)
	if events := auditEvents(t, ts.URL, admin, AuditConnectorLocalOnlyDenied); len(events) != 1 {
		t.Fatalf("audit events = %+v", events)
	}
}

func TestConnectorRunRejectsInvalidPublishPolicy(t *testing.T) {
	runner := &fakeConnectorRunner{}
	ts, _, _ := newTestServer(t, func(cfg *Config) { cfg.ConnectorRunner = runner })
	admin := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken

	for _, body := range []map[string]any{
		{"name": "github", "publish": "everywhere"},
		{"name": "github", "publish": "shared", "allowLocalOnly": true},
	} {
		resp, payload := jsonRequest(t, http.MethodPost, ts.URL+"/v1/connectors/run", body, admin)
		requireStatus(t, resp, payload, http.StatusBadRequest)
	}
	if runner.got.Name != "" {
		t.Fatalf("invalid request reached the runner: %+v", runner.got)
	}
}

// TestServerSearchRawItemsAreAdminOnly is the WB-0.8 read isolation: viewers
// and members never receive raw item hits, an explicit raw request is refused
// and audited, and an admin keeps the raw view.
func TestServerSearchRawItemsAreAdminOnly(t *testing.T) {
	store := &itemSearchStore{MemoryStore: NewMemoryStore()}
	hash, err := auth.HashPassword(adminPassword, fastArgon)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateUser(context.Background(), domain.User{
		Login: adminLogin, DisplayName: "Administrator", Role: domain.RoleAdmin, AuthSource: "local",
	}, hash); err != nil {
		t.Fatal(err)
	}
	seedSearchKinds(t, store.MemoryStore)
	api, err := New(testConfig(), store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	admin := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken
	viewer := createViewer(t, ts.URL, admin).AccessToken
	member := memberToken(t, ts.URL, admin)

	for name, token := range map[string]string{"viewer": viewer, "member": member} {
		t.Run(name+" unfiltered", func(t *testing.T) {
			resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/search?q=rollout", nil, token)
			requireStatus(t, resp, payload, http.StatusOK)
			if kinds := searchKindsOf(t, payload); len(kinds) != 1 || kinds[0] != "note" {
				t.Fatalf("kinds = %v, want only the curated note", kinds)
			}
		})
		t.Run(name+" explicit item", func(t *testing.T) {
			resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/search?q=rollout&kind=item", nil, token)
			requireStatus(t, resp, payload, http.StatusForbidden)
		})
	}

	if events := auditEvents(t, ts.URL, admin, AuditSearchRawDenied); len(events) != 2 {
		t.Fatalf("audit events = %+v, want one per refused reader", events)
	}

	resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/search?q=rollout&kind=item", nil, admin)
	requireStatus(t, resp, payload, http.StatusOK)
	if kinds := searchKindsOf(t, payload); len(kinds) != 2 || kinds[0] != "item" || kinds[1] != "note" {
		t.Fatalf("admin kinds = %v", kinds)
	}
}
