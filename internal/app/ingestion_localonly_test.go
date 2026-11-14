package app

import (
	"context"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// localOnlyItemBatch is one mail item of a connection whose raw-zone policy
// resolves to local-only (no explicit publish=shared).
func localOnlyItemBatch(connectionID string) []ports.ReadBatch {
	return []ports.ReadBatch{{Items: []domain.Item{{
		Ref:    domain.ItemRef{ConnectionID: connectionID, ExternalID: "m-1"},
		Kind:   domain.ItemMessage,
		Title:  "subject",
		Body:   "private mail body",
		Author: "alice@example.test",
	}}}}
}

// TestServerHostDropsLocalOnlyItems is the WB-0.8 storage guarantee: a host
// whose only item store is shared (server mode) never persists items of a
// local-only connection. The run still succeeds, checkpoints and reports the
// dropped items as skips.
func TestServerHostDropsLocalOnlyItems(t *testing.T) {
	connection := domain.Connection{ID: "mail-work", Provider: "sample"}
	source := &sequenceSource{descriptor: ingestionDescriptor(false), batches: localOnlyItemBatch(connection.ID)}
	store := &recordingItemStore{}
	checkpoints := &recordingCheckpointStore{}
	application := ingestionFixture(t, connection, source, store, checkpoints, nil)
	application.DisallowLocalOnlyItems = true

	result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
	if err != nil {
		t.Fatalf("local-only run failed: %v", err)
	}
	if result.Publish != domain.PublishLocalOnly {
		t.Fatalf("publish = %q, want local-only", result.Publish)
	}
	if len(store.items) != 0 {
		t.Fatalf("shared item store received local-only items: %+v", store.items)
	}
	if result.Run.Stats == nil || result.Run.Stats.Skipped != 1 || result.Run.Stats.Upserted != 0 {
		t.Fatalf("stats = %+v", result.Run.Stats)
	}
	if err := result.Run.Stats.Validate(); err != nil {
		t.Fatalf("stats invalid: %v", err)
	}
	if len(result.Skips) != 1 || !strings.Contains(result.Skips[0].Reason, "local-only") {
		t.Fatalf("skips = %+v", result.Skips)
	}
	if result.Run.Status != domain.ConnectorSuccess {
		t.Fatalf("status = %s", result.Run.Status)
	}
}

// TestLocalHostKeepsLocalOnlyItems keeps local/git behavior unchanged: the
// machine-local raw zone still stores local-only items.
func TestLocalHostKeepsLocalOnlyItems(t *testing.T) {
	connection := domain.Connection{ID: "mail-work", Provider: "sample"}
	source := &sequenceSource{descriptor: ingestionDescriptor(false), batches: localOnlyItemBatch(connection.ID)}
	store := &recordingItemStore{}
	application := ingestionFixture(t, connection, source, store, &recordingCheckpointStore{}, nil)

	result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Publish != domain.PublishLocalOnly || len(store.items) != 1 {
		t.Fatalf("publish = %q stored = %d", result.Publish, len(store.items))
	}
}

// TestSharedHostStillStoresExplicitlySharedItems proves the flag is scoped to
// local-only: an explicitly shared connection keeps its items on the server.
func TestSharedHostStillStoresExplicitlySharedItems(t *testing.T) {
	connection := domain.Connection{ID: "gh-work", Provider: "sample", Publish: domain.PublishShared}
	source := &sequenceSource{descriptor: ingestionDescriptor(false), batches: []ports.ReadBatch{{Items: []domain.Item{{
		Ref: domain.ItemRef{ConnectionID: connection.ID, ExternalID: "1"}, Kind: domain.ItemIssue, Title: "issue",
	}}}}}
	store := &recordingItemStore{}
	application := ingestionFixture(t, connection, source, store, &recordingCheckpointStore{}, nil)
	application.DisallowLocalOnlyItems = true

	result, err := application.RunConnection(context.Background(), RunConnectionOptions{ConnectionID: connection.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Publish != domain.PublishShared || len(store.items) != 1 {
		t.Fatalf("publish = %q stored = %d", result.Publish, len(store.items))
	}
}
