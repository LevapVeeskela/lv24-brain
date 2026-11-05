package httpapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func testRefreshRecord(hash, sessionID, familyID, userID string, now time.Time) auth.RefreshRecord {
	return auth.RefreshRecord{
		SessionID: sessionID,
		FamilyID:  familyID,
		UserID:    userID,
		TokenHash: hash,
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	}
}

func TestMemoryStoreRevokeSessionBlocksRotation(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	if err := store.CreateRefresh(ctx, testRefreshRecord("hash-1", "sess-1", "fam-1", "user-1", now)); err != nil {
		t.Fatalf("CreateRefresh: %v", err)
	}
	if err := store.RevokeSession(ctx, "  sess-1  ", now); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	// A logout racing an in-flight refresh must win: a rotated token for the
	// revoked session cannot be stored even though it has a fresh hash.
	if err := store.CreateRefresh(ctx, testRefreshRecord("hash-2", "sess-1", "fam-1", "user-1", now)); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("CreateRefresh after logout = %v, want ErrInvalidRefresh", err)
	}
	if _, err := store.ConsumeRefresh(ctx, "hash-1", now); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh after logout = %v, want ErrInvalidRefresh", err)
	}

	// An unrelated family is unaffected by the tombstone.
	if err := store.CreateRefresh(ctx, testRefreshRecord("hash-3", "sess-3", "fam-2", "user-2", now)); err != nil {
		t.Fatalf("CreateRefresh(other family): %v", err)
	}
}

func TestMemoryStoreRevokeFamilyBlocksInFlightRotation(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	first := testRefreshRecord("hash-1", "sess-1", "fam-1", "user-1", now)
	if err := store.CreateRefresh(ctx, first); err != nil {
		t.Fatalf("CreateRefresh(first): %v", err)
	}
	if _, err := store.ConsumeRefresh(ctx, "hash-1", now); err != nil {
		t.Fatalf("ConsumeRefresh(first): %v", err)
	}
	second := testRefreshRecord("hash-2", "sess-2", "fam-1", "user-1", now)
	if err := store.CreateRefresh(ctx, second); err != nil {
		t.Fatalf("CreateRefresh(rotation): %v", err)
	}

	if err := store.RevokeFamily(ctx, " fam-1 ", now); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}
	if _, err := store.ConsumeRefresh(ctx, "hash-2", now); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh(rotation) after family revoke = %v, want ErrInvalidRefresh", err)
	}
	if err := store.CreateRefresh(ctx, testRefreshRecord("hash-3", "sess-3", "fam-1", "user-1", now)); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("CreateRefresh after family revoke = %v, want ErrInvalidRefresh", err)
	}
}

func TestMemoryStoreDeleteUserDropsRefreshState(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	user, _, err := store.CreateUser(ctx, domain.User{Login: "alice", Role: domain.RoleMember}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.CreateRefresh(ctx, testRefreshRecord("hash-1", "sess-1", "fam-1", user.ID, now)); err != nil {
		t.Fatalf("CreateRefresh: %v", err)
	}
	if err := store.RevokeFamily(ctx, "fam-1", now); err != nil {
		t.Fatalf("RevokeFamily: %v", err)
	}
	if _, err := store.DeleteUser(ctx, user.ID, 0); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	// Tokens and tombstones attributable to the deleted user are gone; a new
	// token for the same (now unused) ids is not blocked by a stale tombstone.
	if _, err := store.ConsumeRefresh(ctx, "hash-1", now); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("ConsumeRefresh after DeleteUser = %v, want ErrInvalidRefresh", err)
	}
	if err := store.CreateRefresh(ctx, testRefreshRecord("hash-2", "sess-1", "fam-1", user.ID, now)); err != nil {
		t.Fatalf("CreateRefresh after DeleteUser: %v", err)
	}
}
