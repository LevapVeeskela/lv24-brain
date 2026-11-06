package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestAPITokenExpiryLookup checks that an expired token is not found by hash,
// exactly like a revoked one, while tokens without an expiry and tokens whose
// expiry is still ahead keep resolving.
func TestAPITokenExpiryLookup(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	if _, _, err := store.CreateUser(ctx, domain.User{ID: "user_expiry", Login: "expiry"}, "test-hash-not-a-secret"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	now := time.Now().UTC()
	expired := now.Add(-time.Minute)
	future := now.Add(time.Hour)

	expiredToken, err := store.CreateAPIToken(ctx, APITokenRecord{
		UserID: "user_expiry", Name: "old", Role: domain.RoleMember, Prefix: "brain_old",
		TokenHash: "hash-expired", ExpiresAt: &expired,
	})
	if err != nil {
		t.Fatalf("CreateAPIToken expired: %v", err)
	}
	if expiredToken.ExpiresAt == nil {
		t.Fatal("CreateAPIToken did not return expires_at")
	}
	if _, err := store.CreateAPIToken(ctx, APITokenRecord{
		UserID: "user_expiry", Name: "later", Role: domain.RoleMember, Prefix: "brain_later",
		TokenHash: "hash-future", ExpiresAt: &future,
	}); err != nil {
		t.Fatalf("CreateAPIToken future: %v", err)
	}
	if _, err := store.CreateAPIToken(ctx, APITokenRecord{
		UserID: "user_expiry", Name: "forever", Role: domain.RoleMember, Prefix: "brain_forever",
		TokenHash: "hash-forever",
	}); err != nil {
		t.Fatalf("CreateAPIToken forever: %v", err)
	}

	if _, err := store.FindAPITokenByHash(ctx, "hash-expired", now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("FindAPITokenByHash expired = %v, want ErrNotFound", err)
	}

	found, err := store.FindAPITokenByHash(ctx, "hash-future", now)
	if err != nil || found.ExpiresAt == nil {
		t.Fatalf("FindAPITokenByHash future = (%+v, %v)", found, err)
	}
	if _, err := store.FindAPITokenByHash(ctx, "hash-future", now.Add(2*time.Hour)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("FindAPITokenByHash after expiry = %v, want ErrNotFound", err)
	}

	forever, err := store.FindAPITokenByHash(ctx, "hash-forever", now.Add(10*365*24*time.Hour))
	if err != nil {
		t.Fatalf("FindAPITokenByHash forever = %v", err)
	}
	if forever.ExpiresAt != nil {
		t.Fatalf("forever token ExpiresAt = %v, want nil", forever.ExpiresAt)
	}
}
