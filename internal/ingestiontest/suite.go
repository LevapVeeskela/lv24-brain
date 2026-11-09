package ingestiontest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// suiteCase is one contract case. A *skipError return skips the subtest;
// any other non-nil error fails it.
type suiteCase struct {
	name string
	fn   func(ctx context.Context, h Harness, desc domain.SourceDescriptor, pageSize int) error
}

// RunSourceSuite runs the source contract suite against h. Each case is a
// t.Run subtest; a case is skipped with a clear message when the Source
// descriptor does not declare the capability it needs.
func RunSourceSuite(t *testing.T, h Harness) {
	t.Helper()
	desc, pageSize, ok := setupSuite(t, h)
	if !ok {
		return
	}
	for _, c := range allCases(h, desc, pageSize) {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if err := c.fn(context.Background(), h, desc, pageSize); err != nil {
				var skip *skipError
				if errors.As(err, &skip) {
					t.Skipf("%s", skip.msg)
				} else {
					t.Fatal(err)
				}
			}
		})
	}
}

// setupSuite validates the harness and reports the descriptor and page size.
func setupSuite(t *testing.T, h Harness) (domain.SourceDescriptor, int, bool) {
	t.Helper()
	if h.NewSource == nil {
		t.Fatal("ingestiontest: harness NewSource is required")
		return domain.SourceDescriptor{}, 0, false
	}
	if err := h.Connection.Validate(); err != nil {
		t.Fatalf("ingestiontest: harness connection is invalid: %v", err)
		return domain.SourceDescriptor{}, 0, false
	}
	source := h.NewSource(h.Connection)
	if source == nil {
		t.Fatal("ingestiontest: harness NewSource returned nil")
		return domain.SourceDescriptor{}, 0, false
	}
	desc := source.Descriptor()
	if err := desc.Validate(); err != nil {
		t.Fatalf("ingestiontest: source descriptor is invalid: %v", err)
		return domain.SourceDescriptor{}, 0, false
	}
	if desc.Provider != h.Connection.Provider {
		t.Fatalf("ingestiontest: descriptor provider %q does not match connection provider %q",
			desc.Provider, h.Connection.Provider)
		return domain.SourceDescriptor{}, 0, false
	}
	return desc, h.pageSize(), true
}

// allCases returns the suite cases in run order. Broken-variant tests call
// individual cases to assert they fail without failing the outer test.
func allCases(h Harness, desc domain.SourceDescriptor, pageSize int) []suiteCase {
	return []suiteCase{
		{name: "check", fn: caseCheck},
		{name: "backfill", fn: caseBackfill},
		{name: "determinism", fn: caseDeterminism},
		{name: "incremental", fn: caseIncremental},
		{name: "deletes", fn: caseDeletes},
		{name: "paging", fn: casePaging},
		{name: "crash-replay", fn: caseCrashReplay},
		{name: "cancellation", fn: caseCancellation},
		{name: "checkpoints", fn: caseCheckpoints},
	}
}

// caseCheck: Check succeeds on a valid fake upstream and fails clearly on
// bad credentials when the harness provides a bad-credential variant.
func caseCheck(ctx context.Context, h Harness, _ domain.SourceDescriptor, _ int) error {
	source := h.NewSource(h.Connection)
	if source == nil {
		return errors.New("harness NewSource returned nil")
	}
	if err := source.Check(ctx); err != nil {
		return fmt.Errorf("Check on the valid fake upstream failed: %w", err)
	}
	if h.BadConnection == nil {
		return nil
	}
	bad := h.NewSource(*h.BadConnection)
	if bad == nil {
		return errors.New("harness NewSource returned nil for the bad connection")
	}
	if err := bad.Check(ctx); err == nil {
		return errors.New("Check with bad credentials succeeded; want a clear error")
	}
	return nil
}

// caseBackfill: a full drive reads everything; every stored item validates,
// carries the right ConnectionID, has a stable ExternalID and a
// deterministic Hash; two full drives agree.
func caseBackfill(ctx context.Context, h Harness, _ domain.SourceDescriptor, _ int) error {
	first, err := driveFresh(ctx, h, Budget{})
	if err != nil {
		return err
	}
	if first.stats.Seen == 0 {
		if h.AddMany == nil {
			return errors.New("backfill read 0 items and the harness cannot seed data (no AddMany)")
		}
		h.AddMany(3)
		first, err = driveFresh(ctx, h, Budget{})
		if err != nil {
			return err
		}
		if first.stats.Seen == 0 {
			return errors.New("backfill read 0 items even after seeding 3")
		}
	}
	if err := assertValidStore(ctx, h, first.items, "backfill"); err != nil {
		return err
	}
	second, err := driveFresh(ctx, h, Budget{})
	if err != nil {
		return err
	}
	if err := assertSameSets(first.items, second.items, "two full drives disagree"); err != nil {
		return err
	}
	live := 0
	for _, item := range first.items {
		if !item.Deleted {
			live++
		}
	}
	if live == 0 {
		return errors.New("backfill stored no live items")
	}
	return nil
}

// caseDeterminism: repeated reads from the same checkpoints yield identical
// items, and re-applying stored items is a no-op (all unchanged).
func caseDeterminism(ctx context.Context, h Harness, _ domain.SourceDescriptor, _ int) error {
	source := h.NewSource(h.Connection)
	host := NewHost()
	if _, err := host.Drive(ctx, source, h.Connection, Budget{}); err != nil {
		return fmt.Errorf("initial drive: %w", err)
	}
	empty := map[string]domain.Checkpoint{}
	first, err := source.Read(ctx, ports.ReadRequest{Connection: h.Connection, Checkpoints: empty})
	if err != nil {
		return fmt.Errorf("first repeated read: %w", err)
	}
	second, err := source.Read(ctx, ports.ReadRequest{Connection: h.Connection, Checkpoints: empty})
	if err != nil {
		return fmt.Errorf("second repeated read: %w", err)
	}
	if err := assertSameBatch(first, second); err != nil {
		return fmt.Errorf("repeated read from the same checkpoints diverged (source is not deterministic): %w", err)
	}
	stats, err := host.Drive(ctx, source, h.Connection, Budget{})
	if err != nil {
		return fmt.Errorf("re-drive from persisted checkpoints: %w", err)
	}
	if stats.Upserted != 0 || stats.Deleted != 0 {
		return fmt.Errorf("re-drive after no upstream change must be a no-op, got upserted=%d deleted=%d (seen=%d)",
			stats.Upserted, stats.Deleted, stats.Seen)
	}
	stored, err := collectAll(ctx, host.Items, h.Connection.ID)
	if err != nil {
		return err
	}
	if len(stored) == 0 {
		return errors.New("nothing stored to re-apply")
	}
	outcomes, err := host.Items.Apply(ctx, stored)
	if err != nil {
		return fmt.Errorf("re-apply: %w", err)
	}
	for i, outcome := range outcomes {
		if outcome != ports.UpsertUnchanged {
			return fmt.Errorf("re-apply of stored item %d (%s) reported %q; want unchanged",
				i, stored[i].Ref.ExternalID, outcome)
		}
	}
	return nil
}

// caseIncremental: after an upstream update only changed items are
// returned/updated, checkpoints advance, and no deletions are inferred from
// a filtered or partial read.
func caseIncremental(ctx context.Context, h Harness, desc domain.SourceDescriptor, _ int) error {
	if h.UpdateItem == nil {
		return skipf("harness provides no UpdateItem")
	}
	before, err := driveFresh(ctx, h, Budget{})
	if err != nil {
		return err
	}
	if before.liveCount() == 0 {
		if h.AddMany == nil {
			return errors.New("nothing stored and the harness cannot seed data (no AddMany)")
		}
		h.AddMany(2)
		before, err = driveFresh(ctx, h, Budget{})
		if err != nil {
			return err
		}
		if before.liveCount() == 0 {
			return errors.New("nothing stored even after seeding")
		}
	}
	beforeCps, err := before.host.Checkpoints.List(ctx, h.Connection.ID)
	if err != nil {
		return err
	}
	h.UpdateItem()
	after, err := driveOnto(ctx, h, before.host, Budget{})
	if err != nil {
		return fmt.Errorf("drive after upstream update: %w", err)
	}
	if after.stats.Upserted == 0 {
		return errors.New("upstream update was not picked up (upserted=0)")
	}
	if after.stats.Deleted != 0 {
		return fmt.Errorf("upstream update introduced %d deletions", after.stats.Deleted)
	}
	changed, lost := diffSnapshots(before.items, after.items)
	if len(changed) == 0 {
		return errors.New("updated item content did not change in the store")
	}
	if len(lost) != 0 {
		return fmt.Errorf("items lost without a tombstone: %q", lost)
	}
	if desc.Capabilities.Incremental {
		afterCps, err := after.host.Checkpoints.List(ctx, h.Connection.ID)
		if err != nil {
			return err
		}
		if sameCheckpoints(beforeCps, afterCps) {
			return errors.New("checkpoints did not advance after an upstream update")
		}
	}
	// A filtered read narrows what is read; it must never delete. Drive a
	// fully synced host with a future Since and require zero deletions and
	// intact items.
	future := time.Now().UTC().Add(time.Hour)
	filtered, err := driveOnto(ctx, h, after.host, Budget{Since: &future})
	if err != nil {
		return fmt.Errorf("filtered drive: %w", err)
	}
	if filtered.stats.Deleted != 0 {
		return fmt.Errorf("filtered read produced %d deletions; deletions need tombstones, never absence",
			filtered.stats.Deleted)
	}
	if err := assertIntact(after.items, filtered.items); err != nil {
		return fmt.Errorf("filtered read lost items: %w", err)
	}
	// A partial (single page) read must not tombstone items it did not
	// return either.
	partialHost := NewHost()
	partialSource := h.NewSource(h.Connection)
	_, partialStats, err := partialHost.RunBatch(ctx, partialSource, h.Connection, ports.ReadRequest{
		Connection:  h.Connection,
		Checkpoints: map[string]domain.Checkpoint{},
		Limit:       1,
	})
	if err != nil {
		return fmt.Errorf("partial read: %w", err)
	}
	if partialStats.Deleted != 0 {
		return fmt.Errorf("partial read of 1 item produced %d deletions; the host must never infer deletion from a partial read",
			partialStats.Deleted)
	}
	return nil
}

// caseDeletes: an upstream deletion surfaces as a tombstone; other items
// are untouched. Skipped without the Deletes capability or DeleteItem.
func caseDeletes(ctx context.Context, h Harness, desc domain.SourceDescriptor, _ int) error {
	if !desc.Capabilities.Deletes {
		return skipf("descriptor does not declare the Deletes capability")
	}
	if h.DeleteItem == nil {
		return skipf("harness provides no DeleteItem")
	}
	before, err := driveFresh(ctx, h, Budget{})
	if err != nil {
		return err
	}
	if before.liveCount() < 2 {
		if h.AddMany == nil {
			return errors.New("need at least 2 live items for the delete case and the harness cannot seed data")
		}
		h.AddMany(2)
		before, err = driveFresh(ctx, h, Budget{})
		if err != nil {
			return err
		}
	}
	if before.liveCount() == 0 {
		return errors.New("nothing stored to delete")
	}
	h.DeleteItem()
	after, err := driveOnto(ctx, h, before.host, Budget{})
	if err != nil {
		return fmt.Errorf("drive after upstream deletion: %w", err)
	}
	if after.stats.Deleted == 0 {
		return errors.New("upstream deletion produced no tombstone (deleted=0)")
	}
	tombstoned, lost := diffSnapshots(before.items, after.items)
	_ = tombstoned
	if len(lost) != 0 {
		return fmt.Errorf("items lost without a tombstone: %q", lost)
	}
	foundTombstone := false
	for _, item := range after.items {
		if !item.Deleted {
			continue
		}
		if _, ok := before.live[item.Key()]; ok {
			foundTombstone = true
			got, err := after.host.Items.Get(ctx, item.Ref)
			if err != nil {
				return fmt.Errorf("tombstoned item %q not retrievable: %w", item.Ref.ExternalID, err)
			}
			if !got.Deleted {
				return fmt.Errorf("item %q should be a tombstone", item.Ref.ExternalID)
			}
		}
	}
	if !foundTombstone {
		return errors.New("no previously live item became a tombstone")
	}
	return nil
}

// casePaging: N > pageSize items are all read across batches, More is
// honored, and the paged read matches an unpaged baseline with no
// duplicates or losses.
func casePaging(ctx context.Context, h Harness, _ domain.SourceDescriptor, pageSize int) error {
	if h.AddMany == nil {
		return skipf("harness provides no AddMany")
	}
	h.AddMany(2*pageSize + 1)
	baseline, err := driveFresh(ctx, h, Budget{})
	if err != nil {
		return fmt.Errorf("unpaged baseline: %w", err)
	}
	if len(baseline.items) <= pageSize {
		return fmt.Errorf("need more than %d items for paging, have %d", pageSize, len(baseline.items))
	}
	source := h.NewSource(h.Connection)
	checkpoints := map[string]domain.Checkpoint{}
	var collected []domain.Item
	seen := make(map[string]struct{})
	batches := 0
	for {
		batches++
		if batches > 10000 {
			return errors.New("More never cleared after 10000 batches")
		}
		batch, err := source.Read(ctx, ports.ReadRequest{
			Connection:  h.Connection,
			Checkpoints: cloneCheckpointMap(checkpoints),
			Limit:       pageSize,
		})
		if err != nil {
			return fmt.Errorf("paged read %d: %w", batches, err)
		}
		for _, checkpoint := range batch.Checkpoints {
			if err := checkpoint.Validate(); err != nil {
				return fmt.Errorf("paged read %d: invalid checkpoint: %w", batches, err)
			}
			if len(checkpoint.Data) > domain.MaxCheckpointBytes {
				return fmt.Errorf("paged read %d: checkpoint exceeds %d bytes", batches, domain.MaxCheckpointBytes)
			}
		}
		for _, item := range batch.Items {
			key := item.Key()
			if _, ok := seen[key]; ok {
				return fmt.Errorf("paged read returned duplicate item ref %s", key)
			}
			seen[key] = struct{}{}
			collected = append(collected, item.WithHash())
		}
		for _, checkpoint := range batch.Checkpoints {
			checkpoints[checkpoint.Stream] = checkpoint
		}
		if !batch.More {
			break
		}
	}
	if batches < 2 {
		return fmt.Errorf("expected multiple batches for %d items at page size %d, got %d",
			len(baseline.items), pageSize, batches)
	}
	if err := assertSameSets(baseline.items, collected, "paged read does not match the unpaged baseline"); err != nil {
		return err
	}
	return nil
}

// caseCrashReplay: dropping the checkpoints of one batch (a crash between
// Apply and Put) and re-running must not duplicate items and must converge
// to the same final state.
func caseCrashReplay(ctx context.Context, h Harness, _ domain.SourceDescriptor, pageSize int) error {
	total, err := driveFresh(ctx, h, Budget{})
	if err != nil {
		return err
	}
	if len(total.items) < pageSize+1 {
		if h.AddMany == nil {
			return skipf("need more than %d items for a multi-batch replay and the harness cannot seed data", pageSize)
		}
		h.AddMany(pageSize + 1 - len(total.items))
	}
	expected, err := driveFresh(ctx, h, Budget{BatchLimit: pageSize})
	if err != nil {
		return fmt.Errorf("clean drive: %w", err)
	}
	// Crash between Apply and Put: persist the first batch's items, drop
	// its checkpoints.
	crashed := NewHost()
	source := h.NewSource(h.Connection)
	first, err := source.Read(ctx, ports.ReadRequest{
		Connection:  h.Connection,
		Checkpoints: map[string]domain.Checkpoint{},
		Limit:       pageSize,
	})
	if err != nil {
		return fmt.Errorf("first batch read: %w", err)
	}
	if len(first.Items) == 0 {
		return errors.New("first batch is empty; cannot simulate a crash")
	}
	prepared, _, err := crashed.Prepare(expected.desc, h.Connection, first)
	if err != nil {
		return fmt.Errorf("first batch invalid: %w", err)
	}
	if _, err := crashed.Items.Apply(ctx, prepared); err != nil {
		return fmt.Errorf("first batch apply: %w", err)
	}
	// Re-run to completion from the old (empty) checkpoints.
	replaySource := h.NewSource(h.Connection)
	if _, err := crashed.Drive(ctx, replaySource, h.Connection, Budget{BatchLimit: pageSize}); err != nil {
		return fmt.Errorf("replay drive: %w", err)
	}
	replayed, err := collectAll(ctx, crashed.Items, h.Connection.ID)
	if err != nil {
		return err
	}
	if err := assertSameSets(expected.items, replayed, "replay diverged from the clean drive"); err != nil {
		return err
	}
	outcomes, err := crashed.Items.Apply(ctx, replayed)
	if err != nil {
		return fmt.Errorf("re-apply after replay: %w", err)
	}
	for i, outcome := range outcomes {
		if outcome != ports.UpsertUnchanged {
			return fmt.Errorf("re-apply after replay reported %q for item %d; want unchanged (duplicates?)",
				outcome, i)
		}
	}
	expectedCps, err := expected.host.Checkpoints.List(ctx, h.Connection.ID)
	if err != nil {
		return err
	}
	replayedCps, err := crashed.Checkpoints.List(ctx, h.Connection.ID)
	if err != nil {
		return err
	}
	if !sameCheckpoints(expectedCps, replayedCps) {
		return errors.New("replay checkpoints differ from the clean drive")
	}
	return nil
}

// caseCancellation: a canceled context returns promptly with an error and
// advances no checkpoint.
func caseCancellation(ctx context.Context, h Harness, _ domain.SourceDescriptor, _ int) error {
	seeded, err := driveFresh(ctx, h, Budget{})
	if err != nil {
		return err
	}
	before, err := seeded.host.Checkpoints.List(ctx, h.Connection.ID)
	if err != nil {
		return err
	}
	checkpointMap := make(map[string]domain.Checkpoint, len(before))
	for _, checkpoint := range before {
		checkpointMap[checkpoint.Stream] = checkpoint
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	source := h.NewSource(h.Connection)
	if _, err := source.Read(canceled, ports.ReadRequest{
		Connection:  h.Connection,
		Checkpoints: checkpointMap,
		Limit:       1,
	}); err == nil {
		return errors.New("Read with a canceled context succeeded; the source must honor cancellation")
	}
	if _, err := seeded.host.Drive(canceled, source, h.Connection, Budget{}); err == nil {
		return errors.New("Drive with a canceled context succeeded; want an error")
	}
	after, err := seeded.host.Checkpoints.List(ctx, h.Connection.ID)
	if err != nil {
		return err
	}
	if !sameCheckpoints(before, after) {
		return errors.New("canceled run advanced checkpoints")
	}
	return nil
}

// caseCheckpoints: persisted checkpoints are opaque and within
// domain.MaxCheckpointBytes, and every stream validates (and is declared
// when the descriptor lists Streams).
func caseCheckpoints(ctx context.Context, h Harness, desc domain.SourceDescriptor, _ int) error {
	driven, err := driveFresh(ctx, h, Budget{})
	if err != nil {
		return err
	}
	checkpoints, err := driven.host.Checkpoints.List(ctx, h.Connection.ID)
	if err != nil {
		return err
	}
	if len(checkpoints) == 0 {
		if desc.Capabilities.Incremental {
			return errors.New("incremental source persisted no checkpoints")
		}
		return skipf("source persists no checkpoints")
	}
	for _, checkpoint := range checkpoints {
		if err := checkpoint.Validate(); err != nil {
			return fmt.Errorf("persisted checkpoint invalid: %w", err)
		}
		if len(checkpoint.Data) > domain.MaxCheckpointBytes {
			return fmt.Errorf("checkpoint for stream %q exceeds %d bytes", checkpoint.Stream, domain.MaxCheckpointBytes)
		}
		if len(desc.Streams) > 0 {
			declared := false
			for _, stream := range desc.Streams {
				if strings.EqualFold(stream, checkpoint.Stream) {
					declared = true
					break
				}
			}
			if !declared {
				return fmt.Errorf("checkpoint stream %q is not declared in the descriptor", checkpoint.Stream)
			}
		}
		// The store keeps bytes byte-for-byte: re-put and re-get must be
		// identical.
		if err := driven.host.Checkpoints.Put(ctx, h.Connection.ID, checkpoint); err != nil {
			return fmt.Errorf("re-put checkpoint: %w", err)
		}
		got, err := driven.host.Checkpoints.Get(ctx, h.Connection.ID, checkpoint.Stream)
		if err != nil {
			return fmt.Errorf("re-get checkpoint: %w", err)
		}
		if !got.Equal(checkpoint) {
			return fmt.Errorf("checkpoint for stream %q did not round-trip byte-for-byte", checkpoint.Stream)
		}
	}
	return nil
}

// driveResult is one full Drive on a fresh host.
type driveResult struct {
	host  *Host
	stats domain.RunStats
	items []domain.Item
	live  map[string]domain.Item
	desc  domain.SourceDescriptor
}

// driveFresh runs a full Drive on a fresh host and snapshots the store.
func driveFresh(ctx context.Context, h Harness, budget Budget) (*driveResult, error) {
	host := NewHost()
	return driveOnto(ctx, h, host, budget)
}

// driveOnto runs a full Drive onto host and snapshots the store.
func driveOnto(ctx context.Context, h Harness, host *Host, budget Budget) (*driveResult, error) {
	source := h.NewSource(h.Connection)
	if source == nil {
		return nil, errors.New("harness NewSource returned nil")
	}
	stats, err := host.Drive(ctx, source, h.Connection, budget)
	if err != nil {
		return nil, err
	}
	items, err := collectAll(ctx, host.Items, h.Connection.ID)
	if err != nil {
		return nil, err
	}
	result := &driveResult{host: host, stats: stats, items: items, live: make(map[string]domain.Item)}
	for _, item := range items {
		result.live[item.Key()] = item
		if item.Deleted {
			delete(result.live, item.Key())
		}
	}
	result.desc = source.Descriptor()
	return result, nil
}

// liveCount counts non-tombstone items in the snapshot.
func (r *driveResult) liveCount() int { return len(r.live) }

// collectAll pages the whole connection, tombstones included.
func collectAll(ctx context.Context, store *MemoryItemStore, connectionID string) ([]domain.Item, error) {
	var out []domain.Item
	cursor := ""
	for {
		page, err := store.List(ctx, connectionID, ports.ItemListOptions{Cursor: cursor, IncludeDeleted: true})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

// assertValidStore checks every stored item: Validate, right ConnectionID,
// stable non-empty ExternalID and a deterministic Hash with the right
// prefix. Tombstones skip the content checks.
func assertValidStore(ctx context.Context, h Harness, items []domain.Item, what string) error {
	_ = ctx
	seen := make(map[string]string, len(items))
	for i, item := range items {
		if err := item.Validate(); err != nil {
			return fmt.Errorf("%s: item %d invalid: %w", what, i, err)
		}
		if item.Ref.ConnectionID != h.Connection.ID {
			return fmt.Errorf("%s: item %d carries connectionId %q; want %q",
				what, i, item.Ref.ConnectionID, h.Connection.ID)
		}
		if item.Hash != item.ComputeHash() {
			return fmt.Errorf("%s: item %q hash does not match content (non-deterministic?)", what, item.Ref.ExternalID)
		}
		if !strings.HasPrefix(item.Hash, domain.ItemHashPrefix) {
			return fmt.Errorf("%s: item %q hash %q lacks the %q prefix", what, item.Ref.ExternalID, item.Hash, domain.ItemHashPrefix)
		}
		if item.Deleted {
			continue
		}
		if item.Ref.ExternalID == "" {
			return fmt.Errorf("%s: item %d has an empty ExternalID", what, i)
		}
		if prev, ok := seen[item.Ref.ExternalID]; ok && prev != item.Key() {
			return fmt.Errorf("%s: ExternalID %q maps to two keys", what, item.Ref.ExternalID)
		}
		seen[item.Ref.ExternalID] = item.Key()
	}
	return nil
}

// assertSameSets requires identical key sets with identical hashes and
// tombstone flags.
func assertSameSets(a, b []domain.Item, what string) error {
	am := keyMap(a)
	bm := keyMap(b)
	if len(am) != len(bm) {
		return fmt.Errorf("%s: size differs (%d vs %d)", what, len(am), len(bm))
	}
	for key, x := range am {
		y, ok := bm[key]
		if !ok {
			return fmt.Errorf("%s: item %s missing from one side (loss or duplicate?)", what, key)
		}
		if x.Hash != y.Hash || x.Deleted != y.Deleted {
			return fmt.Errorf("%s: item %s differs between reads", what, key)
		}
	}
	return nil
}

// assertSameBatch requires identical items, checkpoints and More.
func assertSameBatch(a, b ports.ReadBatch) error {
	if a.More != b.More {
		return fmt.Errorf("More differs (%v vs %v)", a.More, b.More)
	}
	if len(a.Items) != len(b.Items) {
		return fmt.Errorf("item count differs (%d vs %d)", len(a.Items), len(b.Items))
	}
	for i := range a.Items {
		x, y := a.Items[i], b.Items[i]
		if x.Ref != y.Ref || x.Deleted != y.Deleted {
			return fmt.Errorf("item %d ref differs (%v vs %v)", i, x.Ref, y.Ref)
		}
		if x.ComputeHash() != y.ComputeHash() {
			return fmt.Errorf("item %d (%s) content differs between identical reads", i, x.Ref.ExternalID)
		}
	}
	if len(a.Checkpoints) != len(b.Checkpoints) {
		return fmt.Errorf("checkpoint count differs (%d vs %d)", len(a.Checkpoints), len(b.Checkpoints))
	}
	for i := range a.Checkpoints {
		if !a.Checkpoints[i].Equal(b.Checkpoints[i]) {
			return fmt.Errorf("checkpoint %d differs between identical reads", i)
		}
	}
	return nil
}

type itemSummary struct {
	Hash    string
	Deleted bool
}

func keyMap(items []domain.Item) map[string]itemSummary {
	out := make(map[string]itemSummary, len(items))
	for _, item := range items {
		out[item.Key()] = itemSummary{Hash: item.WithHash().Hash, Deleted: item.Deleted}
	}
	return out
}

// diffSnapshots compares before/after snapshots: changed holds live keys
// whose hash changed plus brand-new live keys; lost holds before-live keys
// that vanished entirely (not even tombstoned).
func diffSnapshots(before, after []domain.Item) (changed []string, lost []string) {
	bm := keyMap(before)
	am := keyMap(after)
	for key, x := range bm {
		y, ok := am[key]
		if !ok {
			lost = append(lost, key)
			continue
		}
		if x.Deleted != y.Deleted || x.Hash != y.Hash {
			changed = append(changed, key)
		}
	}
	for key := range am {
		if _, ok := bm[key]; !ok {
			changed = append(changed, key)
		}
	}
	return changed, lost
}

// assertIntact requires every live item of before to still be live in after
// with the same hash.
func assertIntact(before, after []domain.Item) error {
	bm := keyMap(before)
	am := keyMap(after)
	for key, x := range bm {
		if x.Deleted {
			continue
		}
		y, ok := am[key]
		if !ok {
			return fmt.Errorf("item %s vanished", key)
		}
		if y.Deleted {
			return fmt.Errorf("item %s was tombstoned by a filtered read", key)
		}
		if y.Hash != x.Hash {
			return fmt.Errorf("item %s changed under a filtered read", key)
		}
	}
	return nil
}

func sameCheckpoints(a, b []domain.Checkpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

func cloneCheckpointMap(in map[string]domain.Checkpoint) map[string]domain.Checkpoint {
	out := make(map[string]domain.Checkpoint, len(in))
	for stream, checkpoint := range in {
		out[stream] = checkpoint.Clone()
	}
	return out
}
