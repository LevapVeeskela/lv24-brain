package ingestiontest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Budget bounds one Drive call.
type Budget struct {
	// BatchLimit caps the items of a single Read; 0 selects the source
	// default. The whole run stops when the source reports More false.
	BatchLimit int
	// MaxBatches caps the number of Read calls; <= 0 selects 1000. Hitting
	// the cap stops the run with Truncated set to limit.
	MaxBatches int
	// Since is an optional lower bound on provider update time. It narrows
	// what is read; it never means unseen items were deleted.
	Since *time.Time
}

func (b Budget) maxBatches() int {
	if b.MaxBatches > 0 {
		return b.MaxBatches
	}
	return 1000
}

// MemoryItemStore is an in-memory ports.ItemStore for tests.
type MemoryItemStore struct {
	mu    sync.Mutex
	items map[string]domain.Item
}

// NewMemoryItemStore returns an empty store.
func NewMemoryItemStore() *MemoryItemStore {
	return &MemoryItemStore{items: make(map[string]domain.Item)}
}

// Upsert stores item. Equal Hash on an existing entry is UpsertUnchanged and
// does not rewrite it; upserting a tombstoned key revives it.
func (s *MemoryItemStore) Upsert(ctx context.Context, item domain.Item) (ports.UpsertOutcome, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if item.Hash == "" {
		return "", domain.NewValidationError("item.hash", "is required")
	}
	if err := item.Validate(); err != nil {
		return "", err
	}
	key := item.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.items[key]
	if ok && existing.Hash == item.Hash {
		return ports.UpsertUnchanged, nil
	}
	s.items[key] = cloneItem(item)
	if !ok {
		return ports.UpsertCreated, nil
	}
	return ports.UpsertUpdated, nil
}

// Delete replaces the item with a tombstone observed at at. It reports
// whether a live item existed; deleting an unknown ref records the tombstone
// and returns false.
func (s *MemoryItemStore) Delete(ctx context.Context, ref domain.ItemRef, at time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := ref.Validate(); err != nil {
		return false, err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tombstone := domain.Item{Ref: ref, Deleted: true, ObservedAt: at}.WithHash()
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.items[tombstone.Key()]
	existed := ok && !existing.Deleted
	s.items[tombstone.Key()] = tombstone
	return existed, nil
}

// Get returns the item for ref or an error wrapping domain.ErrNotFound.
// Tombstones are returned with Deleted set.
func (s *MemoryItemStore) Get(ctx context.Context, ref domain.ItemRef) (domain.Item, error) {
	if err := ctx.Err(); err != nil {
		return domain.Item{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[ref.Key()]
	if !ok {
		return domain.Item{}, fmt.Errorf("%w: item %q", domain.ErrNotFound, ref.Key())
	}
	return cloneItem(item), nil
}

// List pages the items of one connection ordered by Item.Key().
func (s *MemoryItemStore) List(ctx context.Context, connectionID string, opts ports.ItemListOptions) (ports.ItemPage, error) {
	if err := ctx.Err(); err != nil {
		return ports.ItemPage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for key, item := range s.items {
		if item.Ref.ConnectionID != connectionID {
			continue
		}
		if !opts.IncludeDeleted && item.Deleted {
			continue
		}
		if opts.Cursor != "" && key <= opts.Cursor {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	limit := opts.Limit
	if limit <= 0 {
		limit = len(keys)
	}
	var page ports.ItemPage
	for i, key := range keys {
		if i >= limit {
			page.NextCursor = keys[limit-1]
			break
		}
		page.Items = append(page.Items, cloneItem(s.items[key]))
	}
	return page, nil
}

// Purge removes every item and tombstone of a connection.
func (s *MemoryItemStore) Purge(ctx context.Context, connectionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, item := range s.items {
		if item.Ref.ConnectionID == connectionID {
			delete(s.items, key)
		}
	}
	return nil
}

// Apply stores one batch in order: upserts for live items, tombstones for
// Deleted ones (at Item.ObservedAt). Outcomes align with items: re-applying
// an identical batch reports UpsertUnchanged for every entry.
func (s *MemoryItemStore) Apply(ctx context.Context, items []domain.Item) ([]ports.UpsertOutcome, error) {
	outcomes := make([]ports.UpsertOutcome, 0, len(items))
	for _, item := range items {
		if item.Deleted {
			at := item.ObservedAt
			if at.IsZero() {
				at = time.Now().UTC()
			}
			tombstone := domain.Item{Ref: item.Ref, Deleted: true, ObservedAt: at}.WithHash()
			s.mu.Lock()
			existing, ok := s.items[tombstone.Key()]
			if ok && existing.Hash == tombstone.Hash {
				s.mu.Unlock()
				outcomes = append(outcomes, ports.UpsertUnchanged)
				continue
			}
			s.items[tombstone.Key()] = tombstone
			s.mu.Unlock()
			if !ok {
				outcomes = append(outcomes, ports.UpsertCreated)
			} else {
				outcomes = append(outcomes, ports.UpsertUpdated)
			}
			continue
		}
		outcome, err := s.Upsert(ctx, item)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// MemoryCheckpointStore is an in-memory ports.CheckpointStore for tests.
type MemoryCheckpointStore struct {
	mu sync.Mutex
	// checkpoints[connectionID][stream] = checkpoint.
	checkpoints map[string]map[string]domain.Checkpoint
}

// NewMemoryCheckpointStore returns an empty store.
func NewMemoryCheckpointStore() *MemoryCheckpointStore {
	return &MemoryCheckpointStore{checkpoints: make(map[string]map[string]domain.Checkpoint)}
}

// Get returns the checkpoint or an error wrapping domain.ErrNotFound.
func (s *MemoryCheckpointStore) Get(ctx context.Context, connectionID, stream string) (domain.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return domain.Checkpoint{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	streams, ok := s.checkpoints[connectionID]
	if !ok {
		return domain.Checkpoint{}, fmt.Errorf("%w: no checkpoint for connection %q stream %q", domain.ErrNotFound, connectionID, stream)
	}
	checkpoint, ok := streams[stream]
	if !ok {
		return domain.Checkpoint{}, fmt.Errorf("%w: no checkpoint for connection %q stream %q", domain.ErrNotFound, connectionID, stream)
	}
	return checkpoint.Clone(), nil
}

// List returns every checkpoint of a connection ordered by stream.
func (s *MemoryCheckpointStore) List(ctx context.Context, connectionID string) ([]domain.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Checkpoint
	for _, checkpoint := range s.checkpoints[connectionID] {
		out = append(out, checkpoint.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stream < out[j].Stream })
	return out, nil
}

// Put replaces one checkpoint atomically. Data is stored byte-for-byte.
func (s *MemoryCheckpointStore) Put(ctx context.Context, connectionID string, checkpoint domain.Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	streams, ok := s.checkpoints[connectionID]
	if !ok {
		streams = make(map[string]domain.Checkpoint)
		s.checkpoints[connectionID] = streams
	}
	streams[checkpoint.Stream] = checkpoint.Clone()
	return nil
}

// Delete removes one stream's checkpoint; an empty stream removes all of
// the connection's checkpoints.
func (s *MemoryCheckpointStore) Delete(ctx context.Context, connectionID, stream string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if stream == "" {
		delete(s.checkpoints, connectionID)
		return nil
	}
	delete(s.checkpoints[connectionID], stream)
	return nil
}

// Host is the in-memory test host: memory stores plus the Drive loop that
// follows the host contract exactly. It is a test double, not production
// code.
type Host struct {
	Items       *MemoryItemStore
	Checkpoints *MemoryCheckpointStore
	// Now is the clock for ObservedAt; nil selects time.Now().UTC().
	Now func() time.Time
}

// NewHost returns a Host with empty memory stores.
func NewHost() *Host {
	return &Host{Items: NewMemoryItemStore(), Checkpoints: NewMemoryCheckpointStore()}
}

func (h *Host) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// Drive reads the source to exhaustion (while More and the budget allows)
// following the host contract per batch: Read with the persisted
// checkpoints; on error discard the whole batch; validate every item (after
// WithHash) and persist it via Apply; only after all items are durable,
// persist the batch checkpoints via Put.
func (h *Host) Drive(ctx context.Context, source ports.Source, conn domain.Connection, budget Budget) (domain.RunStats, error) {
	var stats domain.RunStats
	descriptor := source.Descriptor()
	for batch := 0; ; batch++ {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if batch >= budget.maxBatches() {
			stats.Truncated = domain.TruncatedLimit
			return stats, nil
		}
		stored, err := h.Checkpoints.List(ctx, conn.ID)
		if err != nil {
			return stats, err
		}
		checkpoints := make(map[string]domain.Checkpoint, len(stored))
		for _, checkpoint := range stored {
			checkpoints[checkpoint.Stream] = checkpoint
		}
		next, err := source.Read(ctx, ports.ReadRequest{
			Connection:  conn,
			Checkpoints: checkpoints,
			Since:       budget.Since,
			Limit:       budget.BatchLimit,
		})
		if err != nil {
			return stats, err
		}
		batchStats, err := h.commitBatch(ctx, descriptor, conn, next)
		if err != nil {
			return stats, err
		}
		stats = stats.Add(batchStats)
		if !next.More {
			return stats, nil
		}
	}
}

// RunBatch performs one Read plus the contracted store steps and returns
// the raw batch with its stats. It backs the partial-read and crash-replay
// cases.
func (h *Host) RunBatch(ctx context.Context, source ports.Source, conn domain.Connection, req ports.ReadRequest) (ports.ReadBatch, domain.RunStats, error) {
	var stats domain.RunStats
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, stats, err
	}
	batch, err := source.Read(ctx, req)
	if err != nil {
		return ports.ReadBatch{}, stats, err
	}
	stats, err = h.commitBatch(ctx, source.Descriptor(), conn, batch)
	if err != nil {
		return ports.ReadBatch{}, stats, err
	}
	return batch, stats, nil
}

// Prepare validates one batch without persisting anything: every item must
// carry the connection ID, bodies are truncated to
// domain.MaxItemBodyBytes, ObservedAt defaults to the host clock, Hash is
// set via WithHash and the item must Validate; every checkpoint must
// Validate and, when the descriptor lists Streams, belong to that set.
func (h *Host) Prepare(descriptor domain.SourceDescriptor, conn domain.Connection, batch ports.ReadBatch) ([]domain.Item, []domain.Checkpoint, error) {
	items := make([]domain.Item, 0, len(batch.Items))
	for index, item := range batch.Items {
		if item.Ref.ConnectionID != conn.ID {
			return nil, nil, fmt.Errorf("item %d: connectionId %q does not match connection %q",
				index, item.Ref.ConnectionID, conn.ID)
		}
		if item.ObservedAt.IsZero() {
			item.ObservedAt = h.now()
		}
		if len(item.Body) > domain.MaxItemBodyBytes {
			item.Body = truncateUTF8(item.Body, domain.MaxItemBodyBytes)
		}
		item = item.WithHash()
		if err := item.Validate(); err != nil {
			return nil, nil, fmt.Errorf("item %d (%s): invalid: %w", index, item.Ref.ExternalID, err)
		}
		items = append(items, item)
	}
	checkpoints := make([]domain.Checkpoint, 0, len(batch.Checkpoints))
	for index, checkpoint := range batch.Checkpoints {
		if err := checkpoint.Validate(); err != nil {
			return nil, nil, fmt.Errorf("checkpoint %d (stream %q): invalid: %w", index, checkpoint.Stream, err)
		}
		if len(descriptor.Streams) > 0 && !declaredStream(descriptor.Streams, checkpoint.Stream) {
			return nil, nil, fmt.Errorf("checkpoint %d: stream %q is not declared by the source descriptor",
				index, checkpoint.Stream)
		}
		checkpoints = append(checkpoints, checkpoint.Clone())
	}
	return items, checkpoints, nil
}

// Commit persists prepared items and then their checkpoints: Apply first,
// Put only after Apply succeeds, so a crash between the two replays the
// batch from the old checkpoints.
func (h *Host) Commit(ctx context.Context, conn domain.Connection, items []domain.Item, checkpoints []domain.Checkpoint) (domain.RunStats, error) {
	var stats domain.RunStats
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	outcomes, err := h.Items.Apply(ctx, items)
	if err != nil {
		return stats, err
	}
	if len(outcomes) != len(items) {
		return stats, fmt.Errorf("store applied %d of %d items", len(outcomes), len(items))
	}
	for index, item := range items {
		stats.Seen++
		if item.Deleted {
			stats.Deleted++
			continue
		}
		if outcomes[index] == ports.UpsertUnchanged {
			stats.Unchanged++
		} else {
			stats.Upserted++
		}
	}
	stats.Batches = 1
	for _, checkpoint := range checkpoints {
		if err := h.Checkpoints.Put(ctx, conn.ID, checkpoint); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// commitBatch runs Prepare then Commit: the batch is discarded wholesale
// when any step fails, so no checkpoint ever covers unstored items.
func (h *Host) commitBatch(ctx context.Context, descriptor domain.SourceDescriptor, conn domain.Connection, batch ports.ReadBatch) (domain.RunStats, error) {
	items, checkpoints, err := h.Prepare(descriptor, conn, batch)
	if err != nil {
		return domain.RunStats{}, err
	}
	return h.Commit(ctx, conn, items, checkpoints)
}

func declaredStream(streams []string, stream string) bool {
	for _, declared := range streams {
		if strings.EqualFold(declared, stream) {
			return true
		}
	}
	return false
}

// truncateUTF8 cuts body to at most max bytes without splitting a rune.
func truncateUTF8(body string, max int) string {
	if len(body) <= max {
		return body
	}
	body = body[:max]
	for len(body) > 0 && !utf8.ValidString(body) {
		body = body[:len(body)-1]
	}
	return body
}

func cloneItem(item domain.Item) domain.Item {
	item.Labels = append([]string(nil), item.Labels...)
	item.ACL = append([]string(nil), item.ACL...)
	return item
}
