package httpclient

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestSearchCallsServerEndpoint(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/search" {
			t.Errorf("request = %s %s, want GET /v1/search", r.Method, r.URL.Path)
		}
		requireAuth(t, r)
		query := r.URL.Query()
		if got := query.Get("q"); got != "release notes" {
			t.Errorf("q = %q", got)
		}
		if got := query.Get("projectId"); got != "demo" {
			t.Errorf("projectId = %q", got)
		}
		if got := query.Get("limit"); got != "7" {
			t.Errorf("limit = %q", got)
		}
		writeJSON(t, w, http.StatusOK, `{"items":[{"ID":"note-1","ProjectID":"demo","Kind":"note","Title":"Release","Snippet":"ship it","Score":1.25}]}`)
	}))

	hits, err := client.Search(context.Background(), ports.Query{Text: "  release notes ", ProjectID: "demo", Limit: 7})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []ports.Hit{{ID: "note-1", ProjectID: "demo", Kind: "note", Title: "Release", Snippet: "ship it", Score: 1.25}}
	if len(hits) != len(want) || hits[0] != want[0] {
		t.Fatalf("hits = %+v, want %+v", hits, want)
	}
}

func TestSearchOmitsEmptyOptionalParameters(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Has("projectId") || query.Has("limit") {
			t.Errorf("unexpected optional parameters: %s", r.URL.RawQuery)
		}
		if got := query.Get("q"); got != "term" {
			t.Errorf("q = %q", got)
		}
		writeJSON(t, w, http.StatusOK, `{"items":[]}`)
	}))

	hits, err := client.Search(context.Background(), ports.Query{Text: "term"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if hits == nil || len(hits) != 0 {
		t.Fatalf("hits = %#v, want empty non-nil slice", hits)
	}
}

func TestSearchRejectsEmptyText(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server must not be called for empty text")
	}))
	for _, text := range []string{"", "   "} {
		if _, err := client.Search(context.Background(), ports.Query{Text: text}); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Search(%q) error = %v, want validation", text, err)
		}
	}
}

func TestSearchMapsServerErrors(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"token rejected"}}`)
	}))
	if _, err := client.Search(context.Background(), ports.Query{Text: "term"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, want ErrUnauthorized", err)
	}
}
