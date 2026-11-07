package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// seedSearchKinds stores one note, decision and handoff that all match the
// word "rollout" in the in-memory store.
func seedSearchKinds(t *testing.T, store *MemoryStore) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := store.CreateProject(ctx, domain.Project{ID: "demo", Name: "Demo", Kind: domain.ProjectKindApp}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, _, err := store.CreateNote(ctx, domain.Note{ProjectID: "demo", Title: "Rollout", Body: "rollout notes"}); err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if _, _, err := store.CreateDecision(ctx, domain.Decision{ProjectID: "demo", Title: "Rollout plan", Context: "staged"}); err != nil {
		t.Fatalf("CreateDecision: %v", err)
	}
	if _, _, err := store.CreateHandoff(ctx, domain.Handoff{ProjectID: "demo", Summary: "rollout pending"}); err != nil {
		t.Fatalf("CreateHandoff: %v", err)
	}
}

// searchKindsOf decodes a /v1/search response and returns the sorted kinds of
// its hits.
func searchKindsOf(t *testing.T, payload []byte) []string {
	t.Helper()
	var body struct {
		Items []struct {
			Kind string `json:"Kind"`
		} `json:"items"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("decode search response: %v (%s)", err, payload)
	}
	kinds := make([]string, 0, len(body.Items))
	for _, item := range body.Items {
		kinds = append(kinds, item.Kind)
	}
	sort.Strings(kinds)
	return kinds
}

func TestSearchHandlerFiltersByKindParams(t *testing.T) {
	ts, _, store := newTestServer(t, nil)
	seedSearchKinds(t, store)
	token := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"no filter", "q=rollout", []string{"decision", "handoff", "note"}},
		{"single kind", "q=rollout&kind=decision", []string{"decision"}},
		{"repeated kind", "q=rollout&kind=note&kind=handoff", []string{"handoff", "note"}},
		{"exclude kind", "q=rollout&exclude_kind=note", []string{"decision", "handoff"}},
		{"kind then exclude", "q=rollout&kind=note&kind=decision&exclude_kind=note", []string{"decision"}},
		{"blank kind ignored", "q=rollout&kind=%20&kind=note", []string{"note"}},
		{"dump kind is valid and empty", "q=rollout&kind=dump", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/search?"+tc.query, nil, token)
			requireStatus(t, resp, payload, http.StatusOK)
			got := searchKindsOf(t, payload)
			if len(got) != len(tc.want) {
				t.Fatalf("kinds = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("kinds = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSearchHandlerRejectsUnknownKinds(t *testing.T) {
	ts, _, _ := newTestServer(t, nil)
	token := loginAs(t, ts.URL, adminLogin, adminPassword).AccessToken

	for _, query := range []string{
		"q=rollout&kind=notes",
		"q=rollout&kind=note&kind=Decision",
		"q=rollout&exclude_kind=project",
	} {
		resp, payload := jsonRequest(t, http.MethodGet, ts.URL+"/v1/search?"+query, nil, token)
		requireStatus(t, resp, payload, http.StatusBadRequest)
		if got := string(payload); !strings.Contains(got, "invalid_argument") || !strings.Contains(got, "unknown kind") {
			t.Fatalf("%s: error body = %s, want invalid_argument", query, got)
		}
	}
}

func TestMemoryStoreSearchAppliesKindFilters(t *testing.T) {
	store := NewMemoryStore()
	seedSearchKinds(t, store)
	ctx := context.Background()

	cases := []struct {
		name  string
		query ports.Query
		want  []string
	}{
		{"no filter", ports.Query{}, []string{"decision", "handoff", "note"}},
		{"kinds keep one", ports.Query{Kinds: []string{"handoff"}}, []string{"handoff"}},
		{"exclude drops one", ports.Query{ExcludeKinds: []string{"decision"}}, []string{"handoff", "note"}},
		{"kinds then exclude", ports.Query{Kinds: []string{"note", "decision"}, ExcludeKinds: []string{"note"}}, []string{"decision"}},
		{"kind absent from store", ports.Query{Kinds: []string{"dump"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.query.Text = "rollout"
			hits, err := store.Search(ctx, tc.query)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			got := make([]string, 0, len(hits))
			for _, hit := range hits {
				got = append(got, hit.Kind)
			}
			sort.Strings(got)
			if len(got) != len(tc.want) {
				t.Fatalf("kinds = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("kinds = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
