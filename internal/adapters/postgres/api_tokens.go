package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// APITokenRecord is a hashed API token owned by a user. It mirrors the HTTP
// record type without importing the HTTP adapter.
type APITokenRecord struct {
	ID         string
	UserID     string
	Name       string
	Role       domain.Role
	Prefix     string
	TokenHash  string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

const apiTokenColumns = `id, user_id, name, prefix, token_hash, role, created_at, expires_at, last_used_at, revoked_at`

// CreateAPIToken validates and stores a hashed API token. A user id that does
// not exist is reported as domain.ErrNotFound, matching the in-memory adapter
// behavior this replaces.
func (s *Store) CreateAPIToken(ctx context.Context, token APITokenRecord) (APITokenRecord, error) {
	if err := ctx.Err(); err != nil {
		return APITokenRecord{}, err
	}
	if strings.TrimSpace(token.UserID) == "" {
		return APITokenRecord{}, domain.NewValidationError("token.userId", "is required")
	}
	if strings.TrimSpace(token.Name) == "" {
		return APITokenRecord{}, domain.NewValidationError("token.name", "is required")
	}
	if !token.Role.Valid() {
		return APITokenRecord{}, domain.NewValidationError("token.role", "unknown role")
	}
	if token.TokenHash == "" {
		return APITokenRecord{}, domain.NewValidationError("token.hash", "is required")
	}
	if token.ID == "" {
		id, err := newID("token")
		if err != nil {
			return APITokenRecord{}, err
		}
		token.ID = id
	}
	if _, err := s.GetUser(ctx, token.UserID); err != nil {
		return APITokenRecord{}, err
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = s.nowUTC()
	}

	record, err := scanAPIToken(s.pool.QueryRow(ctx, `
		INSERT INTO api_tokens (id, user_id, name, prefix, token_hash, role, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+apiTokenColumns,
		token.ID, token.UserID, token.Name, token.Prefix, token.TokenHash, string(token.Role), token.CreatedAt,
		databaseTimePtr(token.ExpiresAt),
	))
	if err != nil {
		return APITokenRecord{}, mapPgError("insert api token", err)
	}
	return record, nil
}

// GetAPIToken loads one API token record by id.
func (s *Store) GetAPIToken(ctx context.Context, id string) (APITokenRecord, error) {
	if err := ctx.Err(); err != nil {
		return APITokenRecord{}, err
	}
	record, err := scanAPIToken(s.pool.QueryRow(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens WHERE id = $1`, id))
	if err != nil {
		return APITokenRecord{}, mapPgError("read api token", err)
	}
	return record, nil
}

// ListAPITokens returns tokens for one user, or every token when userID is
// empty, newest first.
func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]APITokenRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rows pgx.Rows
	var err error
	if strings.TrimSpace(userID) == "" {
		rows, err = s.pool.Query(ctx,
			`SELECT `+apiTokenColumns+` FROM api_tokens ORDER BY created_at DESC, id`)
	} else {
		rows, err = s.pool.Query(ctx,
			`SELECT `+apiTokenColumns+` FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC, id`, userID)
	}
	if err != nil {
		return nil, mapPgError("list api tokens", err)
	}
	defer rows.Close()

	tokens := make([]APITokenRecord, 0)
	for rows.Next() {
		record, err := scanAPIToken(rows)
		if err != nil {
			return nil, mapPgError("scan api token", err)
		}
		tokens = append(tokens, record)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgError("list api tokens", err)
	}
	return tokens, nil
}

// RevokeAPIToken marks a token unusable. Repeated revocation is a no-op.
func (s *Store) RevokeAPIToken(ctx context.Context, id string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var revoked string
	err := s.pool.QueryRow(ctx, `
		UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, $2)
		WHERE id = $1
		RETURNING id`, id, databaseTime(now)).Scan(&revoked)
	if err != nil {
		return mapPgError("revoke api token", err)
	}
	return nil
}

// FindAPITokenByHash resolves an active token and records its last use. The
// lookup and the last-used update are separate statements: a failure to record
// usage must not reject an otherwise valid token. An absent, revoked or expired
// token (expires_at <= now) is reported as domain.ErrNotFound.
func (s *Store) FindAPITokenByHash(ctx context.Context, hash string, now time.Time) (APITokenRecord, error) {
	if err := ctx.Err(); err != nil {
		return APITokenRecord{}, err
	}
	nowDB := databaseTime(now)
	record, err := scanAPIToken(s.pool.QueryRow(ctx,
		`SELECT `+apiTokenColumns+` FROM api_tokens
		WHERE token_hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $2)`, hash, nowDB))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return APITokenRecord{}, domain.ErrNotFound
		}
		return APITokenRecord{}, mapPgError("find api token", err)
	}
	usedAt := nowDB
	// Best effort: usage telemetry must never turn a valid token into a 401.
	if _, updateErr := s.pool.Exec(ctx,
		`UPDATE api_tokens SET last_used_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $2)`,
		hash, usedAt); updateErr == nil {
		record.LastUsedAt = &usedAt
	}
	return record, nil
}

func scanAPIToken(row rowScanner) (APITokenRecord, error) {
	var (
		record APITokenRecord
		role   string
	)
	err := row.Scan(
		&record.ID, &record.UserID, &record.Name, &record.Prefix, &record.TokenHash,
		&role, &record.CreatedAt, &record.ExpiresAt, &record.LastUsedAt, &record.RevokedAt,
	)
	if err != nil {
		return APITokenRecord{}, err
	}
	record.Role = domain.Role(role)
	return record, nil
}
