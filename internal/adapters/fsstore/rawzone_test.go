package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestRawZoneIsLocalOnlyByDefault: a fresh connection publishes nothing, and
// its items keep the host observation time because they never leave the
// machine.
func TestRawZoneIsLocalOnlyByDefault(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	item := testItem("gh-acme", "issue/1", "Flaky build")
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.Checkpoints().Put(ctx, "gh-acme", domain.Checkpoint{Stream: "issues", Version: 1, Data: []byte("cursor")}); err != nil {
		t.Fatalf("Put checkpoint: %v", err)
	}

	policy, err := store.Items().PublishPolicy(ctx, "gh-acme")
	if err != nil || policy != domain.PublishLocalOnly {
		t.Fatalf("PublishPolicy = %q, %v", policy, err)
	}
	marker := readRawZoneFile(t, filepath.Join(store.root, connectorsDir, "gh-acme", connectionIgnoreFile))
	if marker != localOnlyIgnore {
		t.Fatalf(".gitignore = %q, want the local-only marker %q", marker, localOnlyIgnore)
	}
	raw := readRawZoneFile(t, itemFilePath(store, item))
	if !strings.Contains(raw, `observed_at:`) {
		t.Fatalf("local item lost its observation time:\n%s", raw)
	}
}

// TestSharedPolicyPublishesItemsButNotCheckpoints: an explicit shared policy
// makes the marker keep items and ignore checkpoints, lock and temporary files.
func TestSharedPolicyPublishesItemsButNotCheckpoints(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.Items().SetPublishPolicy(ctx, "gh-acme", domain.PublishShared); err != nil {
		t.Fatalf("SetPublishPolicy: %v", err)
	}
	item := testItem("gh-acme", "issue/1", "Flaky build")
	if _, err := store.Items().Upsert(ctx, item); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.Checkpoints().Put(ctx, "gh-acme", domain.Checkpoint{Stream: "issues", Version: 1, Data: []byte("cursor")}); err != nil {
		t.Fatalf("Put checkpoint: %v", err)
	}

	policy, err := store.Items().PublishPolicy(ctx, "gh-acme")
	if err != nil || policy != domain.PublishShared {
		t.Fatalf("PublishPolicy = %q, %v", policy, err)
	}
	marker := readRawZoneFile(t, filepath.Join(store.root, connectorsDir, "gh-acme", connectionIgnoreFile))
	for _, want := range []string{"checkpoints/", "*.lock", ".tmp-*"} {
		if !strings.Contains(marker, want) {
			t.Fatalf(".gitignore lacks %q:\n%s", want, marker)
		}
	}
	if strings.Contains(marker, "\n*\n") {
		t.Fatalf("shared marker ignores the whole directory:\n%s", marker)
	}
	raw := readRawZoneFile(t, itemFilePath(store, item))
	if strings.Contains(raw, "observed_at:") {
		t.Fatalf("shared item kept the host observation time:\n%s", raw)
	}
}

// TestSharedItemFilesAreIdenticalAcrossHosts: two machines that read the same
// item write byte-identical files at the same path, so git never sees a
// same-path/different-bytes conflict. Only the host observation time differed.
func TestSharedItemFilesAreIdenticalAcrossHosts(t *testing.T) {
	ctx := context.Background()
	first, second := newTestStore(t), newTestStore(t)
	for _, store := range []*Store{first, second} {
		if err := store.Items().SetPublishPolicy(ctx, "gh-acme", domain.PublishShared); err != nil {
			t.Fatalf("SetPublishPolicy: %v", err)
		}
	}
	item := testItem("gh-acme", "issue/1", "Flaky build")
	if _, err := first.Items().Upsert(ctx, item); err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	replayed := item
	replayed.ObservedAt = item.ObservedAt.Add(90 * time.Minute)
	if _, err := second.Items().Upsert(ctx, replayed); err != nil {
		t.Fatalf("Upsert second: %v", err)
	}

	firstRaw := readRawZoneFile(t, itemFilePath(first, item))
	secondRaw := readRawZoneFile(t, itemFilePath(second, replayed))
	if firstRaw != secondRaw {
		t.Fatalf("shared files differ:\n--- first ---\n%s\n--- second ---\n%s", firstRaw, secondRaw)
	}
	got, err := second.Items().Get(ctx, item.Ref)
	if err != nil || got.Hash != item.Hash {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if !got.ObservedAt.IsZero() {
		t.Fatalf("shared item exposes a host time: %s", got.ObservedAt)
	}
}

// TestSwitchingToSharedRewritesStoredItems: previously local items (with the
// host time) become host-neutral when the connection starts publishing, so the
// first shared sync is not a same-path conflict.
func TestSwitchingToSharedRewritesStoredItems(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	items := store.Items()
	item := testItem("gh-acme", "issue/1", "Flaky build")
	if _, err := items.Upsert(ctx, item); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	gone := domain.ItemRef{ConnectionID: "gh-acme", ExternalID: "issue/2"}
	if _, err := items.Delete(ctx, gone, itemClock); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if raw := readRawZoneFile(t, itemFilePath(store, item)); !strings.Contains(raw, "observed_at:") {
		t.Fatalf("local item lost its observation time before the switch:\n%s", raw)
	}

	if err := items.SetPublishPolicy(ctx, "gh-acme", domain.PublishShared); err != nil {
		t.Fatalf("SetPublishPolicy: %v", err)
	}

	raw := readRawZoneFile(t, itemFilePath(store, item))
	if strings.Contains(raw, "observed_at:") {
		t.Fatalf("switched item kept the host time:\n%s", raw)
	}
	got, err := items.Get(ctx, item.Ref)
	if err != nil || got.Hash != item.Hash || got.Title != item.Title {
		t.Fatalf("Get after switch = %+v, %v", got, err)
	}
	tombstonePath := filepath.Join(store.root, connectorsDir, "gh-acme", itemsDir, itemFileHex(t, gone)+itemFileExt)
	tombstoneRaw := readRawZoneFile(t, tombstonePath)
	if strings.Contains(tombstoneRaw, "observed_at:") || !strings.Contains(tombstoneRaw, "deleted: true") {
		t.Fatalf("switched tombstone = %q", tombstoneRaw)
	}
	if again, err := items.Get(ctx, gone); err != nil || !again.Deleted || !again.ObservedAt.IsZero() {
		t.Fatalf("tombstone after switch = %+v, %v", again, err)
	}
}

// TestLegacySharedMarkerStaysShared: a marker written by an earlier engine
// keeps the connection shared until the next run applies the connection policy,
// so an upgrade does not silently hide already published items.
func TestLegacySharedMarkerStaysShared(t *testing.T) {
	store := newTestStore(t)
	dir, err := store.connectionDir("gh-acme")
	if err != nil {
		t.Fatalf("connectionDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacy := "# lv24-brain: shared connection; only lock and temporary files are ignored.\n*.lock\n.tmp-*\n"
	if err := os.WriteFile(filepath.Join(dir, connectionIgnoreFile), []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy marker: %v", err)
	}
	policy, err := store.Items().PublishPolicy(context.Background(), "gh-acme")
	if err != nil || policy != domain.PublishShared {
		t.Fatalf("legacy policy = %q, %v", policy, err)
	}
}

func readRawZoneFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func itemFileHex(t *testing.T, ref domain.ItemRef) string {
	t.Helper()
	connectionID, hexPart, ok := strings.Cut(ref.Key(), "/")
	if !ok || connectionID != ref.ConnectionID {
		t.Fatalf("unexpected key %q", ref.Key())
	}
	return hexPart
}
