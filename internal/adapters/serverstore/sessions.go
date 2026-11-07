package serverstore

import (
	"context"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/postgres"
	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// This file implements the auth.SessionStore and API-token half of
// httpapi.Store on top of the durable PostgreSQL tables added by migration
// 0002_auth_tokens.sql. Tokens survive restarts and are shared by every
// replica that points at the same database.

func toHTTPAPIToken(record postgres.APITokenRecord) httpapi.APITokenRecord {
	return httpapi.APITokenRecord{
		ID:         record.ID,
		UserID:     record.UserID,
		Name:       record.Name,
		Role:       record.Role,
		Prefix:     record.Prefix,
		TokenHash:  record.TokenHash,
		CreatedAt:  record.CreatedAt,
		ExpiresAt:  record.ExpiresAt,
		LastUsedAt: record.LastUsedAt,
		RevokedAt:  record.RevokedAt,
	}
}

func fromHTTPAPIToken(record httpapi.APITokenRecord) postgres.APITokenRecord {
	return postgres.APITokenRecord{
		ID:         record.ID,
		UserID:     record.UserID,
		Name:       record.Name,
		Role:       record.Role,
		Prefix:     record.Prefix,
		TokenHash:  record.TokenHash,
		CreatedAt:  record.CreatedAt,
		ExpiresAt:  record.ExpiresAt,
		LastUsedAt: record.LastUsedAt,
		RevokedAt:  record.RevokedAt,
	}
}

// CreateAPIToken stores a hashed API token.
func (s *Store) CreateAPIToken(ctx context.Context, token httpapi.APITokenRecord) (httpapi.APITokenRecord, error) {
	record, err := s.pg.CreateAPIToken(ctx, fromHTTPAPIToken(token))
	if err != nil {
		return httpapi.APITokenRecord{}, mapError(err)
	}
	return toHTTPAPIToken(record), nil
}

// GetAPIToken loads one API token record.
func (s *Store) GetAPIToken(ctx context.Context, id string) (httpapi.APITokenRecord, error) {
	record, err := s.pg.GetAPIToken(ctx, id)
	if err != nil {
		return httpapi.APITokenRecord{}, mapError(err)
	}
	return toHTTPAPIToken(record), nil
}

// ListAPITokens returns tokens for one user, or every token when userID is
// empty, newest first.
func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]httpapi.APITokenRecord, error) {
	records, err := s.pg.ListAPITokens(ctx, userID)
	if err != nil {
		return nil, mapError(err)
	}
	tokens := make([]httpapi.APITokenRecord, 0, len(records))
	for _, record := range records {
		tokens = append(tokens, toHTTPAPIToken(record))
	}
	return tokens, nil
}

// RevokeAPIToken marks a token unusable. Repeated revocation is a no-op.
func (s *Store) RevokeAPIToken(ctx context.Context, id string, now time.Time) error {
	return mapError(s.pg.RevokeAPIToken(ctx, id, now))
}

// FindAPITokenByHash resolves an active token and records its last use.
func (s *Store) FindAPITokenByHash(ctx context.Context, hash string, now time.Time) (httpapi.APITokenRecord, error) {
	record, err := s.pg.FindAPITokenByHash(ctx, hash, now)
	if err != nil {
		return httpapi.APITokenRecord{}, mapError(err)
	}
	return toHTTPAPIToken(record), nil
}

// CreateRefresh stores a refresh token hash and prunes expired records.
func (s *Store) CreateRefresh(ctx context.Context, record auth.RefreshRecord) error {
	return mapError(s.pg.CreateRefresh(ctx, record))
}

// ConsumeRefresh atomically marks an unused refresh token as used.
func (s *Store) ConsumeRefresh(ctx context.Context, tokenHash string, now time.Time) (auth.RefreshRecord, error) {
	record, err := s.pg.ConsumeRefresh(ctx, tokenHash, now)
	return record, mapError(err)
}

// RevokeSession revokes every token issued for one session.
func (s *Store) RevokeSession(ctx context.Context, sessionID string, now time.Time) error {
	return mapError(s.pg.RevokeSession(ctx, sessionID, now))
}

// RevokeFamily revokes every rotated token of one login family.
func (s *Store) RevokeFamily(ctx context.Context, familyID string, now time.Time) error {
	return mapError(s.pg.RevokeFamily(ctx, familyID, now))
}

// IsRevoked reports whether an access token's family or session was revoked.
func (s *Store) IsRevoked(ctx context.Context, familyID, sessionID string) (bool, error) {
	revoked, err := s.pg.IsRevoked(ctx, familyID, sessionID)
	return revoked, mapError(err)
}

// UpdateUserPasswordHash replaces the stored password hash of a local account.
func (s *Store) UpdateUserPasswordHash(ctx context.Context, id, passwordHash string, expected int64) (domain.SyncRevision, error) {
	revision, err := s.pg.UpdateUserPasswordHash(ctx, id, passwordHash, expected)
	return revision, mapError(err)
}

// RevokeUserSessions revokes every refresh-token family of a user except one.
func (s *Store) RevokeUserSessions(ctx context.Context, userID, exceptFamilyID string, now time.Time) (int, error) {
	count, err := s.pg.RevokeUserSessions(ctx, userID, exceptFamilyID, now)
	return count, mapError(err)
}
