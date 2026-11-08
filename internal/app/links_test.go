package app_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// steppingClock returns a clock that advances one minute per call, so every
// record gets a distinct id and creation time.
func steppingClock() func() time.Time {
	current := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	return func() time.Time {
		current = current.Add(time.Minute)
		return current
	}
}

func newLinksLocalApp(t *testing.T) *app.App {
	t.Helper()
	store, err := fsstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	return app.New(cfg, app.Dependencies{Store: store, Now: steppingClock()})
}

// linkFixture is a small graph: a decision, a note linking to it (plain,
// display text, cross-project, missing), an attempt linking to it, and a
// record in project beta.
type linkFixture struct {
	decision, note, attempt, beta string
}

func seedLinks(t *testing.T, uc *app.App) linkFixture {
	t.Helper()
	ctx := context.Background()
	if _, err := uc.Note(ctx, app.NoteInput{ProjectID: "beta", Title: "Beta note", Body: "beta body", CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	beta := recordIDByTitle(t, uc, "beta", "Beta note")
	if _, err := uc.Decision(ctx, app.DecisionInput{ProjectID: "alpha", Title: "Use fsstore", Status: domain.DecisionAccepted, CreateProject: true}); err != nil {
		t.Fatal(err)
	}
	d := recordIDByTitle(t, uc, "alpha", "Use fsstore")
	if _, err := uc.Note(ctx, app.NoteInput{ProjectID: "alpha", Title: "Links", Body: fmt.Sprintf(
		"see [[%s]] and [[%s|the decision]]\nalso [[beta/%s]] and [[decision-gone]] and [[nope/x]]\n`[[ignored]]`", d, d, beta)}); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.Attempt(ctx, app.AttemptInput{ProjectID: "alpha", Title: "Try sqlite", Reason: "contradicts [[alpha/" + d + "]]"}); err != nil {
		t.Fatal(err)
	}
	return linkFixture{
		decision: d,
		note:     recordIDByTitle(t, uc, "alpha", "Links"),
		attempt:  recordIDByTitle(t, uc, "alpha", "Try sqlite"),
		beta:     beta,
	}
}

// recordIDByTitle reads the stored id back: in server mode the server, not
// the client, assigns record ids.
func recordIDByTitle(t *testing.T, uc *app.App, projectID, title string) string {
	t.Helper()
	records, err := uc.ListRecords(context.Background(), app.ListRecordsOptions{ProjectID: projectID})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Title == title {
			return record.ID
		}
	}
	t.Fatalf("no record titled %q in %s", title, projectID)
	return ""
}

func TestRecordLinksLocal(t *testing.T) {
	// Arrange
	uc := newLinksLocalApp(t)
	fx := seedLinks(t, uc)
	ctx := context.Background()

	// Act
	noteLinks, err := uc.RecordLinks(ctx, "alpha", fx.note)
	if err != nil {
		t.Fatal(err)
	}
	decisionLinks, err := uc.RecordLinks(ctx, "alpha", fx.decision)
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	states := []string{}
	for _, link := range noteLinks.Links {
		states = append(states, link.Ref+"="+link.State)
	}
	want := []string{fx.decision + "=found", fx.decision + "=found", "beta/" + fx.beta + "=found", "decision-gone=missing", "nope/x=missing"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Fatalf("links = %v, want %v", states, want)
	}
	first := noteLinks.Links[0]
	if first.Target.Kind != "decision" || first.Target.Status != "accepted" || first.Target.Title != "Use fsstore" || first.Field != "body" {
		t.Fatalf("resolved target = %+v", first)
	}
	if noteLinks.Links[1].Text != "the decision" || noteLinks.Links[2].ProjectID != "beta" {
		t.Fatalf("display/qualified = %+v / %+v", noteLinks.Links[1], noteLinks.Links[2])
	}
	if !strings.Contains(noteLinks.Links[4].Detail, "project nope not found") {
		t.Fatalf("missing project detail = %q", noteLinks.Links[4].Detail)
	}
	sources := map[string]string{}
	for _, backlink := range decisionLinks.Backlinks {
		sources[backlink.Source.ID] = backlink.Field
	}
	if len(decisionLinks.Backlinks) != 2 || sources[fx.note] != "body" || sources[fx.attempt] != "reason" {
		t.Fatalf("backlinks = %+v", decisionLinks.Backlinks)
	}
	if len(decisionLinks.Links) != 0 {
		t.Fatalf("decision links = %+v", decisionLinks.Links)
	}
}

func TestRecordLinksUnknownRecord(t *testing.T) {
	uc := newLinksLocalApp(t)
	seedLinks(t, uc)
	if _, err := uc.RecordLinks(context.Background(), "alpha", "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}

func TestBrokenLinksAndReviewFinding(t *testing.T) {
	// Arrange
	uc := newLinksLocalApp(t)
	fx := seedLinks(t, uc)
	ctx := context.Background()

	// Act
	broken, err := uc.BrokenLinks(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	findings, err := uc.Findings(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	betaBroken, err := uc.BrokenLinks(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	refs := []string{}
	for _, item := range broken {
		if item.Source.ID != fx.note {
			t.Fatalf("broken source = %s, want %s", item.Source.ID, fx.note)
		}
		refs = append(refs, item.Link.Ref)
	}
	if strings.Join(refs, ",") != "decision-gone,nope/x" {
		t.Fatalf("broken = %v", refs)
	}
	count := 0
	for _, finding := range findings {
		if finding.Kind == app.FindingBrokenLink {
			count++
			if finding.Fixable || finding.Evidence != fx.note || !strings.Contains(finding.Message, "[[decision-gone]]") && !strings.Contains(finding.Message, "[[nope/x]]") {
				t.Fatalf("finding = %+v", finding)
			}
		}
	}
	if count != 2 {
		t.Fatalf("broken-link findings = %d, want 2", count)
	}
	if len(betaBroken) != 0 {
		t.Fatalf("beta broken = %+v", betaBroken)
	}
	if kinds, err := app.ParseFindingKinds("broken-link"); err != nil || len(kinds) != 1 {
		t.Fatalf("ParseFindingKinds(broken-link) = %v, %v", kinds, err)
	}
}

const (
	linksServerLogin    = "admin@example.test"
	linksServerPassword = "admin-password-1"
)

// newLinksServerApp runs the real HTTP API over an in-memory store and returns
// a server-mode App whose store is the HTTP client, plus a request counter.
func newLinksServerApp(t *testing.T) (*app.App, *atomic.Int64) {
	t.Helper()
	ctx := context.Background()
	params := auth.Argon2Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}
	store := httpapi.NewMemoryStore()
	hash, err := auth.HashPassword(linksServerPassword, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateUser(ctx, domain.User{Login: linksServerLogin, DisplayName: "Admin", Role: domain.RoleAdmin, AuthSource: "local"}, hash); err != nil {
		t.Fatal(err)
	}
	api, err := httpapi.New(httpapi.Config{
		Secret: []byte(strings.Repeat("k", 32)), Issuer: "test", Audience: "test",
		AccessTTL: 5 * time.Minute, RefreshTTL: time.Hour, TokenPrefix: "brain_",
		MaxBodyBytes: 1 << 20, PasswordParams: params, Now: time.Now,
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	handler := api.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		server.Close()
		api.Close()
	})
	client, err := httpclient.New(httpclient.Config{BaseURL: server.URL, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	authClient, err := httpclient.NewAuth(client, memorySecrets{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := authClient.Login(ctx, ports.Credentials{Login: linksServerLogin, Password: linksServerPassword})
	if err != nil {
		t.Fatal(err)
	}
	client, err = httpclient.New(httpclient.Config{BaseURL: server.URL, Token: session.AccessToken, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Mode = config.ModeServer
	cfg.ServerURL = server.URL
	return app.New(cfg, app.Dependencies{Store: client, Now: steppingClock()}), &requests
}

// memorySecrets is a throwaway secret store for the login above.
type memorySecrets map[string]string

func (m memorySecrets) Get(_ context.Context, key string) (string, error) {
	if value, ok := m[key]; ok {
		return value, nil
	}
	return "", domain.ErrNotFound
}
func (m memorySecrets) Set(_ context.Context, key, value string) error { m[key] = value; return nil }
func (m memorySecrets) Delete(_ context.Context, key string) error     { delete(m, key); return nil }
func (m memorySecrets) Names(context.Context) ([]string, error)        { return nil, nil }

func TestRecordLinksServerMode(t *testing.T) {
	// Arrange
	uc, requests := newLinksServerApp(t)
	fx := seedLinks(t, uc)
	ctx := context.Background()
	requests.Store(0)

	// Act
	decisionLinks, err := uc.RecordLinks(ctx, "alpha", fx.decision)
	if err != nil {
		t.Fatal(err)
	}
	decisionRequests := requests.Load()
	noteLinks, err := uc.RecordLinks(ctx, "alpha", fx.note)
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	if len(decisionLinks.Backlinks) != 2 {
		t.Fatalf("server backlinks = %+v", decisionLinks.Backlinks)
	}
	found := 0
	for _, link := range noteLinks.Links {
		if link.State == app.LinkFound {
			found++
		}
	}
	if found != 3 {
		t.Fatalf("server links = %+v", noteLinks.Links)
	}
	// One project lookup plus one listing per record kind, independent of the
	// number of records: no request per record.
	if decisionRequests > 8 {
		t.Fatalf("RecordLinks made %d requests, want a fixed small number", decisionRequests)
	}
}
