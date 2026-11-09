package serverstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ItemZone exposes raw items without leaking a concrete adapter to the host.
func (s *Store) ItemZone() ports.ItemStore { return s.Items() }

// CheckpointZone exposes durable per-stream checkpoints to the host.
func (s *Store) CheckpointZone() ports.CheckpointStore { return s.Checkpoints() }

// TryLock holds a database-session advisory lock for one connection. PostgreSQL
// releases it when a server process or its database session dies. The key uses
// 64-bit hashtextextended (not 32-bit hashtext) to make an unrelated connection
// ID collision negligible.
//
// The lock uses a dedicated pgx connection, not a pgxpool lease: the ingestion
// host lists checkpoints and writes items through the pool while the lock is
// held, so a pooled lock connection could deadlock a small
// BRAIN_DATABASE_MAX_CONNS pool (even MaxConns=1) until the run timeout.
func (s *Store) TryLock(ctx context.Context, id string) (func() error, error) {
	if err := domain.ValidateConnectionID("connection.id", id); err != nil {
		return nil, err
	}
	config := s.pg.Pool().Config().ConnConfig.Copy()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	closeConn := func(closeCtx context.Context) error { return conn.Close(closeCtx) }
	var acquired bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 47615))`, id).Scan(&acquired); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = closeConn(closeCtx)
		return nil, err
	}
	if !acquired {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = closeConn(closeCtx)
		return nil, fmt.Errorf("%w: connection busy", domain.ErrConflict)
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			releaseErr = conn.QueryRow(releaseCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 47615))`, id).Scan(&unlocked)
			if releaseErr != nil || !unlocked {
				_ = closeConn(releaseCtx)
				if releaseErr == nil {
					releaseErr = fmt.Errorf("connection lock was lost")
				}
				return
			}
			if err := closeConn(releaseCtx); err != nil && !errors.Is(err, context.Canceled) {
				releaseErr = err
			}
		})
		return releaseErr
	}, nil
}
