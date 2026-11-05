package serverstore

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// This file implements the auth.SessionStore and API-token half of
// httpapi.Store.
//
// The current PostgreSQL schema (0001_init.sql) has no api_tokens or
// refresh_tokens tables, and postgres.Store exposes no methods for them.
// Rather than inventing schema outside the embedded migrations, the adapter
// keeps those records in process memory.
//
// Consequences, accepted for the single-node v1 server:
//   - API tokens and refresh-token families do not survive a restart; clients
//     must re-authenticate and re-mint tokens.
//   - A second replica would not see tokens issued by the first.
//
// A follow-up migration (api_tokens, refresh_tokens) plus postgres.Store
// methods must replace these maps before any multi-replica deployment. See the
// WP8 integration report for the exact contract.

func (s *Store) nowUTC() time.Time {
	clock := s.now
	if clock == nil {
		clock = time.Now
	}
	return clock().UTC().Truncate(time.Microsecond)
}

// CreateAPIToken stores a hashed API token.
func (s *Store) CreateAPIToken(ctx context.Context, token httpapi.APITokenRecord) (httpapi.APITokenRecord, error) {
	if err := ctx.Err(); err != nil {
		return httpapi.APITokenRecord{}, err
	}
	if strings.TrimSpace(token.UserID) == "" {
		return httpapi.APITokenRecord{}, domain.NewValidationError("token.userId", "is required")
	}
	if strings.TrimSpace(token.Name) == "" {
		return httpapi.APITokenRecord{}, domain.NewValidationError("token.name", "is required")
	}
	if !token.Role.Valid() {
		return httpapi.APITokenRecord{}, domain.NewValidationError("token.role", "unknown role")
	}
	if token.TokenHash == "" {
		return httpapi.APITokenRecord{}, domain.NewValidationError("token.hash", "is required")
	}
	if token.ID == "" {
		id, err := auth.RandomID("token")
		if err != nil {
			return httpapi.APITokenRecord{}, err
		}
		token.ID = id
	}
	if _, err := s.pg.GetUser(ctx, token.UserID); err != nil {
		return httpapi.APITokenRecord{}, mapError(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if token.CreatedAt.IsZero() {
		token.CreatedAt = s.nowUTC()
	}
	s.apiTokens[token.ID] = token
	return token, nil
}

// GetAPIToken loads one API token record.
func (s *Store) GetAPIToken(ctx context.Context, id string) (httpapi.APITokenRecord, error) {
	if err := ctx.Err(); err != nil {
		return httpapi.APITokenRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.apiTokens[id]
	if !ok {
		return httpapi.APITokenRecord{}, domain.ErrNotFound
	}
	return record, nil
}

// ListAPITokens returns tokens for one user, or every token when userID is
// empty, newest first.
func (s *Store) ListAPITokens(ctx context.Context, userID string) ([]httpapi.APITokenRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens := make([]httpapi.APITokenRecord, 0, len(s.apiTokens))
	for _, record := range s.apiTokens {
		if userID != "" && record.UserID != userID {
			continue
		}
		tokens = append(tokens, record)
	}
	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].CreatedAt.Equal(tokens[j].CreatedAt) {
			return tokens[i].ID < tokens[j].ID
		}
		return tokens[i].CreatedAt.After(tokens[j].CreatedAt)
	})
	return tokens, nil
}

// RevokeAPIToken marks a token unusable. Repeated revocation is a no-op.
func (s *Store) RevokeAPIToken(ctx context.Context, id string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.apiTokens[id]
	if !ok {
		return domain.ErrNotFound
	}
	if record.RevokedAt == nil {
		at := now.UTC()
		record.RevokedAt = &at
		s.apiTokens[id] = record
	}
	return nil
}

// FindAPITokenByHash resolves an active token and records its last use.
func (s *Store) FindAPITokenByHash(ctx context.Context, hash string, now time.Time) (httpapi.APITokenRecord, error) {
	if err := ctx.Err(); err != nil {
		return httpapi.APITokenRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, record := range s.apiTokens {
		if record.TokenHash != hash || record.RevokedAt != nil {
			continue
		}
		at := now.UTC()
		record.LastUsedAt = &at
		s.apiTokens[id] = record
		return record, nil
	}
	return httpapi.APITokenRecord{}, domain.ErrNotFound
}

// CreateRefresh stores a refresh token hash and prunes expired records.
func (s *Store) CreateRefresh(ctx context.Context, record auth.RefreshRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if record.TokenHash == "" || record.SessionID == "" || record.UserID == "" {
		return domain.NewValidationError("refresh", "session, user and hash are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowUTC()
	for hash, existing := range s.refresh {
		if existing.ExpiresAt.Before(now) {
			delete(s.refresh, hash)
		}
	}
	if _, exists := s.refresh[record.TokenHash]; exists {
		return domain.ErrConflict
	}
	s.refresh[record.TokenHash] = record
	return nil
}

// ConsumeRefresh atomically marks an unused refresh token as used.
func (s *Store) ConsumeRefresh(ctx context.Context, tokenHash string, now time.Time) (auth.RefreshRecord, error) {
	if err := ctx.Err(); err != nil {
		return auth.RefreshRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.refresh[tokenHash]
	if !ok {
		return auth.RefreshRecord{}, auth.ErrInvalidRefresh
	}
	if record.RevokedAt != nil || !record.ExpiresAt.After(now) {
		return record, auth.ErrInvalidRefresh
	}
	if record.UsedAt != nil {
		return record, auth.ErrRefreshReused
	}
	at := now.UTC()
	record.UsedAt = &at
	s.refresh[tokenHash] = record
	return record, nil
}

// RevokeSession revokes every token issued for one session.
func (s *Store) RevokeSession(ctx context.Context, sessionID string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at := now.UTC()
	for hash, record := range s.refresh {
		if record.SessionID == sessionID && record.RevokedAt == nil {
			record.RevokedAt = &at
			s.refresh[hash] = record
		}
	}
	return nil
}

// RevokeFamily revokes every rotated token of one login family.
func (s *Store) RevokeFamily(ctx context.Context, familyID string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(familyID) == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at := now.UTC()
	for hash, record := range s.refresh {
		if record.FamilyID == familyID && record.RevokedAt == nil {
			record.RevokedAt = &at
			s.refresh[hash] = record
		}
	}
	return nil
}
