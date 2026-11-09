package serverstore

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	defaultItemPageSize = 100
	maxItemPageSize     = 1000
	itemCursorPrefix    = "i1:"
)

// ItemStore is the server raw zone (ports.ItemStore) over the
// connector_items table (migration 0009). Apply runs in one transaction, so a
// batch is stored all-or-nothing. Every statement is parameterized.
//
// Times are stored as timestamptz, which keeps microseconds: an item with
// sub-microsecond provider times reads back truncated, but its stored Hash is
// the host's original, so replaying it is still UpsertUnchanged.
type ItemStore struct {
	pool *pgxpool.Pool
}

var _ ports.ItemStore = (*ItemStore)(nil)

// Items returns the item store backed by this server database.
func (s *Store) Items() *ItemStore { return &ItemStore{pool: s.pg.Pool()} }

// itemQuerier is satisfied by a pool and a transaction.
type itemQuerier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const itemColumns = `connection_id, external_id, kind, title, body, url, author, labels, acl,
	project_id, created_at, updated_at, observed_at, hash, deleted`

const upsertItemSQL = `INSERT INTO connector_items
	(connection_id, item_key, external_id, kind, title, body, url, author, labels, acl,
	 project_id, created_at, updated_at, observed_at, hash, deleted, stored_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, now())
ON CONFLICT (connection_id, item_key) DO UPDATE SET
	external_id = excluded.external_id, kind = excluded.kind, title = excluded.title,
	body = excluded.body, url = excluded.url, author = excluded.author,
	labels = excluded.labels, acl = excluded.acl, project_id = excluded.project_id,
	created_at = excluded.created_at, updated_at = excluded.updated_at,
	observed_at = excluded.observed_at, hash = excluded.hash, deleted = excluded.deleted,
	stored_at = now()`

const lockItemSQL = `SELECT deleted, hash FROM connector_items
WHERE connection_id = $1 AND item_key = $2 FOR UPDATE`

// Upsert stores a live item; see ports.ItemStore. A missing Hash is computed.
func (s *ItemStore) Upsert(ctx context.Context, item domain.Item) (ports.UpsertOutcome, error) {
	if item.Deleted {
		return "", domain.NewValidationError("item.deleted", "use Delete or Apply for tombstones")
	}
	prepared, err := prepareServerItem(item)
	if err != nil {
		return "", err
	}
	var outcome ports.UpsertOutcome
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var applyErr error
		outcome, applyErr = upsertItemTx(ctx, tx, prepared)
		return applyErr
	})
	return outcome, err
}

// Delete replaces the item with a tombstone observed at at; an existing
// tombstone is left untouched. It reports whether a live item existed.
func (s *ItemStore) Delete(ctx context.Context, ref domain.ItemRef, at time.Time) (bool, error) {
	if err := ref.Validate(); err != nil {
		return false, err
	}
	var existed bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var deleteErr error
		existed, deleteErr = deleteItemTx(ctx, tx, ref, at)
		return deleteErr
	})
	return existed, err
}

// Get returns the item or tombstone for ref, or domain.ErrNotFound.
func (s *ItemStore) Get(ctx context.Context, ref domain.ItemRef) (domain.Item, error) {
	if err := ref.Validate(); err != nil {
		return domain.Item{}, err
	}
	item, err := scanItem(s.pool.QueryRow(ctx,
		`SELECT `+itemColumns+` FROM connector_items WHERE connection_id = $1 AND item_key = $2`,
		ref.ConnectionID, ref.Key()))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Item{}, fmt.Errorf("serverstore: item %q: %w", ref.Key(), domain.ErrNotFound)
	}
	if err != nil {
		return domain.Item{}, itemError("get item", err)
	}
	return item, nil
}

// List pages one connection's items ordered by key; the cursor is the last
// returned key, so pages are stable under concurrent writes.
func (s *ItemStore) List(ctx context.Context, connectionID string, opts ports.ItemListOptions) (ports.ItemPage, error) {
	if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
		return ports.ItemPage{}, err
	}
	limit, err := itemPageLimit(opts.Limit)
	if err != nil {
		return ports.ItemPage{}, err
	}
	after, err := decodeItemCursor(connectionID, opts.Cursor)
	if err != nil {
		return ports.ItemPage{}, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+itemColumns+` FROM connector_items
		 WHERE connection_id = $1 AND item_key > $2 AND ($3 OR NOT deleted)
		 ORDER BY item_key LIMIT $4`,
		connectionID, after, opts.IncludeDeleted, limit+1)
	if err != nil {
		return ports.ItemPage{}, itemError("list items", err)
	}
	defer rows.Close()
	page := ports.ItemPage{Items: []domain.Item{}}
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return ports.ItemPage{}, itemError("list items", err)
		}
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return ports.ItemPage{}, itemError("list items", err)
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.NextCursor = encodeItemCursor(page.Items[limit-1].Key())
	}
	return page, nil
}

// Purge removes every item and tombstone of a connection.
func (s *ItemStore) Purge(ctx context.Context, connectionID string) error {
	if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM connector_items WHERE connection_id = $1`, connectionID); err != nil {
		return itemError("purge items", err)
	}
	return nil
}

// Apply stores a batch in one transaction, in order. Outcomes align with
// items; a tombstone reports UpsertUpdated when it replaced a live item and
// UpsertUnchanged otherwise (the same convention as the filesystem store).
func (s *ItemStore) Apply(ctx context.Context, items []domain.Item) ([]ports.UpsertOutcome, error) {
	prepared := make([]domain.Item, len(items))
	for index, item := range items {
		if item.Deleted {
			if err := item.Ref.Validate(); err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			prepared[index] = item
			continue
		}
		ready, err := prepareServerItem(item)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", index, err)
		}
		prepared[index] = ready
	}
	var outcomes []ports.UpsertOutcome
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		outcomes = make([]ports.UpsertOutcome, 0, len(prepared))
		for _, item := range prepared {
			outcome, err := applyItemTx(ctx, tx, item)
			if err != nil {
				return err
			}
			outcomes = append(outcomes, outcome)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcomes, nil
}

func applyItemTx(ctx context.Context, tx pgx.Tx, item domain.Item) (ports.UpsertOutcome, error) {
	if !item.Deleted {
		return upsertItemTx(ctx, tx, item)
	}
	existed, err := deleteItemTx(ctx, tx, item.Ref, item.ObservedAt)
	if err != nil {
		return "", err
	}
	if existed {
		return ports.UpsertUpdated, nil
	}
	return ports.UpsertUnchanged, nil
}

func upsertItemTx(ctx context.Context, tx itemQuerier, item domain.Item) (ports.UpsertOutcome, error) {
	present, deleted, hash, err := lockItem(ctx, tx, item.Ref)
	if err != nil {
		return "", err
	}
	if present && !deleted && hash == item.Hash {
		return ports.UpsertUnchanged, nil
	}
	if err := writeItem(ctx, tx, item); err != nil {
		return "", err
	}
	if present && !deleted {
		return ports.UpsertUpdated, nil
	}
	return ports.UpsertCreated, nil
}

func deleteItemTx(ctx context.Context, tx itemQuerier, ref domain.ItemRef, at time.Time) (bool, error) {
	present, deleted, _, err := lockItem(ctx, tx, ref)
	if err != nil {
		return false, err
	}
	if present && deleted {
		return false, nil
	}
	tombstone := domain.Item{Ref: ref, Deleted: true, ObservedAt: at.UTC()}.WithHash()
	if err := writeItem(ctx, tx, tombstone); err != nil {
		return false, err
	}
	return present, nil
}

func lockItem(ctx context.Context, tx itemQuerier, ref domain.ItemRef) (bool, bool, string, error) {
	var deleted bool
	var hash string
	err := tx.QueryRow(ctx, lockItemSQL, ref.ConnectionID, ref.Key()).Scan(&deleted, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, "", nil
	}
	if err != nil {
		return false, false, "", itemError("read item", err)
	}
	return true, deleted, hash, nil
}

func writeItem(ctx context.Context, tx itemQuerier, item domain.Item) error {
	_, err := tx.Exec(ctx, upsertItemSQL,
		item.Ref.ConnectionID, item.Key(), item.Ref.ExternalID, string(item.Kind),
		item.Title, item.Body, item.URL, item.Author, nonNilTags(item.Labels), nonNilTags(item.ACL),
		strings.TrimSpace(item.ProjectID), optionalTime(item.CreatedAt), optionalTime(item.UpdatedAt),
		optionalTime(item.ObservedAt), item.Hash, item.Deleted,
	)
	if err != nil {
		return itemError("store item", err)
	}
	return nil
}

func scanItem(row interface{ Scan(dest ...any) error }) (domain.Item, error) {
	var (
		item                           domain.Item
		kind                           string
		createdAt, updatedAt, observed *time.Time
	)
	if err := row.Scan(
		&item.Ref.ConnectionID, &item.Ref.ExternalID, &kind, &item.Title, &item.Body, &item.URL,
		&item.Author, &item.Labels, &item.ACL, &item.ProjectID, &createdAt, &updatedAt, &observed,
		&item.Hash, &item.Deleted,
	); err != nil {
		return domain.Item{}, err
	}
	item.Kind = domain.ItemKind(kind)
	item.CreatedAt = utcOrZero(createdAt)
	item.UpdatedAt = utcOrZero(updatedAt)
	item.ObservedAt = utcOrZero(observed)
	if len(item.Labels) == 0 {
		item.Labels = nil
	}
	if len(item.ACL) == 0 {
		item.ACL = nil
	}
	return item, nil
}

func prepareServerItem(item domain.Item) (domain.Item, error) {
	if item.Hash == "" {
		item = item.WithHash()
	}
	if err := item.Validate(); err != nil {
		return domain.Item{}, err
	}
	return item, nil
}

// inTx runs fn in a transaction, rolling back on any error.
func (s *ItemStore) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return itemError("begin transaction", err)
	}
	if err := fn(tx); err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return itemError("commit transaction", err)
	}
	return nil
}

// itemError maps check and not-null violations to validation errors and
// wraps everything else with the operation name. No values are included.
func itemError(op string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23514", "23502", "22P02", "22001":
			return fmt.Errorf("serverstore: %s: %w (%s)", op, domain.ErrValidation, pgErr.ConstraintName)
		}
	}
	return fmt.Errorf("serverstore: %s: %w", op, err)
}

func nonNilTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}

func utcOrZero(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}

func itemPageLimit(limit int) (int, error) {
	switch {
	case limit < 0:
		return 0, domain.NewValidationError("items.limit", "must not be negative")
	case limit == 0:
		return defaultItemPageSize, nil
	case limit > maxItemPageSize:
		return maxItemPageSize, nil
	default:
		return limit, nil
	}
}

func encodeItemCursor(key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(itemCursorPrefix + key))
}

// decodeItemCursor returns the key after which listing resumes ("" starts).
func decodeItemCursor(connectionID, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", domain.NewValidationError("items.cursor", "is not a valid cursor")
	}
	key, ok := strings.CutPrefix(string(raw), itemCursorPrefix)
	if !ok || !strings.HasPrefix(key, connectionID+"/") {
		return "", domain.NewValidationError("items.cursor", "is not a valid cursor for this connection")
	}
	return key, nil
}
