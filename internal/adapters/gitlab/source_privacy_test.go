package gitlab_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/gitlab"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ingestiontest"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// TestGitLabStoredItemHashesAuthorEmailForSharedZone runs the real GitLab
// source through the ingest policy with an explicitly shared raw zone (the
// GitLab default) and proves that the stored commit item no longer carries the
// commit author's address: the descriptor marks GitLab as PersonalData, so the
// host replaces the address with the stable per-connection placeholder before
// anything reaches the shared zone.
func TestGitLabStoredItemHashesAuthorEmailForSharedZone(t *testing.T) {
	const token = "gl-privacy-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/repository/commits") {
			_, _ = w.Write([]byte(`[{"id":"abc123","title":"Commit title","message":"Commit title","author_name":"Alice Smith","author_email":"alice@example.test","committed_date":"2026-11-01T00:00:00Z","authored_date":"2026-11-01T00:00:00Z","web_url":"https://gitlab.test/acme/widgets/-/commit/abc123"}]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	clock := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	connection := domain.Connection{
		ID:       "gl-privacy",
		Provider: gitlab.Name,
		BaseURL:  server.URL,
		Settings: map[string]string{"projects": "acme/widgets"},
		Publish:  domain.PublishShared,
	}
	items := ingestiontest.NewMemoryItemStore()
	ingestion := &app.Ingestion{
		Connections: privacyConnectionStore{connection: connection},
		Registry:    connectorbuild.Builtin(),
		Deps: connectorbuild.Deps{
			Credentials: privacyCredentials{token: token},
			HTTPClient:  func() *http.Client { return server.Client() },
			Now:         func() time.Time { return clock },
		},
		Items:       items,
		Checkpoints: ingestiontest.NewMemoryCheckpointStore(),
		Locker:      privacyLocker{},
		Now:         func() time.Time { return clock },
	}
	result, err := ingestion.RunConnection(context.Background(), app.RunConnectionOptions{ConnectionID: connection.ID, MaxBatches: 10})
	if err != nil {
		t.Fatalf("RunConnection: %v", err)
	}
	if result.Publish != domain.PublishShared {
		t.Fatalf("publish = %q, want shared", result.Publish)
	}

	page, err := items.List(context.Background(), connection.ID, ports.ItemListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Kind != domain.ItemCommit {
		t.Fatalf("items = %#v, want one commit", page.Items)
	}
	item := page.Items[0]
	for name, value := range map[string]string{
		"title": item.Title, "body": item.Body, "url": item.URL, "author": item.Author, "project": item.ProjectID,
	} {
		if strings.Contains(value, "@") || strings.Contains(value, "alice@example.test") {
			t.Fatalf("%s = %q, want no raw author address in a stored shared item", name, value)
		}
	}
	if !strings.Contains(item.Author, "Alice Smith") || !strings.Contains(item.Author, "[email:") {
		t.Fatalf("author = %q, want the name and an e-mail placeholder", item.Author)
	}
}

type privacyConnectionStore struct{ connection domain.Connection }

func (s privacyConnectionStore) List(context.Context) ([]domain.Connection, error) {
	return []domain.Connection{s.connection}, nil
}

func (s privacyConnectionStore) Get(_ context.Context, id string) (domain.Connection, error) {
	if id != s.connection.ID {
		return domain.Connection{}, domain.ErrNotFound
	}
	return s.connection, nil
}

func (s privacyConnectionStore) Create(context.Context, domain.Connection) error {
	return domain.ErrUnsupported
}

func (s privacyConnectionStore) Update(context.Context, domain.Connection) error {
	return domain.ErrUnsupported
}

func (s privacyConnectionStore) Patch(context.Context, string, func(*domain.Connection) error) (domain.Connection, error) {
	return domain.Connection{}, domain.ErrUnsupported
}

func (s privacyConnectionStore) Delete(context.Context, string) error { return domain.ErrUnsupported }

type privacyCredentials struct{ token string }

func (c privacyCredentials) Get(_ context.Context, connectionID, field string) (string, error) {
	if connectionID != "gl-privacy" || field != "token" || c.token == "" {
		return "", domain.ErrNotFound
	}
	return c.token, nil
}

type privacyLocker struct{}

func (privacyLocker) TryLock(context.Context, string) (func() error, error) {
	return func() error { return nil }, nil
}
