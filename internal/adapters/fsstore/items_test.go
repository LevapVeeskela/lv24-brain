package fsstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var itemClock = time.Date(2026, 11, 9, 12, 0, 0, 123456789, time.UTC)

func testItem(connectionID, externalID, title string) domain.Item {
	return domain.Item{
		Ref:        domain.ItemRef{ConnectionID: connectionID, ExternalID: externalID},
		Kind:       domain.ItemIssue,
		Title:      title,
		Body:       "First line\r\n\r\n- bullet\n",
		URL:        "https://example.test/" + externalID,
		Author:     "alice",
		Labels:     []string{"bug", "p1"},
		ACL:        []string{"public"},
		CreatedAt:  itemClock.Add(-time.Hour),
		UpdatedAt:  itemClock,
		ObservedAt: itemClock,
		ProjectID:  "demo",
	}.WithHash()
}

func itemFilePath(store *Store, item domain.Item) string {
	_, hexPart, _ := strings.Cut(item.Key(), "/")
	return filepath.Join(store.root, connectorsDir, item.Ref.ConnectionID, itemsDir, hexPart+itemFileExt)
}

func TestItemUpsertCreatesFileUnderKeyPath(t *testing.T) {
	// Arrange
	store := newTestStore(t)
	items := store.Items()
	item := testItem("gh-acme", "issue/1", "Flaky build")

	// Act
	outcome, err := items.Upsert(context.Background(), item)

	// Assert
	if err != nil || outcome != ports.UpsertCreated {
		t.Fatalf("Upsert = %q, %v", outcome, err)
	}
	raw, err := os.ReadFile(itemFilePath(store, item))
	if err != nil {
		t.Fatalf("item file: %v", err)
	}
	text := string(raw)
	for _, want := range []string{`connection_id: "gh-acme"`, `external_id: "issue/1"`, `kind: "issue"`, `hash: "sha256:`, "deleted: false", "\n- bullet\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("file lacks %q:\n%s", want, text)
		}
	}
	got, err := items.Get(context.Background(), item.Ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Hash != item.Hash || got.ComputeHash() != item.Hash || !got.ObservedAt.Equal(itemClock) || got.ProjectID != "demo" {
		t.Fatalf("Get = %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("stored item does not validate: %v", err)
	}
}

func TestItemUpsertUnchangedDoesNotRewrite(t *testing.T) {
	// Arrange
	store := newTestStore(t)
	items := store.Items()
	item := testItem("gh-acme", "issue/1", "Flaky build")
	if _, err := items.Upsert(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	path := itemFilePath(store, item)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	replay := item
	replay.ObservedAt = itemClock.Add(time.Hour)

	// Act
	outcome, err := items.Upsert(context.Background(), replay)

	// Assert
	if err != nil || outcome != ports.UpsertUnchanged {
		t.Fatalf("Upsert = %q, %v", outcome, err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("file was rewritten: %v %v", info.ModTime(), err)
	}
}

func TestItemUpdateDeleteRevive(t *testing.T) {
	// Arrange
	ctx := context.Background()
	items := newTestStore(t).Items()
	item := testItem("gh-acme", "issue/1", "Flaky build")
	if _, err := items.Upsert(ctx, item); err != nil {
		t.Fatal(err)
	}
	changed := item
	changed.Title = "Flaky build on Windows"
	changed = changed.WithHash()

	// Act
	updated, updateErr := items.Upsert(ctx, changed)
	existed, deleteErr := items.Delete(ctx, item.Ref, itemClock.Add(time.Minute))
	again, againErr := items.Delete(ctx, item.Ref, itemClock.Add(2*time.Minute))
	tombstone, getErr := items.Get(ctx, item.Ref)
	revived, reviveErr := items.Upsert(ctx, changed)

	// Assert
	if updateErr != nil || updated != ports.UpsertUpdated {
		t.Fatalf("update = %q, %v", updated, updateErr)
	}
	if deleteErr != nil || !existed {
		t.Fatalf("delete = %v, %v", existed, deleteErr)
	}
	if againErr != nil || again {
		t.Fatalf("second delete = %v, %v", again, againErr)
	}
	if getErr != nil || !tombstone.Deleted || tombstone.Body != "" || tombstone.Title != "" || !tombstone.ObservedAt.Equal(itemClock.Add(time.Minute)) {
		t.Fatalf("tombstone = %+v, %v", tombstone, getErr)
	}
	if reviveErr != nil || revived != ports.UpsertCreated {
		t.Fatalf("revive = %q, %v", revived, reviveErr)
	}
}

func TestItemDeleteUnknownRecordsTombstone(t *testing.T) {
	// Arrange
	ctx := context.Background()
	items := newTestStore(t).Items()
	ref := domain.ItemRef{ConnectionID: "gh-acme", ExternalID: "gone"}

	// Act
	existed, err := items.Delete(ctx, ref, itemClock)

	// Assert
	if err != nil || existed {
		t.Fatalf("Delete = %v, %v", existed, err)
	}
	got, err := items.Get(ctx, ref)
	if err != nil || !got.Deleted {
		t.Fatalf("Get = %+v, %v", got, err)
	}
}

func TestItemGetUnknownIsNotFound(t *testing.T) {
	items := newTestStore(t).Items()

	_, err := items.Get(context.Background(), domain.ItemRef{ConnectionID: "gh-acme", ExternalID: "x"})

	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestItemApplyOrderAndReplayIdempotent(t *testing.T) {
	// Arrange
	ctx := context.Background()
	items := newTestStore(t).Items()
	first := testItem("gh-acme", "issue/1", "One")
	second := testItem("gh-acme", "issue/2", "Two")
	secondEdited := second
	secondEdited.Title = "Two edited"
	secondEdited = secondEdited.WithHash()
	firstGone := domain.Item{Ref: first.Ref, Deleted: true, ObservedAt: itemClock.Add(time.Minute)}
	batch := []domain.Item{first, second, secondEdited, firstGone}

	// Act
	outcomes, err := items.Apply(ctx, batch)
	replay, replayErr := items.Apply(ctx, batch[2:])

	// Assert
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []ports.UpsertOutcome{ports.UpsertCreated, ports.UpsertCreated, ports.UpsertUpdated, ports.UpsertUpdated}
	if fmt.Sprint(outcomes) != fmt.Sprint(want) {
		t.Fatalf("outcomes = %v, want %v", outcomes, want)
	}
	if replayErr != nil || fmt.Sprint(replay) != fmt.Sprint([]ports.UpsertOutcome{ports.UpsertUnchanged, ports.UpsertUnchanged}) {
		t.Fatalf("replay = %v, %v", replay, replayErr)
	}
	got, err := items.Get(ctx, second.Ref)
	if err != nil || got.Title != "Two edited" {
		t.Fatalf("second = %+v, %v", got, err)
	}
	gone, err := items.Get(ctx, first.Ref)
	if err != nil || !gone.Deleted {
		t.Fatalf("first = %+v, %v", gone, err)
	}
}

func TestItemApplyValidatesBeforeWriting(t *testing.T) {
	// Arrange
	ctx := context.Background()
	items := newTestStore(t).Items()
	good := testItem("gh-acme", "issue/1", "One")
	bad := testItem("gh-acme", "issue/2", "Two")
	bad.Title = "tampered"

	// Act
	_, err := items.Apply(ctx, []domain.Item{good, bad})

	// Assert
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Apply err = %v", err)
	}
	if _, err := items.Get(ctx, good.Ref); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("good item was written before validation failed: %v", err)
	}
}

func TestItemListPagesStablyByKey(t *testing.T) {
	// Arrange
	ctx := context.Background()
	items := newTestStore(t).Items()
	for index := 0; index < 7; index++ {
		if _, err := items.Upsert(ctx, testItem("gh-acme", fmt.Sprintf("issue/%d", index), "Issue")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := items.Delete(ctx, domain.ItemRef{ConnectionID: "gh-acme", ExternalID: "issue/3"}, itemClock); err != nil {
		t.Fatal(err)
	}
	if _, err := items.Upsert(ctx, testItem("other", "issue/1", "Other connection")); err != nil {
		t.Fatal(err)
	}

	// Act
	var keys []string
	cursor := ""
	pages := 0
	for {
		page, err := items.List(ctx, "gh-acme", ports.ItemListOptions{Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		for _, item := range page.Items {
			keys = append(keys, item.Key())
		}
		if page.NextCursor == "" {
			break
		}
		if pages == 2 {
			// A write between pages must not repeat or skip existing items.
			if _, err := items.Upsert(ctx, testItem("gh-acme", "issue/late", "Late")); err != nil {
				t.Fatal(err)
			}
		}
		cursor = page.NextCursor
	}
	all, err := items.List(ctx, "gh-acme", ports.ItemListOptions{IncludeDeleted: true})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for index, key := range keys {
		if seen[key] || (index > 0 && keys[index-1] >= key) || !strings.HasPrefix(key, "gh-acme/") {
			t.Fatalf("keys not strictly ordered or repeated: %v", keys)
		}
		seen[key] = true
	}
	if len(all.Items) < 7 || len(all.Items) > 8 || len(keys) < 6 || len(keys) > 7 {
		t.Fatalf("live keys %d, all %d", len(keys), len(all.Items))
	}
	for _, item := range all.Items {
		if !seen[item.Key()] && !item.Deleted && item.Ref.ExternalID != "issue/late" {
			t.Fatalf("live item %s was skipped", item.Key())
		}
	}
	if _, err := items.List(ctx, "other", ports.ItemListOptions{Cursor: encodeItemCursor(keys[0])}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("foreign cursor err = %v", err)
	}
}

func TestItemPurgeRemovesConnectionItemsOnly(t *testing.T) {
	// Arrange
	ctx := context.Background()
	store := newTestStore(t)
	items := store.Items()
	kept := testItem("keep", "issue/1", "Keep")
	for _, item := range []domain.Item{testItem("drop", "issue/1", "Drop"), kept} {
		if _, err := items.Upsert(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Checkpoints().Put(ctx, "drop", domain.Checkpoint{Stream: "issues", Version: 1, Data: []byte("c")}); err != nil {
		t.Fatal(err)
	}

	// Act
	err := items.Purge(ctx, "drop")
	again := items.Purge(ctx, "drop")

	// Assert
	if err != nil || again != nil {
		t.Fatalf("Purge = %v, %v", err, again)
	}
	page, err := items.List(ctx, "drop", ports.ItemListOptions{IncludeDeleted: true})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("purged list = %+v, %v", page, err)
	}
	if _, err := items.Get(ctx, kept.Ref); err != nil {
		t.Fatalf("other connection lost its item: %v", err)
	}
	if _, err := store.Checkpoints().Get(ctx, "drop", "issues"); err != nil {
		t.Fatalf("purge removed checkpoints: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(store.root, connectorsDir, "drop"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), tempFilePrefix) {
			t.Fatalf("purge left %s", entry.Name())
		}
	}
	ids, err := items.Connections(ctx)
	if err != nil || fmt.Sprint(ids) != "[keep]" {
		t.Fatalf("Connections = %v, %v", ids, err)
	}
}

func TestItemPathContainment(t *testing.T) {
	ctx := context.Background()
	items := newTestStore(t).Items()
	for _, connectionID := range []string{"../escape", "runs", "Upper", "con", ""} {
		if _, err := items.List(ctx, connectionID, ports.ItemListOptions{}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("List(%q) err = %v", connectionID, err)
		}
	}
	for _, key := range []string{"gh/../../x", "gh/" + strings.Repeat("A", 40), "../x/" + strings.Repeat("a", 40), "gh"} {
		if _, err := items.GetByKey(ctx, key); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("GetByKey(%q) err = %v", key, err)
		}
	}
}

func TestItemFrontmatterSurvivesHostileValues(t *testing.T) {
	// Arrange
	ctx := context.Background()
	items := newTestStore(t).Items()
	item := testItem("gh-acme", "id: x\n---\n", "Title\n---\nnot frontmatter")
	item.Author = "---"
	item.Body = "---\nbody: also not frontmatter\n---\n"
	item = item.WithHash()

	// Act
	_, err := items.Upsert(ctx, item)
	got, getErr := items.Get(ctx, item.Ref)

	// Assert
	if err != nil || getErr != nil {
		t.Fatalf("Upsert %v, Get %v", err, getErr)
	}
	if got.Title != item.Title || got.Author != "---" || got.Ref != item.Ref || got.ComputeHash() != item.Hash {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestItemListSkipsDamagedFiles(t *testing.T) {
	// Arrange
	ctx := context.Background()
	store := newTestStore(t)
	items := store.Items()
	good := testItem("gh-acme", "issue/1", "Good")
	if _, err := items.Upsert(ctx, good); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(store.root, connectorsDir, "gh-acme", itemsDir)
	if err := os.WriteFile(filepath.Join(dir, strings.Repeat("0", 40)+".md"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("---\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Act
	page, err := items.List(ctx, "gh-acme", ports.ItemListOptions{})

	// Assert
	if err != nil || len(page.Items) != 1 || page.Items[0].Key() != good.Key() {
		t.Fatalf("List = %+v, %v", page, err)
	}
}

func TestPublishPolicyWritesGitignore(t *testing.T) {
	// Arrange
	ctx := context.Background()
	store := newTestStore(t)
	items := store.Items()
	ignorePath := filepath.Join(store.root, connectorsDir, "mail-corp", connectionIgnoreFile)

	// Act
	setErr := items.SetPublishPolicy(ctx, "mail-corp", domain.PublishLocalOnly)
	_, upsertErr := items.Upsert(ctx, testItem("mail-corp", "<m1@x>", "Hello"))
	local, localErr := items.PublishPolicy(ctx, "mail-corp")

	// Assert
	if setErr != nil || upsertErr != nil || localErr != nil || local != domain.PublishLocalOnly {
		t.Fatalf("set %v upsert %v policy %q %v", setErr, upsertErr, local, localErr)
	}
	raw, err := os.ReadFile(ignorePath)
	if err != nil || !strings.Contains(string(raw), "\n*\n") {
		t.Fatalf(".gitignore = %q, %v", raw, err)
	}
	if err := items.SetPublishPolicy(ctx, "mail-corp", domain.PublishShared); err != nil {
		t.Fatal(err)
	}
	shared, _ := items.PublishPolicy(ctx, "mail-corp")
	raw, _ = os.ReadFile(ignorePath)
	if shared != domain.PublishShared || strings.Contains(string(raw), "\n*\n") || !strings.Contains(string(raw), "*.lock") {
		t.Fatalf("shared policy = %q, .gitignore = %q", shared, raw)
	}
	if err := items.SetPublishPolicy(ctx, "mail-corp", "public"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("bad policy err = %v", err)
	}
}

func TestFirstWriteAddsSharedGitignoreWithoutOverridingLocalOnly(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	items := store.Items()
	if _, err := items.Upsert(ctx, testItem("gh-acme", "issue/1", "One")); err != nil {
		t.Fatal(err)
	}
	if policy, _ := items.PublishPolicy(ctx, "gh-acme"); policy != domain.PublishShared {
		t.Fatalf("default policy = %q", policy)
	}
	if err := items.SetPublishPolicy(ctx, "gh-acme", domain.PublishLocalOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := items.Upsert(ctx, testItem("gh-acme", "issue/2", "Two")); err != nil {
		t.Fatal(err)
	}
	if policy, _ := items.PublishPolicy(ctx, "gh-acme"); policy != domain.PublishLocalOnly {
		t.Fatalf("write reset local-only policy to %q", policy)
	}
}

func TestItemsDoNotChangeCuratedRevision(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	before, err := store.CurrentRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Items().Upsert(ctx, testItem("gh-acme", "issue/1", "One")); err != nil {
		t.Fatal(err)
	}
	after, err := store.CurrentRevision(ctx)
	if err != nil || after.Value != before.Value {
		t.Fatalf("revision changed: %s -> %s (%v)", before.Value, after.Value, err)
	}
}
