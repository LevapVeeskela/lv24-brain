package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Pagination is a normal result, not a failure: a page that still has hits
// after it prints the cursor hint and exits 0, so a script can tell "there is
// another page" from "the search failed". Only a real error keeps a non-zero
// code (1 runtime, 2 usage).

// searchHintCursor extracts the cursor the "more results" hint offered.
func searchHintCursor(t *testing.T, stderr string) string {
	t.Helper()
	const marker = `pass --cursor "`
	start := strings.Index(stderr, marker)
	if start < 0 {
		t.Fatalf("stderr has no cursor hint:\n%s", stderr)
	}
	rest := stderr[start+len(marker):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("stderr cursor hint is not quoted:\n%s", stderr)
	}
	cursor := rest[:end]
	if cursor == "" {
		t.Fatalf("stderr cursor hint is empty:\n%s", stderr)
	}
	return cursor
}

// searchFixtureNotes writes count notes that all match marker.
func searchFixtureNotes(t *testing.T, cfgPath, marker string, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		createNoteForTest(t, cfgPath, "Pagination "+string(rune('A'+index))+" "+marker, marker+" body")
	}
}

// A page with more results behind it prints the hint and exits 0, and the
// hinted cursor serves the next page with exit 0 as well.
func TestSearchPaginationMoreResultsExitsZero(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	searchFixtureNotes(t, cfgPath, "pageexitmarker", 5)

	code, stdout, stderr := runForTest(t, "search", "pageexitmarker", "--config", cfgPath, "--limit", "3")
	if code != 0 {
		t.Fatalf("more results exited %d, want 0 (stderr: %s)", code, stderr)
	}
	if hits := strings.Count(stdout, "note/demo/"); hits != 3 {
		t.Fatalf("page 1 printed %d hits, want 3:\n%s", hits, stdout)
	}
	if !strings.Contains(stderr, "more results") || !strings.Contains(stderr, "page 2") {
		t.Fatalf("stderr lacks the page-2 hint:\n%s", stderr)
	}

	cursor := searchHintCursor(t, stderr)
	code, second, secondErr := runForTest(t, "search", "pageexitmarker", "--config", cfgPath, "--limit", "3", "--cursor", cursor)
	if code != 0 {
		t.Fatalf("page 2 exited %d, want 0 (stderr: %s)", code, secondErr)
	}
	if second == stdout {
		t.Fatalf("page 2 repeated page 1:\n%s", second)
	}
	if hits := strings.Count(second, "note/demo/"); hits != 2 {
		t.Fatalf("page 2 printed %d hits, want 2:\n%s", hits, second)
	}
	if strings.Contains(secondErr, "more results") {
		t.Fatalf("last page still advertises a next page:\n%s", secondErr)
	}
}

// Walking every page exits 0, including the page that ends the result set.
func TestSearchPaginationLastPageExitsZero(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	searchFixtureNotes(t, cfgPath, "lastexitmarker", 4)

	cursor := ""
	pages := 0
	for page := 1; page <= 4; page++ {
		args := []string{"search", "lastexitmarker", "--config", cfgPath, "--page-size", "2"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		code, _, stderr := runForTest(t, args...)
		if code != 0 {
			t.Fatalf("page %d exited %d, want 0 (stderr: %s)", page, code, stderr)
		}
		pages++
		if !strings.Contains(stderr, "more results") {
			if pages < 2 {
				t.Fatalf("pagination ended after %d page, want at least 2", pages)
			}
			return
		}
		cursor = searchHintCursor(t, stderr)
	}
	t.Fatal("pagination never reached the last page")
}

// A cursor this command did not issue is a usage error: exit 2, not 1.
func TestSearchBadCursorExitsTwo(t *testing.T) {
	cfgPath, _, _ := codexFixture(t)
	searchFixtureNotes(t, cfgPath, "cursorusage", 3)

	code, _, stderr := runForTest(t, "search", "cursorusage", "--config", cfgPath, "--cursor", "not-a-cursor")
	if code != 2 {
		t.Fatalf("bad cursor exited %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "invalid search cursor") {
		t.Fatalf("stderr does not name the bad cursor:\n%s", stderr)
	}
}

// staleIndex is a search backend that fails, to prove a real failure keeps
// exit 1 while pagination stays successful.
type staleIndex struct{ err error }

func (s staleIndex) Upsert(context.Context, ports.Document) error { return nil }

func (s staleIndex) Delete(context.Context, string) error { return nil }

func (s staleIndex) Search(context.Context, ports.Query) ([]ports.Hit, error) {
	return nil, s.err
}

// A backend failure is still a failure: exit 1 and no hits on stdout.
func TestSearchBackendErrorExitsOne(t *testing.T) {
	previous := buildRuntime
	buildRuntime = func(cfg config.BrainConfig) (*bootstrap.Runtime, error) {
		return &bootstrap.Runtime{App: app.New(cfg, app.Dependencies{
			Index: staleIndex{err: errors.New("index is locked by another process")},
		})}, nil
	}
	t.Cleanup(func() { buildRuntime = previous })

	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = t.TempDir()
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	code, stdout, stderr := runForTest(t, "search", "anything", "--config", path)
	if code != 1 {
		t.Fatalf("backend error exited %d, want 1 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "index is locked by another process") {
		t.Fatalf("stderr does not report the backend error:\n%s", stderr)
	}
	if hits := strings.Count(stdout, "note/"); hits != 0 {
		t.Fatalf("a failed search printed %d hits:\n%s", hits, stdout)
	}
}

// A full final page inside the result window must not advertise a cursor the
// command would then refuse: page 100 of a 200-hit window is the last page.
func TestSearchPaginationWindowEdgeHasMore(t *testing.T) {
	hits := make([]ports.Hit, searchMaxWindow)
	items, page := pageSearchHits(hits, searchMaxWindow-2, 2, searchMaxWindow, "filter")
	if len(items) != 2 {
		t.Fatalf("last page items = %d, want 2", len(items))
	}
	if page.HasMore || page.NextCursor != "" {
		t.Fatalf("last page in window: has_more=%v cursor=%q, want false/empty", page.HasMore, page.NextCursor)
	}

	// The page before it still offers a cursor that the decoder accepts.
	items, page = pageSearchHits(hits, searchMaxWindow-4, 2, searchMaxWindow, "filter")
	if len(items) != 2 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("penultimate page: items=%d has_more=%v cursor=%q", len(items), page.HasMore, page.NextCursor)
	}
	offset, err := decodeSearchCursor(page.NextCursor, "filter")
	if err != nil {
		t.Fatalf("hinted cursor is refused: %v", err)
	}
	if offset != searchMaxWindow-2 {
		t.Fatalf("hinted cursor offset = %d, want %d", offset, searchMaxWindow-2)
	}
}
