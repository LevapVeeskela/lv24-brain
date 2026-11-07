package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	storeTestLogin    = "admin@example.test"
	storeTestPassword = "admin-password-1"
)

var storeTestArgon = auth.Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}

// newServerBackedClient runs the real HTTP API over an in-memory store and
// returns a client logged in as an admin. The store is returned so tests can
// seed records that the client cannot create itself.
func newServerBackedClient(t *testing.T) (*Client, *httpapi.MemoryStore) {
	t.Helper()
	ctx := context.Background()
	store := httpapi.NewMemoryStore()
	hash, err := auth.HashPassword(storeTestPassword, storeTestArgon)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, _, err := store.CreateUser(ctx, domain.User{
		Login:       storeTestLogin,
		DisplayName: "Administrator",
		Role:        domain.RoleAdmin,
		AuthSource:  "local",
	}, hash); err != nil {
		t.Fatalf("create user: %v", err)
	}
	api, err := httpapi.New(httpapi.Config{
		Secret:         []byte(strings.Repeat("k", 32)),
		Issuer:         "test-issuer",
		Audience:       "test-audience",
		AccessTTL:      5 * time.Minute,
		RefreshTTL:     time.Hour,
		TokenPrefix:    "brain_",
		MaxBodyBytes:   1 << 20,
		LoginRateLimit: 0,
		PasswordParams: storeTestArgon,
		Now:            time.Now,
	}, store)
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(func() {
		server.Close()
		api.Close()
	})

	token := loginAccessToken(t, server.URL)
	client, err := New(Config{BaseURL: server.URL, Token: token, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, store
}

func loginAccessToken(t *testing.T, baseURL string) string {
	t.Helper()
	body := strings.NewReader(fmt.Sprintf(`{"login":%q,"password":%q}`, storeTestLogin, storeTestPassword))
	resp, err := http.Post(baseURL+"/v1/auth/login", "application/json", body)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	var session struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if session.AccessToken == "" {
		t.Fatal("login returned no access token")
	}
	return session.AccessToken
}

func TestListRecordsFiltersByProject(t *testing.T) {
	ctx := context.Background()
	client, _ := newServerBackedClient(t)
	for _, id := range []string{"p1", "p2"} {
		if err := client.PutProject(ctx, domain.Project{ID: id, Name: "Project " + id}); err != nil {
			t.Fatalf("PutProject(%s): %v", id, err)
		}
	}
	for _, project := range []string{"p1", "p2"} {
		if _, err := client.AppendNote(ctx, domain.Note{ProjectID: project, Title: "note-" + project, Body: "body"}); err != nil {
			t.Fatalf("AppendNote: %v", err)
		}
		if _, err := client.AppendDecision(ctx, domain.Decision{ProjectID: project, Title: "decision-" + project}); err != nil {
			t.Fatalf("AppendDecision: %v", err)
		}
		if _, err := client.AppendAttempt(ctx, domain.Attempt{ProjectID: project, Title: "attempt-" + project}); err != nil {
			t.Fatalf("AppendAttempt: %v", err)
		}
		if _, err := client.AppendHandoff(ctx, domain.Handoff{ProjectID: project, Summary: "handoff-" + project}); err != nil {
			t.Fatalf("AppendHandoff: %v", err)
		}
	}

	notes, err := client.ListNotes(ctx, "p1")
	if err != nil || len(notes) != 1 || notes[0].Title != "note-p1" || notes[0].ProjectID != "p1" {
		t.Fatalf("ListNotes(p1) = (%+v, %v)", notes, err)
	}
	decisions, err := client.ListDecisions(ctx, "p1")
	if err != nil || len(decisions) != 1 || decisions[0].Title != "decision-p1" || decisions[0].ID == "" {
		t.Fatalf("ListDecisions(p1) = (%+v, %v)", decisions, err)
	}
	attempts, err := client.ListAttempts(ctx, "p2")
	if err != nil || len(attempts) != 1 || attempts[0].Title != "attempt-p2" {
		t.Fatalf("ListAttempts(p2) = (%+v, %v)", attempts, err)
	}
	handoffs, err := client.ListHandoffs(ctx, "p2")
	if err != nil || len(handoffs) != 1 || handoffs[0].Summary != "handoff-p2" {
		t.Fatalf("ListHandoffs(p2) = (%+v, %v)", handoffs, err)
	}
	empty, err := client.ListNotes(ctx, "unknown")
	if err != nil || len(empty) != 0 {
		t.Fatalf("ListNotes(unknown) = (%+v, %v)", empty, err)
	}
}

func TestListReadsAllPages(t *testing.T) {
	ctx := context.Background()
	client, store := newServerBackedClient(t)
	if err := client.PutProject(ctx, domain.Project{ID: "p1", Name: "Project p1"}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	const total = listPageSize + 1
	for i := 0; i < total; i++ {
		if _, _, err := store.CreateNote(ctx, domain.Note{ProjectID: "p1", Title: fmt.Sprintf("n%d", i), Body: "body"}); err != nil {
			t.Fatalf("seed note %d: %v", i, err)
		}
	}

	notes, err := client.ListNotes(ctx, "p1")
	if err != nil {
		t.Fatalf("ListNotes: %v", err)
	}
	if len(notes) != total {
		t.Fatalf("ListNotes returned %d notes, want %d", len(notes), total)
	}
	seen := make(map[string]bool, total)
	for _, note := range notes {
		if seen[note.ID] {
			t.Fatalf("duplicate note %s across pages", note.ID)
		}
		seen[note.ID] = true
	}
}

func TestUpdateDecisionStatusVisibleOnReread(t *testing.T) {
	ctx := context.Background()
	client, _ := newServerBackedClient(t)
	if err := client.PutProject(ctx, domain.Project{ID: "p1", Name: "Project p1"}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	if _, err := client.AppendDecision(ctx, domain.Decision{ProjectID: "p1", Title: "use postgres", Status: domain.DecisionProposed, Context: "why"}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	listed, err := client.ListDecisions(ctx, "p1")
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListDecisions = (%+v, %v)", listed, err)
	}
	decision := listed[0]
	decision.Status = domain.DecisionAccepted

	if err := client.UpdateDecision(ctx, decision); err != nil {
		t.Fatalf("UpdateDecision: %v", err)
	}
	reread, err := client.ListDecisions(ctx, "p1")
	if err != nil || len(reread) != 1 {
		t.Fatalf("ListDecisions after update = (%+v, %v)", reread, err)
	}
	if reread[0].Status != domain.DecisionAccepted {
		t.Fatalf("status after update = %q, want accepted", reread[0].Status)
	}
	if reread[0].Title != "use postgres" || reread[0].Context != "why" {
		t.Fatalf("content lost on update: %+v", reread[0])
	}
}

func TestUpdateDecisionRejectsOtherProjectAndMissing(t *testing.T) {
	ctx := context.Background()
	client, _ := newServerBackedClient(t)
	if err := client.PutProject(ctx, domain.Project{ID: "p1", Name: "Project p1"}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	if _, err := client.AppendDecision(ctx, domain.Decision{ProjectID: "p1", Title: "t"}); err != nil {
		t.Fatalf("AppendDecision: %v", err)
	}
	listed, err := client.ListDecisions(ctx, "p1")
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListDecisions = (%+v, %v)", listed, err)
	}

	wrongProject := listed[0]
	wrongProject.ProjectID = "p2"
	if err := client.UpdateDecision(ctx, wrongProject); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("UpdateDecision(other project) error = %v, want ErrNotFound", err)
	}

	missing := listed[0]
	missing.ID = "does-not-exist"
	if err := client.UpdateDecision(ctx, missing); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("UpdateDecision(missing) error = %v, want ErrNotFound", err)
	}
}
