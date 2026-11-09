package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var itemIndexClock = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

func rawItem(connectionID, externalID, title, body string) domain.Item {
	return domain.Item{
		Ref:        domain.ItemRef{ConnectionID: connectionID, ExternalID: externalID},
		Kind:       domain.ItemIssue,
		Title:      title,
		Body:       body,
		URL:        "https://tracker.test/" + externalID,
		Author:     "zebediah",
		Labels:     []string{"quokka-label"},
		UpdatedAt:  itemIndexClock,
		ObservedAt: itemIndexClock,
		ProjectID:  "p1",
	}.WithHash()
}

func itemHits(t *testing.T, application *app.App, text string, opts app.SearchOptions) []ports.Hit {
	t.Helper()
	opts.Text = text
	hits, err := application.SearchWith(context.Background(), opts)
	if err != nil {
		t.Fatalf("search %q: %v", text, err)
	}
	return hits
}

func TestItemsAreIndexedButExcludedFromCuratedSearch(t *testing.T) {
	// Arrange
	ctx := context.Background()
	dataDir := t.TempDir()
	registerScopeProjects(t, dataDir)
	application, _ := newScopeFixture(t, dataDir)
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	item := rawItem("tracker", "ISSUE-1", "Wombat outage", "The wombat service fell over")
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatal(err)
	}

	// Act
	result, err := application.Reindex(ctx, app.IndexOptions{})

	// Assert
	if err != nil || result.Items != 1 {
		t.Fatalf("Reindex = %+v, %v", result, err)
	}
	if hits := itemHits(t, application, "wombat", app.SearchOptions{}); len(hits) != 0 {
		t.Fatalf("curated search returned items: %+v", hits)
	}
	for _, opts := range []app.SearchOptions{{IncludeDumps: true}, {Kinds: []string{app.KindItem}}} {
		hits := itemHits(t, application, "wombat", opts)
		if len(hits) != 1 || hits[0].ID != "item:"+item.Key() || hits[0].Kind != app.KindItem || hits[0].ProjectID != "p1" {
			t.Fatalf("search %+v = %+v", opts, hits)
		}
	}
	for _, term := range []string{"zebediah", "quokka", "tracker"} {
		if hits := itemHits(t, application, term, app.SearchOptions{IncludeDumps: true}); len(hits) != 1 {
			t.Fatalf("term %q not indexed: %+v", term, hits)
		}
	}
}

func TestIndexItemsIncrementalUpdateDeleteAndPartialRead(t *testing.T) {
	// Arrange
	ctx := context.Background()
	dataDir := t.TempDir()
	registerScopeProjects(t, dataDir)
	application, _ := newScopeFixture(t, dataDir)
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	items := store.Items()
	older := rawItem("tracker", "ISSUE-1", "Platypus backlog", "old item")
	newer := rawItem("tracker", "ISSUE-2", "Echidna bug", "first version")
	for _, item := range []domain.Item{older, newer} {
		if _, err := items.Upsert(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := application.IndexItems(ctx, "tracker", []string{older.Key(), newer.Key()}); err != nil {
		t.Fatal(err)
	}
	edited := newer
	edited.Body = "second version mentions narwhal"
	edited = edited.WithHash()

	// Act: a --since style run only re-reads ISSUE-2 and indexes its key.
	if _, err := items.Upsert(ctx, edited); err != nil {
		t.Fatal(err)
	}
	partial, partialErr := application.IndexItems(ctx, "tracker", []string{edited.Key()})

	// Assert: the update is searchable and the older item is still there.
	if partialErr != nil || partial.Indexed != 1 || partial.Removed != 0 {
		t.Fatalf("partial = %+v, %v", partial, partialErr)
	}
	if hits := itemHits(t, application, "narwhal", app.SearchOptions{IncludeDumps: true}); len(hits) != 1 {
		t.Fatalf("update not indexed: %+v", hits)
	}
	if hits := itemHits(t, application, "platypus", app.SearchOptions{IncludeDumps: true}); len(hits) != 1 {
		t.Fatalf("partial read dropped an older item: %+v", hits)
	}

	// Act: a tombstone removes the document.
	if _, err := items.Delete(ctx, older.Ref, itemIndexClock.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	removed, removeErr := application.IndexItems(ctx, "tracker", []string{older.Key()})

	// Assert
	if removeErr != nil || removed.Removed != 1 {
		t.Fatalf("remove = %+v, %v", removed, removeErr)
	}
	if hits := itemHits(t, application, "platypus", app.SearchOptions{IncludeDumps: true}); len(hits) != 0 {
		t.Fatalf("tombstoned item still searchable: %+v", hits)
	}
	if _, err := application.IndexItems(ctx, "other", []string{edited.Key()}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("foreign key err = %v", err)
	}
}

func TestIndexItemsWholeConnectionAndReindexPrunePurgedItems(t *testing.T) {
	// Arrange
	ctx := context.Background()
	dataDir := t.TempDir()
	registerScopeProjects(t, dataDir)
	application, _ := newScopeFixture(t, dataDir)
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	items := store.Items()
	dropped := rawItem("dropme", "A", "Axolotl note", "body")
	kept := rawItem("keepme", "B", "Axolotl kept", "body")
	unmapped := rawItem("keepme", "C", "Axolotl unmapped", "body")
	unmapped.ProjectID = ""
	unmapped = unmapped.WithHash()
	for _, item := range []domain.Item{dropped, kept, unmapped} {
		if _, err := items.Upsert(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := application.Reindex(ctx, app.IndexOptions{}); err != nil {
		t.Fatal(err)
	}

	// Act
	if err := items.Purge(ctx, "dropme"); err != nil {
		t.Fatal(err)
	}
	whole, wholeErr := application.IndexItems(ctx, "dropme", nil)
	full, fullErr := application.Reindex(ctx, app.IndexOptions{})

	// Assert
	if wholeErr != nil || whole.Removed != 1 || whole.Indexed != 0 {
		t.Fatalf("whole = %+v, %v", whole, wholeErr)
	}
	if fullErr != nil || full.Items != 2 || full.Pruned != 0 {
		t.Fatalf("full = %+v, %v", full, fullErr)
	}
	hits := itemHits(t, application, "axolotl", app.SearchOptions{IncludeDumps: true})
	projects := map[string]bool{}
	for _, hit := range hits {
		projects[hit.ProjectID] = true
	}
	if len(hits) != 2 || !projects["p1"] || !projects["connection:keepme"] {
		t.Fatalf("hits = %+v", hits)
	}

	// Act: removing a file behind the index's back is pruned by a full reindex.
	if _, err := items.Delete(ctx, kept.Ref, itemIndexClock); err != nil {
		t.Fatal(err)
	}
	again, err := application.Reindex(ctx, app.IndexOptions{})
	if err != nil || again.Items != 1 || again.Pruned != 1 {
		t.Fatalf("reindex after tombstone = %+v, %v", again, err)
	}
}
