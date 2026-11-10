package fsstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestItemStatePersistsAndChangesTheHash(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	item := testItem("gh-acme", "issue/7", "Closed issue")
	item.State = domain.ItemStateClosed
	item = item.WithHash()

	outcome, err := store.Items().Upsert(ctx, item)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if outcome != ports.UpsertCreated {
		t.Fatalf("outcome = %q", outcome)
	}
	got, err := store.Items().Get(ctx, item.Ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != domain.ItemStateClosed {
		t.Fatalf("stored state = %q", got.State)
	}
	if got.Hash != item.Hash {
		t.Fatalf("stored hash = %s, want %s", got.Hash, item.Hash)
	}
	raw, err := os.ReadFile(itemFilePath(store, item))
	if err != nil {
		t.Fatalf("read item file: %v", err)
	}
	if !strings.Contains(string(raw), `state:`) || !strings.Contains(string(raw), `closed`) {
		t.Fatalf("item frontmatter lacks state:\n%s", raw)
	}

	outcome, err = store.Items().Upsert(ctx, item)
	if err != nil || outcome != ports.UpsertUnchanged {
		t.Fatalf("idempotent Upsert = %q, %v", outcome, err)
	}
	changed := item
	changed.State = domain.ItemStateMerged
	changed = changed.WithHash()
	outcome, err = store.Items().Upsert(ctx, changed)
	if err != nil || outcome != ports.UpsertUpdated {
		t.Fatalf("state-change Upsert = %q, %v", outcome, err)
	}
	got, err = store.Items().Get(ctx, item.Ref)
	if err != nil || got.State != domain.ItemStateMerged {
		t.Fatalf("updated item = %+v, %v", got, err)
	}
}

// TestReadItemRejectsUnknownState: a hand-edited state must be skipped like
// any other damaged item instead of being returned as valid metadata.
func TestReadItemRejectsUnknownState(t *testing.T) {
	item := testItem("gh-acme", "issue/7", "Closed issue")
	item.State = "bogus"
	item = item.WithHash()
	raw, err := encodeItem(item)
	if err != nil {
		t.Fatalf("encode item: %v", err)
	}
	path := filepath.Join(t.TempDir(), "item.md")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write item: %v", err)
	}
	if _, err := readItemFile(path, item.Key()); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("readItemFile error = %v, want validation error", err)
	}
}
