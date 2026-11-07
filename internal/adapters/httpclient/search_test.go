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

func TestSearchSendsKindFilters(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if got := query["kind"]; len(got) != 2 || got[0] != "note" || got[1] != "decision" {
			t.Errorf("kind = %v, want [note decision]", got)
		}
		if got := query["exclude_kind"]; len(got) != 1 || got[0] != "dump" {
			t.Errorf("exclude_kind = %v, want [dump]", got)
		}
		writeJSON(t, w, http.StatusOK, `{"items":[]}`)
	}))

	_, err := client.Search(context.Background(), ports.Query{
		Text:         "term",
		Kinds:        []string{" note ", "", "decision"},
		ExcludeKinds: []string{"dump"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
}

func TestSearchOmitsEmptyKindFilters(t *testing.T) {
	client := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if query := r.URL.Query(); query.Has("kind") || query.Has("exclude_kind") {
			t.Errorf("unexpected kind parameters: %s", r.URL.RawQuery)
		}
		writeJSON(t, w, http.StatusOK, `{"items":[]}`)
	}))
	if _, err := client.Search(context.Background(), ports.Query{Text: "term"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
}
