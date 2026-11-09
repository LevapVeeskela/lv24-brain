package serverstore

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ItemZone exposes raw items without leaking a concrete adapter to the host.
func (s *Store) ItemZone() ports.ItemStore { return s.Items() }

// CheckpointZone exposes durable per-stream checkpoints to the host.
func (s *Store) CheckpointZone() ports.CheckpointStore { return s.Checkpoints() }

// TryLock holds a database-session advisory lock for one connection. PostgreSQL
// releases it when a server process or its database session dies.
func (s *Store) TryLock(ctx context.Context, id string) (func() error, error) {
	if err := domain.ValidateConnectionID("connection.id", id); err != nil {
		return nil, err
	}
	conn, err := s.pg.Pool().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var acquired bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(47615, hashtext($1))`, id).Scan(&acquired); err != nil {
		// An interrupted reply could have acquired the lock: never return this
		// session to the pool in an unknown state.
		raw := conn.Hijack()
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = raw.Close(closeCtx)
		return nil, err
	}
	if !acquired {
		conn.Release()
		return nil, fmt.Errorf("%w: connection busy", domain.ErrConflict)
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var unlocked bool
			releaseErr = conn.QueryRow(releaseCtx, `SELECT pg_advisory_unlock(47615, hashtext($1))`, id).Scan(&unlocked)
			if releaseErr != nil || !unlocked {
				raw := conn.Hijack()
				_ = raw.Close(releaseCtx)
				if releaseErr == nil {
					releaseErr = fmt.Errorf("connection lock was lost")
				}
				return
			}
			conn.Release()
		})
		return releaseErr
	}, nil
}
