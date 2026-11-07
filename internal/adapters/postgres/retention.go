package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// retentionLockSQL serializes the retention sweep across replicas. The
	// lock is session-scoped and held on one pooled connection for the whole
	// run, so a second replica skips its tick instead of waiting.
	retentionLockSQL   = `SELECT pg_try_advisory_lock(hashtext('lv24_brain_retention'))`
	retentionUnlockSQL = `SELECT pg_advisory_unlock(hashtext('lv24_brain_retention'))`

	// DefaultRetentionBatchSize bounds rows deleted by one statement so a
	// sweep never holds long row locks.
	DefaultRetentionBatchSize = 1000
	// retentionMaxBatches caps one sweep per table per run. Remaining rows are
	// picked up by the next scheduled run.
	retentionMaxBatches = 500
)

// ErrRetentionBusy reports that another replica holds the retention lock.
var ErrRetentionBusy = errors.New("postgres: retention is already running elsewhere")

// RetentionPolicy holds the absolute cutoffs for one sweep. A zero cutoff
// disables that sweep. Rows strictly older than a cutoff are deleted.
type RetentionPolicy struct {
	// Audit applies to audit_events.created_at.
	Audit time.Time
	// Revisions applies to revisions.created_at, and only to revisions that no
	// audit event and no entity's current revision still references.
	Revisions time.Time
	// Revocations applies to auth_revocations.revoked_at. Callers must pass a
	// cutoff at least one refresh-token lifetime in the past, so no token that
	// the tombstone guarded can still be valid.
	Revocations time.Time
	// RefreshTokens applies to refresh_tokens.expires_at. Expired rows are
	// unusable, and revoked rows are removed when they expire.
	RefreshTokens time.Time
	// APITokens applies to api_tokens.expires_at (expired tokens only; tokens
	// without an expiry are never removed).
	APITokens time.Time
	// BatchSize bounds rows per DELETE. Zero means DefaultRetentionBatchSize.
	BatchSize int
}

// RetentionResult counts the rows removed by one sweep.
type RetentionResult struct {
	AuditEvents   int64
	Revisions     int64
	Revocations   int64
	RefreshTokens int64
	APITokens     int64
}

// PruneRetained deletes data past its retention cutoff in bounded batches. It
// takes a PostgreSQL advisory lock first and returns ErrRetentionBusy when
// another replica holds it. Sweeps run in dependency order: audit events go
// first so the revisions they reference can be released in the same run.
func (s *Store) PruneRetained(ctx context.Context, policy RetentionPolicy) (RetentionResult, error) {
	if s == nil || s.pool == nil {
		return RetentionResult{}, errors.New("postgres: store is not configured")
	}
	batch := policy.BatchSize
	if batch <= 0 {
		batch = DefaultRetentionBatchSize
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return RetentionResult{}, mapPgError("acquire retention connection", err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, retentionLockSQL).Scan(&locked); err != nil {
		return RetentionResult{}, mapPgError("acquire retention lock", err)
	}
	if !locked {
		return RetentionResult{}, ErrRetentionBusy
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, retentionUnlockSQL)
	}()

	var result RetentionResult
	sweeps := []struct {
		cutoff time.Time
		sql    string
		target *int64
	}{
		{policy.Audit, deleteAuditSQL, &result.AuditEvents},
		{policy.Revisions, deleteRevisionsSQL, &result.Revisions},
		{policy.Revocations, deleteRevocationsSQL, &result.Revocations},
		{policy.RefreshTokens, deleteRefreshTokensSQL, &result.RefreshTokens},
		{policy.APITokens, deleteAPITokensSQL, &result.APITokens},
	}
	for _, sweep := range sweeps {
		if sweep.cutoff.IsZero() {
			continue
		}
		deleted, err := deleteInBatches(ctx, conn, sweep.sql, sweep.cutoff.UTC(), batch)
		*sweep.target = deleted
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// deleteInBatches repeats one bounded DELETE until it removes fewer rows than
// the batch size or the per-run batch cap is reached. Each statement commits
// on its own, so locks are released between batches.
func deleteInBatches(ctx context.Context, conn *pgxpool.Conn, sql string, cutoff time.Time, batch int) (int64, error) {
	var total int64
	for i := 0; i < retentionMaxBatches; i++ {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		tag, err := conn.Exec(ctx, sql, cutoff, batch)
		if err != nil {
			return total, mapPgError("delete expired rows", err)
		}
		n := tag.RowsAffected()
		total += n
		if n < int64(batch) {
			return total, nil
		}
	}
	// Batch cap reached: the remaining rows are left for the next run.
	return total, nil
}

const deleteAuditSQL = `
DELETE FROM audit_events
WHERE id IN (
    SELECT id FROM audit_events
    WHERE created_at < $1
    ORDER BY created_at, id
    LIMIT $2
)`

// deleteRevisionsSQL keeps any revision that an audit event or an entity's
// current revision still references, so the foreign keys stay satisfied.
const deleteRevisionsSQL = `
DELETE FROM revisions
WHERE id IN (
    SELECT r.id FROM revisions r
    WHERE r.created_at < $1
      AND NOT EXISTS (SELECT 1 FROM audit_events x WHERE x.revision = r.id)
      AND NOT EXISTS (SELECT 1 FROM users x WHERE x.revision = r.id)
      AND NOT EXISTS (SELECT 1 FROM projects x WHERE x.revision = r.id)
      AND NOT EXISTS (SELECT 1 FROM project_state x WHERE x.revision = r.id)
      AND NOT EXISTS (SELECT 1 FROM notes x WHERE x.revision = r.id)
      AND NOT EXISTS (SELECT 1 FROM decisions x WHERE x.revision = r.id)
      AND NOT EXISTS (SELECT 1 FROM attempts x WHERE x.revision = r.id)
      AND NOT EXISTS (SELECT 1 FROM handoffs x WHERE x.revision = r.id)
      AND NOT EXISTS (SELECT 1 FROM connector_runs x WHERE x.revision = r.id)
    ORDER BY r.id
    LIMIT $2
)`

const deleteRevocationsSQL = `
DELETE FROM auth_revocations
WHERE (scope_kind, scope_id) IN (
    SELECT scope_kind, scope_id FROM auth_revocations
    WHERE revoked_at < $1
    LIMIT $2
)`

const deleteRefreshTokensSQL = `
DELETE FROM refresh_tokens
WHERE token_hash IN (
    SELECT token_hash FROM refresh_tokens
    WHERE expires_at < $1
    LIMIT $2
)`

const deleteAPITokensSQL = `
DELETE FROM api_tokens
WHERE id IN (
    SELECT id FROM api_tokens
    WHERE expires_at IS NOT NULL AND expires_at < $1
    LIMIT $2
)`
