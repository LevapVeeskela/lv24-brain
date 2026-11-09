package serverstore

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var serverItemClock = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func serverItem(connectionID, externalID, title, projectID string) domain.Item {
	return domain.Item{
		Ref:        domain.ItemRef{ConnectionID: connectionID, ExternalID: externalID},
		Kind:       domain.ItemIssue,
		Title:      title,
		Body:       "body of " + title,
		URL:        "https://tracker.test/" + externalID,
		Author:     "alice",
		Labels:     []string{"bug"},
		ACL:        []string{"public"},
		CreatedAt:  serverItemClock.Add(-time.Hour),
		UpdatedAt:  serverItemClock,
		ObservedAt: serverItemClock,
		ProjectID:  projectID,
	}.WithHash()
}

func TestServerItemUpsertUnchangedUpdateDeleteRevive(t *testing.T) {
	// Arrange
	store := newTestStore(t)
	ctx := testContext(t)
	items := store.Items()
	item := serverItem("gh-acme", "issue/1", "Flaky", "")
	edited := item
	edited.Title = "Flaky on Windows"
	edited = edited.WithHash()

	// Act
	created, createErr := items.Upsert(ctx, item)
	unchanged, unchangedErr := items.Upsert(ctx, item)
	updated, updateErr := items.Upsert(ctx, edited)
	existed, deleteErr := items.Delete(ctx, item.Ref, serverItemClock.Add(time.Minute))
	again, againErr := items.Delete(ctx, item.Ref, serverItemClock.Add(2*time.Minute))
	tombstone, tombstoneErr := items.Get(ctx, item.Ref)
	revived, reviveErr := items.Upsert(ctx, edited)
	got, getErr := items.Get(ctx, item.Ref)

	// Assert
	if createErr != nil || created != ports.UpsertCreated || unchangedErr != nil || unchanged != ports.UpsertUnchanged ||
		updateErr != nil || updated != ports.UpsertUpdated {
		t.Fatalf("create %q %v, unchanged %q %v, update %q %v", created, createErr, unchanged, unchangedErr, updated, updateErr)
	}
	if deleteErr != nil || !existed || againErr != nil || again {
		t.Fatalf("delete %v %v, again %v %v", existed, deleteErr, again, againErr)
	}
	if tombstoneErr != nil || !tombstone.Deleted || tombstone.Body != "" || !tombstone.ObservedAt.Equal(serverItemClock.Add(time.Minute)) {
		t.Fatalf("tombstone = %+v, %v", tombstone, tombstoneErr)
	}
	if reviveErr != nil || revived != ports.UpsertCreated {
		t.Fatalf("revive = %q, %v", revived, reviveErr)
	}
	if getErr != nil || got.Title != edited.Title || got.Hash != edited.Hash || got.Validate() != nil {
		t.Fatalf("Get = %+v, %v", got, getErr)
	}
	if _, err := items.Get(ctx, domain.ItemRef{ConnectionID: "gh-acme", ExternalID: "missing"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing err = %v", err)
	}
}

func TestServerItemApplyIsTransactionalAndReplayable(t *testing.T) {
	// Arrange
	store := newTestStore(t)
	ctx := testContext(t)
	items := store.Items()
	first := serverItem("gh-acme", "issue/1", "One", "")
	second := serverItem("gh-acme", "issue/2", "Two", "")
	gone := domain.Item{Ref: first.Ref, Deleted: true, ObservedAt: serverItemClock}
	batch := []domain.Item{first, second, gone}
	invalid := serverItem("gh-acme", "issue/3", "Three", "")
	invalid.Hash = "sha256:" + fmt.Sprintf("%064d", 0)

	// Act
	outcomes, err := items.Apply(ctx, batch)
	replay, replayErr := items.Apply(ctx, batch)
	_, invalidErr := items.Apply(ctx, []domain.Item{serverItem("gh-acme", "issue/4", "Four", ""), invalid})

	// Assert
	if err != nil || fmt.Sprint(outcomes) != fmt.Sprint([]ports.UpsertOutcome{ports.UpsertCreated, ports.UpsertCreated, ports.UpsertUpdated}) {
		t.Fatalf("Apply = %v, %v", outcomes, err)
	}
	if replayErr != nil || fmt.Sprint(replay) != fmt.Sprint([]ports.UpsertOutcome{ports.UpsertCreated, ports.UpsertUnchanged, ports.UpsertUpdated}) {
		// Replaying revives the first item and deletes it again in order.
		t.Fatalf("replay = %v, %v", replay, replayErr)
	}
	if !errors.Is(invalidErr, domain.ErrValidation) {
		t.Fatalf("invalid batch err = %v", invalidErr)
	}
	if _, err := items.Get(ctx, domain.ItemRef{ConnectionID: "gh-acme", ExternalID: "issue/4"}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rejected batch wrote an item: %v", err)
	}
	if got, err := items.Get(ctx, first.Ref); err != nil || !got.Deleted {
		t.Fatalf("first = %+v, %v", got, err)
	}
}

func TestServerItemListPagingAndPurge(t *testing.T) {
	// Arrange
	store := newTestStore(t)
	ctx := testContext(t)
	items := store.Items()
	for index := 0; index < 5; index++ {
		if _, err := items.Upsert(ctx, serverItem("gh-acme", fmt.Sprintf("issue/%d", index), "Issue", "")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := items.Upsert(ctx, serverItem("other", "x", "Other", "")); err != nil {
		t.Fatal(err)
	}
	if _, err := items.Delete(ctx, domain.ItemRef{ConnectionID: "gh-acme", ExternalID: "issue/2"}, serverItemClock); err != nil {
		t.Fatal(err)
	}

	// Act
	var keys []string
	cursor := ""
	for {
		page, err := items.List(ctx, "gh-acme", ports.ItemListOptions{Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, item := range page.Items {
			keys = append(keys, item.Key())
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	withDeleted, err := items.List(ctx, "gh-acme", ports.ItemListOptions{IncludeDeleted: true})
	purgeErr := items.Purge(ctx, "gh-acme")
	afterPurge, afterErr := items.List(ctx, "gh-acme", ports.ItemListOptions{IncludeDeleted: true})
	other, otherErr := items.List(ctx, "other", ports.ItemListOptions{})

	// Assert
	if len(keys) != 4 {
		t.Fatalf("live keys = %v", keys)
	}
	for index := 1; index < len(keys); index++ {
		if keys[index-1] >= keys[index] {
			t.Fatalf("keys out of order: %v", keys)
		}
	}
	if err != nil || len(withDeleted.Items) != 5 {
		t.Fatalf("with deleted = %d, %v", len(withDeleted.Items), err)
	}
	if purgeErr != nil || afterErr != nil || len(afterPurge.Items) != 0 || otherErr != nil || len(other.Items) != 1 {
		t.Fatalf("purge %v, after %+v %v, other %+v %v", purgeErr, afterPurge, afterErr, other, otherErr)
	}
	if _, err := items.List(ctx, "other", ports.ItemListOptions{Cursor: encodeItemCursor(keys[0])}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("foreign cursor err = %v", err)
	}
}

func TestServerCheckpointsCaseSensitiveAndDelete(t *testing.T) {
	// Arrange
	store := newTestStore(t)
	ctx := testContext(t)
	checkpoints := store.Checkpoints()
	upper := domain.Checkpoint{Stream: "INBOX", Version: 1, Data: []byte{0, 1, 0xff}}
	lower := domain.Checkpoint{Stream: "inbox", Version: 2, Data: []byte("lower")}

	// Act
	for _, checkpoint := range []domain.Checkpoint{upper, lower, {Stream: "INBOX", Version: 3, Data: []byte("new")}} {
		if err := checkpoints.Put(ctx, "mail-corp", checkpoint); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	gotUpper, upperErr := checkpoints.Get(ctx, "mail-corp", "INBOX")
	listed, listErr := checkpoints.List(ctx, "mail-corp")
	deleteErr := checkpoints.Delete(ctx, "mail-corp", "inbox")
	_, missingErr := checkpoints.Get(ctx, "mail-corp", "inbox")
	allErr := checkpoints.Delete(ctx, "mail-corp", "")
	empty, emptyErr := checkpoints.List(ctx, "mail-corp")

	// Assert
	if upperErr != nil || gotUpper.Version != 3 || !bytes.Equal(gotUpper.Data, []byte("new")) {
		t.Fatalf("INBOX = %+v, %v", gotUpper, upperErr)
	}
	if listErr != nil || len(listed) != 2 || listed[0].Stream != "INBOX" || listed[1].Stream != "inbox" {
		t.Fatalf("List = %+v, %v", listed, listErr)
	}
	if deleteErr != nil || !errors.Is(missingErr, domain.ErrNotFound) || allErr != nil || emptyErr != nil || len(empty) != 0 {
		t.Fatalf("delete %v, missing %v, all %v, empty %+v %v", deleteErr, missingErr, allErr, empty, emptyErr)
	}
	if err := checkpoints.Put(ctx, "mail-corp", domain.Checkpoint{Stream: "x", Version: 1, Data: make([]byte, domain.MaxCheckpointBytes+1)}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("oversized err = %v", err)
	}
}

func TestServerSearchItemsOnlyWhenRequestedAndScoped(t *testing.T) {
	// Arrange
	store := newTestStore(t)
	ctx := testContext(t)
	open := mustProject(t, ctx, store, "items_open")
	secret := mustProject(t, ctx, store, "items_secret")
	for _, item := range []domain.Item{
		serverItem("tracker", "open-1", "Capybara open", open.ID),
		serverItem("tracker", "secret-1", "Capybara secret", secret.ID),
		serverItem("tracker", "unmapped-1", "Capybara unmapped", ""),
		serverItem("tracker", "ghost-1", "Capybara ghost", "no_such_project"),
	} {
		if _, err := store.Items().Upsert(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Items().Delete(ctx, domain.ItemRef{ConnectionID: "tracker", ExternalID: "gone"}, serverItemClock); err != nil {
		t.Fatal(err)
	}
	all := httpapi.ProjectScope{}
	scoped := httpapi.ProjectScope{Exclude: []string{secret.ID}}

	// Act + Assert
	cases := []struct {
		name  string
		query ports.Query
		scope httpapi.ProjectScope
		want  int
	}{
		{"curated default excludes dump", ports.Query{Text: "capybara", ExcludeKinds: []string{"dump"}}, all, 0},
		{"dumps requested", ports.Query{Text: "capybara"}, all, 2},
		{"kind dump", ports.Query{Text: "capybara", Kinds: []string{"dump"}}, all, 2},
		{"kind item", ports.Query{Text: "capybara", Kinds: []string{"item"}}, all, 2},
		{"kind note only", ports.Query{Text: "capybara", Kinds: []string{"note"}}, all, 0},
		{"scope excludes secret", ports.Query{Text: "capybara"}, scoped, 1},
		{"scope only open", ports.Query{Text: "capybara", Kinds: []string{"item"}}, httpapi.ProjectScope{Only: []string{open.ID}}, 1},
	}
	for _, tc := range cases {
		hits, err := store.SearchScoped(ctx, tc.query, tc.scope)
		if err != nil || len(hits) != tc.want {
			t.Fatalf("%s: hits = %+v, %v; want %d", tc.name, hits, err, tc.want)
		}
		for _, hit := range hits {
			if hit.Kind != "item" || hit.ProjectID == secret.ID && len(tc.scope.Exclude) > 0 {
				t.Fatalf("%s: unexpected hit %+v", tc.name, hit)
			}
		}
	}
}

func TestServerConnectorRunPersistsConnectionAndStats(t *testing.T) {
	// Arrange
	store := newTestStore(t)
	ctx := testContext(t)
	stats := &domain.RunStats{Seen: 3, Upserted: 2, Unchanged: 1, Batches: 1}
	run := domain.ConnectorRun{Source: "github", Status: domain.ConnectorRunning, ConnectionID: "gh-acme", Stats: &domain.RunStats{}}

	// Act
	created, _, createErr := store.CreateConnectorRun(ctx, run)
	finished, _, finishErr := store.FinishConnectorRunStats(ctx, created.ID, domain.ConnectorSuccess, nil, stats, "", 0)
	legacy, _, legacyErr := store.CreateConnectorRun(ctx, domain.ConnectorRun{Source: "gitlab", Status: domain.ConnectorRunning})
	legacyDone, _, legacyDoneErr := store.FinishConnectorRun(ctx, legacy.ID, domain.ConnectorSuccess, map[string]int{"issues": 1}, "", 0)
	loaded, getErr := store.GetConnectorRun(ctx, created.ID)

	// Assert
	if createErr != nil || finishErr != nil || legacyErr != nil || legacyDoneErr != nil || getErr != nil {
		t.Fatalf("errors: %v %v %v %v %v", createErr, finishErr, legacyErr, legacyDoneErr, getErr)
	}
	if finished.ConnectionID != "gh-acme" || finished.Stats == nil || *finished.Stats != *stats {
		t.Fatalf("finished = %+v", finished.ConnectorRun)
	}
	if loaded.ConnectionID != "gh-acme" || loaded.Stats == nil || *loaded.Stats != *stats {
		t.Fatalf("loaded = %+v", loaded.ConnectorRun)
	}
	if legacyDone.ConnectionID != "" || legacyDone.Stats != nil {
		t.Fatalf("legacy run gained item fields: %+v", legacyDone.ConnectorRun)
	}
}
