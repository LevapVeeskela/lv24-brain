package postgres

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const refreshTokenColumns = `token_hash, session_id, family_id, user_id, issued_at, expires_at, used_at, revoked_at`

// refreshScopeLockSQL serializes refresh-token rotation and revocation for one
// session or family. The lock is transaction-scoped and released on commit or
// rollback. Revocations write a durable tombstone under the same lock, so a
// concurrent rotation cannot insert an unrevoked row behind a revocation.
const refreshScopeLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`

// CreateRefresh stores a refresh token hash. Expired records of the same
// session or family are pruned in the same transaction; unrelated scopes are
// left untouched so a rotation never takes row locks outside its own advisory
// locks (which could deadlock with unrelated revocations). A duplicate hash is
// a domain.ErrConflict. The family and session advisory locks serialize this
// insert with revocations, and a tombstone in auth_revocations makes a revoked
// family or session stay revoked even after its refresh_tokens rows expired or
// were pruned.
func (s *Store) CreateRefresh(ctx context.Context, record auth.RefreshRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if record.TokenHash == "" || record.SessionID == "" || record.UserID == "" {
		return domain.NewValidationError("refresh", "session, user and hash are required")
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := lockRefreshScopes(ctx, tx, record.FamilyID, record.SessionID); err != nil {
			return err
		}
		// The database clock is authoritative so replicas with clock skew
		// cannot prune each other's still-valid tokens. Prune only the scope
		// locked above: a global DELETE would take row locks on other
		// sessions/families and can deadlock against an unrelated revoke. Only
		// expired rows are pruned; revocation tombstones are never pruned.
		if _, err := tx.Exec(ctx, `
			DELETE FROM refresh_tokens
			WHERE expires_at < now() AND (session_id = $1 OR family_id = $2)`,
			record.SessionID, record.FamilyID); err != nil {
			return mapPgError("prune refresh tokens", err)
		}
		revoked, err := refreshRevoked(ctx, tx, record.FamilyID, record.SessionID)
		if err != nil {
			return err
		}
		if revoked {
			return auth.ErrInvalidRefresh
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO refresh_tokens (token_hash, session_id, family_id, user_id, issued_at, expires_at, used_at, revoked_at)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8
			WHERE NOT EXISTS (
				SELECT 1 FROM refresh_tokens
				WHERE (session_id = $2 OR family_id = $3) AND revoked_at IS NOT NULL
			)`,
			record.TokenHash, record.SessionID, record.FamilyID, record.UserID,
			databaseTime(record.IssuedAt), databaseTime(record.ExpiresAt),
			databaseTimePtr(record.UsedAt), databaseTimePtr(record.RevokedAt),
		)
		if err != nil {
			return mapPgError("insert refresh token", err)
		}
		if tag.RowsAffected() == 0 {
			return auth.ErrInvalidRefresh
		}
		return nil
	})
}

// ConsumeRefresh atomically marks an unused refresh token as used. A missing,
// expired or revoked token returns auth.ErrInvalidRefresh; an already used
// token returns its record together with auth.ErrRefreshReused so the caller
// can revoke the whole family. The family/session lock and tombstone checks
// keep an in-flight token from being consumed after its family was revoked.
func (s *Store) ConsumeRefresh(ctx context.Context, tokenHash string, now time.Time) (auth.RefreshRecord, error) {
	if err := ctx.Err(); err != nil {
		return auth.RefreshRecord{}, err
	}
	at := databaseTime(now)
	var result auth.RefreshRecord
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		// Read the scopes first so the advisory lock is taken before any
		// validity decision; the row is read again under the lock.
		current, err := scanRefreshToken(tx.QueryRow(ctx,
			`SELECT `+refreshTokenColumns+` FROM refresh_tokens WHERE token_hash = $1`, tokenHash))
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.ErrInvalidRefresh
		}
		if err != nil {
			return mapPgError("read refresh token", err)
		}
		if err := lockRefreshScopes(ctx, tx, current.FamilyID, current.SessionID); err != nil {
			return err
		}
		current, live, err := scanRefreshTokenState(tx.QueryRow(ctx,
			`SELECT `+refreshTokenColumns+`, expires_at > now() FROM refresh_tokens WHERE token_hash = $1`, tokenHash))
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.ErrInvalidRefresh
		}
		if err != nil {
			return mapPgError("read refresh token", err)
		}
		revoked, err := refreshRevoked(ctx, tx, current.FamilyID, current.SessionID)
		if err != nil {
			return err
		}
		if revoked || current.RevokedAt != nil || !live {
			result = current
			return auth.ErrInvalidRefresh
		}
		if current.UsedAt != nil {
			result = current
			return auth.ErrRefreshReused
		}
		tag, err := tx.Exec(ctx, `
			UPDATE refresh_tokens SET used_at = $2
			WHERE token_hash = $1
			  AND used_at IS NULL
			  AND revoked_at IS NULL
			  AND expires_at > now()`, tokenHash, at)
		if err != nil {
			return mapPgError("consume refresh token", err)
		}
		if tag.RowsAffected() == 0 {
			result = current
			return auth.ErrInvalidRefresh
		}
		result = current
		return nil
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

// RevokeSession revokes every token issued for one session and records a
// durable session tombstone. The id is trimmed so the tombstone and the
// revoked rows always use the same canonical value.
func (s *Store) RevokeSession(ctx context.Context, sessionID string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := lockRefreshScopes(ctx, tx, sessionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO auth_revocations (scope_kind, scope_id, revoked_at)
			VALUES ('session', $1, $2)
			ON CONFLICT (scope_kind, scope_id) DO NOTHING`,
			sessionID, databaseTime(now)); err != nil {
			return mapPgError("record refresh session revocation", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, $2) WHERE session_id = $1`,
			sessionID, databaseTime(now)); err != nil {
			return mapPgError("revoke refresh session", err)
		}
		return nil
	})
}

// RevokeFamily revokes every rotated token of one login family and records a
// durable family tombstone. The id is trimmed so the tombstone and the revoked
// rows always use the same canonical value.
func (s *Store) RevokeFamily(ctx context.Context, familyID string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	familyID = strings.TrimSpace(familyID)
	if familyID == "" {
		return nil
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		if err := lockRefreshScopes(ctx, tx, familyID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO auth_revocations (scope_kind, scope_id, revoked_at)
			VALUES ('family', $1, $2)
			ON CONFLICT (scope_kind, scope_id) DO NOTHING`,
			familyID, databaseTime(now)); err != nil {
			return mapPgError("record refresh family revocation", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, $2) WHERE family_id = $1`,
			familyID, databaseTime(now)); err != nil {
			return mapPgError("revoke refresh family", err)
		}
		return nil
	})
}

// RevokeUserSessions revokes every login family of one user, except
// exceptFamilyID when it is non-empty, and records a durable family tombstone
// for each. It returns the number of families it covered. The families are
// locked in the same order as single-family revocations, and each tombstone
// stops a concurrent rotation from inserting a row behind the revocation. A
// login that starts after the family list is read is not covered.
func (s *Store) RevokeUserSessions(ctx context.Context, userID, exceptFamilyID string, now time.Time) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	userID = strings.TrimSpace(userID)
	exceptFamilyID = strings.TrimSpace(exceptFamilyID)
	if userID == "" {
		return 0, domain.NewValidationError("user.id", "is required")
	}
	at := databaseTime(now)
	var covered int
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		families, err := userFamilies(ctx, tx, userID, exceptFamilyID)
		if err != nil {
			return err
		}
		if err := lockRefreshScopes(ctx, tx, families...); err != nil {
			return err
		}
		for _, family := range families {
			if _, err := tx.Exec(ctx, `
				INSERT INTO auth_revocations (scope_kind, scope_id, revoked_at)
				VALUES ('family', $1, $2)
				ON CONFLICT (scope_kind, scope_id) DO NOTHING`,
				family, at); err != nil {
				return mapPgError("record user family revocation", err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE refresh_tokens SET revoked_at = COALESCE(revoked_at, $2) WHERE family_id = $1`,
				family, at); err != nil {
				return mapPgError("revoke user family", err)
			}
		}
		covered = len(families)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return covered, nil
}

// userFamilies lists the distinct login families of one user, skipping
// exceptFamilyID when it is set.
func userFamilies(ctx context.Context, tx pgx.Tx, userID, exceptFamilyID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT family_id FROM refresh_tokens
		WHERE user_id = $1 AND family_id <> $2
		ORDER BY family_id`, userID, exceptFamilyID)
	if err != nil {
		return nil, mapPgError("list user families", err)
	}
	defer rows.Close()
	var families []string
	for rows.Next() {
		var family string
		if err := rows.Scan(&family); err != nil {
			return nil, mapPgError("scan user family", err)
		}
		families = append(families, family)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgError("read user families", err)
	}
	return families, nil
}

// lockRefreshScopes takes transaction-scoped advisory locks for the given
// scope ids in a stable order so concurrent rotations and revocations cannot
// deadlock. Empty ids are skipped and duplicates are locked once.
func lockRefreshScopes(ctx context.Context, tx pgx.Tx, scopes ...string) error {
	keys := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		if scope != "" {
			keys = append(keys, scope)
		}
	}
	sort.Strings(keys)
	previous := ""
	for _, key := range keys {
		if key == previous {
			continue
		}
		if _, err := tx.Exec(ctx, refreshScopeLockSQL, key); err != nil {
			return mapPgError("lock refresh scope", err)
		}
		previous = key
	}
	return nil
}

// refreshRevoked reports whether a family or session has a revocation
// tombstone. Tombstones never expire.
func refreshRevoked(ctx context.Context, tx pgx.Tx, familyID, sessionID string) (bool, error) {
	var revoked bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM auth_revocations
			WHERE (scope_kind = 'family' AND scope_id = $1)
			   OR (scope_kind = 'session' AND scope_id = $2)
		)`, familyID, sessionID).Scan(&revoked)
	if err != nil {
		return false, mapPgError("check refresh revocation", err)
	}
	return revoked, nil
}

func scanRefreshToken(row rowScanner) (auth.RefreshRecord, error) {
	var record auth.RefreshRecord
	if err := row.Scan(
		&record.TokenHash, &record.SessionID, &record.FamilyID, &record.UserID,
		&record.IssuedAt, &record.ExpiresAt, &record.UsedAt, &record.RevokedAt,
	); err != nil {
		return auth.RefreshRecord{}, err
	}
	return record, nil
}

// scanRefreshTokenState scans a refresh record plus the database's own
// expires_at > now() verdict.
func scanRefreshTokenState(row rowScanner) (auth.RefreshRecord, bool, error) {
	var record auth.RefreshRecord
	var live bool
	if err := row.Scan(
		&record.TokenHash, &record.SessionID, &record.FamilyID, &record.UserID,
		&record.IssuedAt, &record.ExpiresAt, &record.UsedAt, &record.RevokedAt,
		&live,
	); err != nil {
		return auth.RefreshRecord{}, false, err
	}
	return record, live, nil
}

func databaseTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	normalized := databaseTime(*value)
	return &normalized
}

// IsRevoked reports whether the family or session of an access token has a
// revocation tombstone (auth.RevocationChecker). Empty ids never match.
func (s *Store) IsRevoked(ctx context.Context, familyID, sessionID string) (bool, error) {
	familyID, sessionID = strings.TrimSpace(familyID), strings.TrimSpace(sessionID)
	if familyID == "" && sessionID == "" {
		return false, nil
	}
	var revoked bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM auth_revocations
			WHERE (scope_kind = 'family' AND scope_id = $1 AND $1 <> '')
			   OR (scope_kind = 'session' AND scope_id = $2 AND $2 <> '')
		)`, familyID, sessionID).Scan(&revoked)
	if err != nil {
		return false, mapPgError("check access token revocation", err)
	}
	return revoked, nil
}
