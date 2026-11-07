package app_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// fakeSearcher records the query it receives and returns canned hits.
type fakeSearcher struct {
	calls int
	query ports.Query
	hits  []ports.Hit
	err   error
}

func (f *fakeSearcher) Search(_ context.Context, query ports.Query) ([]ports.Hit, error) {
	f.calls++
	f.query = query
	return f.hits, f.err
}

func serverModeApp(searcher app.Searcher) *app.App {
	cfg := config.Default()
	cfg.Mode = config.ModeServer
	cfg.ServerURL = "https://brain.example.test"
	return app.New(cfg, app.Dependencies{Searcher: searcher})
}

func TestServerModeSearchDelegatesToSearcher(t *testing.T) {
	fake := &fakeSearcher{hits: []ports.Hit{{ID: "n1", ProjectID: "demo", Kind: "note", Title: "T"}}}
	uc := serverModeApp(fake)

	hits, err := uc.Search(context.Background(), "  release ", " demo ", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("searcher calls = %d, want 1", fake.calls)
	}
	want := ports.Query{Text: "release", ProjectID: "demo", Limit: app.DefaultSearchLimit, ExcludeKinds: []string{app.KindDump}}
	if !reflect.DeepEqual(fake.query, want) {
		t.Fatalf("query = %+v, want %+v", fake.query, want)
	}
	if len(hits) != 1 || hits[0].ID != "n1" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestServerModeSearchValidatesBeforeCalling(t *testing.T) {
	fake := &fakeSearcher{}
	uc := serverModeApp(fake)
	if _, err := uc.Search(context.Background(), "   ", "", 5); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
	if fake.calls != 0 {
		t.Fatalf("searcher calls = %d, want 0", fake.calls)
	}
}

func TestServerModeSearchWithoutSearcherIsUnsupported(t *testing.T) {
	uc := serverModeApp(nil)
	if _, err := uc.Search(context.Background(), "term", "", 5); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("error = %v, want unsupported", err)
	}
}

func TestServerModeIndexStaysLocalOnly(t *testing.T) {
	fake := &fakeSearcher{}
	uc := serverModeApp(fake)
	if _, err := uc.Reindex(context.Background(), app.IndexOptions{}); !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("Reindex error = %v, want unsupported", err)
	}
	if fake.calls != 0 {
		t.Fatalf("searcher calls = %d, want 0", fake.calls)
	}
}
