package ports

import (
	"context"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Ingestion contracts (contracts-v1): item-level sources and the stores the
// host uses to persist their output. Sources only read; the host owns
// validation, policy, persistence and checkpoints.

// ReadRequest asks a source for the next batch of one connection.
type ReadRequest struct {
	// Connection is the validated connection being read.
	Connection domain.Connection
	// Checkpoints are the last persisted checkpoints keyed by stream. A
	// missing stream means "start from the beginning" (backfill). Sources
	// must not mutate the map or the Data slices.
	Checkpoints map[string]domain.Checkpoint
	// Since is an optional lower bound on provider update time. It narrows
	// what is read; it never means that unseen items were deleted.
	Since *time.Time
	// Limit caps the number of items in this batch; 0 selects the source
	// default. The host bounds the whole run separately.
	Limit int
}

// ReadBatch is one page of source output.
type ReadBatch struct {
	// Items are upserts and tombstones (Item.Deleted) in source order; for a
	// repeated Ref within one batch the last entry wins. Every Ref must carry
	// the request's Connection.ID.
	Items []domain.Item
	// Checkpoints are the next cursors for the streams this batch advanced;
	// streams not listed keep their previous checkpoint. A checkpoint must
	// only cover positions whose items are in this batch or an earlier one.
	Checkpoints []domain.Checkpoint
	// More is true when the source has more data right now; the host calls
	// Read again with the newly persisted checkpoints.
	More bool
}

// Source is an item-level, read-only integration (native API client, MCP
// client or external process).
//
// Host contract, per batch:
//  1. call Read with the persisted checkpoints;
//  2. on error discard the whole batch (items and checkpoints);
//  3. validate and persist every item (ItemStore.Upsert/Delete);
//  4. only after all items are durable, persist the batch checkpoints
//     (CheckpointStore.Put);
//  5. repeat while More and the run budget allows.
//
// A crash between 3 and 4 replays the batch from the old checkpoints. Sources
// must therefore be deterministic per checkpoint and items idempotent by Ref
// (same Ref + same content = same Hash = no-op), which makes replay harmless.
// Deletions are explicit tombstones only; the host never infers a deletion
// from an item missing in a filtered or partial read.
type Source interface {
	// Descriptor returns the static provider description.
	Descriptor() domain.SourceDescriptor
	// Check verifies configuration and credentials with the cheapest call
	// the provider allows. It reads no items and writes nothing.
	Check(ctx context.Context) error
	// Read returns the next batch. It must honor ctx cancellation.
	Read(ctx context.Context, req ReadRequest) (ReadBatch, error)
}

// UpsertOutcome says what an ItemStore.Upsert changed.
type UpsertOutcome string

const (
	UpsertCreated   UpsertOutcome = "created"
	UpsertUpdated   UpsertOutcome = "updated"
	UpsertUnchanged UpsertOutcome = "unchanged"
)

// ItemListOptions pages one connection's items.
type ItemListOptions struct {
	// Cursor is an opaque NextCursor from a previous page; empty starts.
	Cursor string
	// Limit caps the page; 0 selects the adapter default.
	Limit int
	// IncludeDeleted also returns tombstones.
	IncludeDeleted bool
}

// ItemPage is one page of items ordered by Item.Key().
type ItemPage struct {
	Items []domain.Item
	// NextCursor is empty on the last page.
	NextCursor string
}

// ItemStore is the raw zone: normalized external items keyed by
// Item.Key(). It never holds curated records.
type ItemStore interface {
	// Upsert stores item (Hash already set by the host). Equal Hash on an
	// existing live item is UpsertUnchanged and does not rewrite it. Upsert
	// of a tombstoned key revives it.
	Upsert(ctx context.Context, item domain.Item) (UpsertOutcome, error)
	// Delete replaces the item with a tombstone observed at at. It reports
	// whether a live item existed; deleting an unknown ref records the
	// tombstone and returns false.
	Delete(ctx context.Context, ref domain.ItemRef, at time.Time) (bool, error)
	// Get returns domain.ErrNotFound for an unknown ref; tombstones are
	// returned with Deleted set.
	Get(ctx context.Context, ref domain.ItemRef) (domain.Item, error)
	// List pages the items of one connection.
	List(ctx context.Context, connectionID string, opts ItemListOptions) (ItemPage, error)
	// Purge removes every item and tombstone of a connection.
	Purge(ctx context.Context, connectionID string) error
	// Apply stores one batch in order: upserts for live items, tombstones
	// for Deleted ones (at Item.ObservedAt). Transactional backends apply it
	// all-or-nothing; others apply in order and may stop midway. Either is
	// safe because the host advances checkpoints only after Apply succeeds,
	// so a partial batch is replayed. Outcomes align with items.
	Apply(ctx context.Context, items []domain.Item) ([]UpsertOutcome, error)
}

// CheckpointStore keeps opaque per-(connection, stream) checkpoints.
type CheckpointStore interface {
	// Get returns domain.ErrNotFound when the stream has no checkpoint.
	Get(ctx context.Context, connectionID, stream string) (domain.Checkpoint, error)
	// List returns every checkpoint of a connection ordered by stream.
	List(ctx context.Context, connectionID string) ([]domain.Checkpoint, error)
	// Put replaces one checkpoint atomically: after a crash the old or the
	// new value is visible, never a mix. Data is stored byte-for-byte.
	Put(ctx context.Context, connectionID string, checkpoint domain.Checkpoint) error
	// Delete removes one stream's checkpoint (forces a re-read); an empty
	// stream removes all of the connection's checkpoints.
	Delete(ctx context.Context, connectionID, stream string) error
}

// ConnectionStore persists connections (never secrets).
type ConnectionStore interface {
	// List returns connections ordered by ID.
	List(ctx context.Context) ([]domain.Connection, error)
	// Get returns domain.ErrNotFound for an unknown ID.
	Get(ctx context.Context, id string) (domain.Connection, error)
	// Create returns domain.ErrConflict when the ID or the
	// Connection.Identity() is already used in this data directory.
	Create(ctx context.Context, connection domain.Connection) error
	// Update replaces a connection. Identity (provider, base URL, account)
	// is immutable: changing it returns domain.ErrConflict; create a new
	// connection instead. Unknown IDs return domain.ErrNotFound.
	Update(ctx context.Context, connection domain.Connection) error
	// Delete removes a connection; callers purge its items, checkpoints and
	// credentials separately. Unknown IDs return domain.ErrNotFound.
	Delete(ctx context.Context, id string) error
}

// ConnectionLocker serializes runs of one connection across processes (agent,
// CLI, MCP server). TryLock returns domain.ErrConflict while another holder
// runs the connection; release is idempotent. A crashed holder's lock must
// expire or be recoverable (stale-lock detection is the adapter's job).
type ConnectionLocker interface {
	TryLock(ctx context.Context, connectionID string) (release func() error, err error)
}

// CredentialStore also writes connection credentials (token entry in the UI,
// OAuth token rotation). Writers must persist a rotated refresh token before
// using the new access token.
type CredentialStore interface {
	CredentialProvider
	Set(ctx context.Context, connectionID, field, value string) error
	Delete(ctx context.Context, connectionID, field string) error
}

// CredentialProvider resolves secret connection fields from the credential
// store under domain.CredentialKey(connectionID, field), i.e.
// conn/<id>/<field>. Missing values return domain.ErrNotFound. Values must
// never be logged or stored on items, runs or connections.
type CredentialProvider interface {
	Get(ctx context.Context, connectionID, field string) (string, error)
}
