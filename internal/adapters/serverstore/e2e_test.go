package serverstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestServerStoreWithHTTPClient wires the whole server-mode path: Postgres ->
// serverstore -> httpapi -> httpclient. It guards on the same database
// environment variable as the rest of the integration suite.
func TestServerStoreWithHTTPClient(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)

	params := auth.Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}
	hash, err := auth.HashPassword("admin-password-1", params)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, _, err := store.CreateUser(ctx, domain.User{
		Login: "admin@example.test", Role: domain.RoleAdmin, AuthSource: "local",
	}, hash); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	api, err := httpapi.New(httpapi.Config{
		Secret:         []byte("serverstore-e2e-signing-key-123456"),
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

	token := login(t, server.URL, "admin@example.test", "admin-password-1")
	client, err := httpclient.New(httpclient.Config{BaseURL: server.URL, Token: token})
	if err != nil {
		t.Fatalf("httpclient.New: %v", err)
	}

	if err := client.PutProject(ctx, domain.Project{ID: "p1", Name: "Alpha", Kind: domain.ProjectKindApp}); err != nil {
		t.Fatalf("PutProject create: %v", err)
	}
	if err := client.PutProject(ctx, domain.Project{ID: "p1", Name: "Alpha 2", Kind: domain.ProjectKindService}); err != nil {
		t.Fatalf("PutProject update: %v", err)
	}
	project, err := client.GetProject(ctx, "p1")
	if err != nil || project.Name != "Alpha 2" || project.Kind != domain.ProjectKindService {
		t.Fatalf("GetProject = (%+v, %v)", project, err)
	}
	projects, err := client.ListProjects(ctx)
	if err != nil || len(projects) != 1 {
		t.Fatalf("ListProjects = (%d, %v)", len(projects), err)
	}

	if _, err := client.PutState(ctx, domain.State{ProjectID: "p1", Goal: "wire wp8", Active: []string{"e2e"}}); err != nil {
		t.Fatalf("PutState: %v", err)
	}
	state, err := client.GetState(ctx, "p1")
	if err != nil || state.Goal != "wire wp8" {
		t.Fatalf("GetState = (%+v, %v)", state, err)
	}

	noteRevision, err := client.AppendNote(ctx, domain.Note{ProjectID: "p1", Title: "Note", Body: "sqlite body"})
	if err != nil || noteRevision.Value == "" {
		t.Fatalf("AppendNote = (%+v, %v)", noteRevision, err)
	}
	if _, err := client.AppendDecision(ctx, domain.Decision{ProjectID: "p1", Title: "Decision"}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	if _, err := client.AppendAttempt(ctx, domain.Attempt{ProjectID: "p1", Title: "Attempt"}); err != nil {
		t.Fatalf("AppendAttempt: %v", err)
	}
	handoffRevision, err := client.AppendHandoff(ctx, domain.Handoff{ProjectID: "p1", Summary: "Handoff"})
	if err != nil {
		t.Fatalf("AppendHandoff: %v", err)
	}

	current, err := client.CurrentRevision(ctx)
	if err != nil {
		t.Fatalf("CurrentRevision: %v", err)
	}
	if current.Kind != domain.SyncKindServer || current.Value == "0" || current.At.IsZero() {
		t.Fatalf("current revision = %+v", current)
	}
	if current.Value != handoffRevision.Value {
		t.Fatalf("current revision = %s, want last write %s", current.Value, handoffRevision.Value)
	}

	if _, err := client.GetProject(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("GetProject(missing) error = %v, want not found", err)
	}
}

func login(t *testing.T, baseURL, loginValue, password string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"login": loginValue, "password": password})
	if err != nil {
		t.Fatalf("encode login: %v", err)
	}
	response, err := http.Post(baseURL+"/v1/auth/login", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read login response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d: %s", response.StatusCode, raw)
	}
	var session struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &session); err != nil || session.AccessToken == "" {
		t.Fatalf("decode login response = %s (%v)", raw, err)
	}
	return session.AccessToken
}
