package serverstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// CheckpointStore keeps per-(connection, stream) checkpoints in
// connector_checkpoints (migration 0009). Put is one upsert statement, so a
// reader sees the old or the new row, never a mix. Streams are compared
// byte-for-byte ("C" collation): "INBOX" and "inbox" are distinct.
type CheckpointStore struct {
	pool *pgxpool.Pool
}

var _ ports.CheckpointStore = (*CheckpointStore)(nil)

// Checkpoints returns the checkpoint store backed by this server database.
func (s *Store) Checkpoints() *CheckpointStore { return &CheckpointStore{pool: s.pg.Pool()} }

// Get returns domain.ErrNotFound when the stream has no checkpoint.
func (s *CheckpointStore) Get(ctx context.Context, connectionID, stream string) (domain.Checkpoint, error) {
	if err := validateCheckpointKey(connectionID, stream); err != nil {
		return domain.Checkpoint{}, err
	}
	var checkpoint domain.Checkpoint
	err := s.pool.QueryRow(ctx,
		`SELECT stream, version, data FROM connector_checkpoints WHERE connection_id = $1 AND stream = $2`,
		connectionID, stream).Scan(&checkpoint.Stream, &checkpoint.Version, &checkpoint.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Checkpoint{}, fmt.Errorf("serverstore: checkpoint %s/%s: %w", connectionID, stream, domain.ErrNotFound)
	}
	if err != nil {
		return domain.Checkpoint{}, itemError("get checkpoint", err)
	}
	return checkpoint, nil
}

// List returns every checkpoint of a connection ordered by stream.
func (s *CheckpointStore) List(ctx context.Context, connectionID string) ([]domain.Checkpoint, error) {
	if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT stream, version, data FROM connector_checkpoints WHERE connection_id = $1 ORDER BY stream`,
		connectionID)
	if err != nil {
		return nil, itemError("list checkpoints", err)
	}
	defer rows.Close()
	checkpoints := []domain.Checkpoint{}
	for rows.Next() {
		var checkpoint domain.Checkpoint
		if err := rows.Scan(&checkpoint.Stream, &checkpoint.Version, &checkpoint.Data); err != nil {
			return nil, itemError("list checkpoints", err)
		}
		checkpoints = append(checkpoints, checkpoint)
	}
	if err := rows.Err(); err != nil {
		return nil, itemError("list checkpoints", err)
	}
	return checkpoints, nil
}

// Put replaces one checkpoint atomically; Data is stored byte-for-byte.
func (s *CheckpointStore) Put(ctx context.Context, connectionID string, checkpoint domain.Checkpoint) error {
	if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
		return err
	}
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	data := checkpoint.Data
	if data == nil {
		data = []byte{}
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO connector_checkpoints (connection_id, stream, version, data, updated_at)
		 VALUES ($1, $2, $3, $4, now())
		 ON CONFLICT (connection_id, stream) DO UPDATE SET
		     version = excluded.version, data = excluded.data, updated_at = now()`,
		connectionID, checkpoint.Stream, checkpoint.Version, data); err != nil {
		return itemError("put checkpoint", err)
	}
	return nil
}

// Delete removes one stream's checkpoint; an empty stream removes all of the
// connection's checkpoints. Missing checkpoints are not an error.
func (s *CheckpointStore) Delete(ctx context.Context, connectionID, stream string) error {
	if stream == "" {
		if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM connector_checkpoints WHERE connection_id = $1`, connectionID); err != nil {
			return itemError("delete checkpoints", err)
		}
		return nil
	}
	if err := validateCheckpointKey(connectionID, stream); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM connector_checkpoints WHERE connection_id = $1 AND stream = $2`, connectionID, stream); err != nil {
		return itemError("delete checkpoint", err)
	}
	return nil
}

func validateCheckpointKey(connectionID, stream string) error {
	if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
		return err
	}
	return domain.ValidateStream("checkpoint.stream", stream)
}
