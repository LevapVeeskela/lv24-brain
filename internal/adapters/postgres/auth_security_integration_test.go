package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/auth"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TestLastAdminConcurrentRemovalExactlyOneSucceeds races two removals of the
// two remaining admins and requires exactly one to succeed. The other must be
// refused with domain.ErrLastAdmin, and one admin must remain afterwards. Each
// round starts from a fresh pair of admins; leftovers are demoted first.
func TestLastAdminConcurrentRemovalExactlyOneSucceeds(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)

	for round := 0; round < 12; round++ {
		a := createAdmin(t, ctx, store, "race-a-"+string(rune('a'+round)))
		b := createAdmin(t, ctx, store, "race-b-"+string(rune('a'+round)))
		// Demote leftovers from earlier rounds while the fresh pair is present.
		users, err := store.ListUsers(ctx)
		if err != nil {
			t.Fatalf("round %d: ListUsers: %v", round, err)
		}
		for _, user := range users {
			if user.Role == domain.RoleAdmin && user.ID != a.ID && user.ID != b.ID {
				if _, _, err := store.UpdateUserRole(ctx, user.ID, domain.RoleMember, 0); err != nil {
					t.Fatalf("round %d: clear leftover admin: %v", round, err)
				}
			}
		}

		removeA := func() error { return removeAdmin(ctx, store, a.ID, round%2 == 0) }
		removeB := func() error { return removeAdmin(ctx, store, b.ID, round%2 == 1) }
		errs := runConcurrently(removeA, removeB)

		succeeded, refused := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, domain.ErrLastAdmin):
				refused++
			default:
				t.Fatalf("round %d: unexpected error: %v", round, err)
			}
		}
		if succeeded != 1 || refused != 1 {
			t.Fatalf("round %d: succeeded=%d refused=%d, want 1 and 1", round, succeeded, refused)
		}
		if n := countAdmins(t, ctx, store); n != 1 {
			t.Fatalf("round %d: admins after race = %d, want 1", round, n)
		}
	}
}

// TestLastAdminGuardAllowsRemovalWhenAnotherAdminExists checks the guard does
// not block a normal demotion or deletion.
func TestLastAdminGuardAllowsRemovalWhenAnotherAdminExists(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	a := createAdmin(t, ctx, store, "solo-a")
	b := createAdmin(t, ctx, store, "solo-b")

	if _, _, err := store.UpdateUserRole(ctx, a.ID, domain.RoleMember, 0); err != nil {
		t.Fatalf("demote with another admin present: %v", err)
	}
	if _, err := store.DeleteUser(ctx, b.ID, 0); !errors.Is(err, domain.ErrLastAdmin) {
		t.Fatalf("delete last admin = %v, want ErrLastAdmin", err)
	}
	if _, err := store.DeleteUser(ctx, a.ID, 0); err != nil {
		t.Fatalf("delete non-admin: %v", err)
	}
}

// TestPasswordChangeRevokesOtherFamiliesOnly stores a new hash and revokes all
// other families of the user. The kept family keeps rotating; the others and
// their access tokens' revocation checks fail.
func TestPasswordChangeRevokesOtherFamiliesOnly(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_pwchange", Login: "pwchange", Role: domain.RoleMember}, "old-hash-placeholder")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()
	keep := seedFamily(t, ctx, store, user.ID, "keep", now)
	other := seedFamily(t, ctx, store, user.ID, "other", now)

	newHash, err := auth.HashPassword("new-password-123", auth.DefaultArgon2Params)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if _, err := store.UpdateUserPasswordHash(ctx, user.ID, newHash, 0); err != nil {
		t.Fatalf("UpdateUserPasswordHash: %v", err)
	}
	covered, err := store.RevokeUserSessions(ctx, user.ID, keep.FamilyID, now)
	if err != nil {
		t.Fatalf("RevokeUserSessions: %v", err)
	}
	if covered != 1 {
		t.Fatalf("covered = %d, want 1", covered)
	}

	stored, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	ok, err := auth.VerifyPassword(stored.PasswordHash, "new-password-123")
	if err != nil || !ok {
		t.Fatalf("stored hash does not verify the new password: ok=%v err=%v", ok, err)
	}

	if revoked, err := store.IsRevoked(ctx, other.FamilyID, other.SessionID); err != nil || !revoked {
		t.Fatalf("other family revoked = %v, %v; want true", revoked, err)
	}
	if revoked, err := store.IsRevoked(ctx, keep.FamilyID, keep.SessionID); err != nil || revoked {
		t.Fatalf("kept family revoked = %v, %v; want false", revoked, err)
	}
	if _, err := store.ConsumeRefresh(ctx, other.TokenHash, now); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("other family refresh = %v, want ErrInvalidRefresh", err)
	}
	if _, err := store.ConsumeRefresh(ctx, keep.TokenHash, now); err != nil {
		t.Fatalf("kept family refresh = %v, want success", err)
	}
}

// TestRevokeUserSessionsIsIdempotent checks a second revoke-all covers the
// same families without error and leaves the tombstones in place.
func TestRevokeUserSessionsIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	user, _, err := store.CreateUser(ctx, domain.User{ID: "user_revoke_twice", Login: "revoke-twice", Role: domain.RoleMember}, "")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()
	seeded := seedFamily(t, ctx, store, user.ID, "twice", now)
	for i := 0; i < 2; i++ {
		if _, err := store.RevokeUserSessions(ctx, user.ID, "", now); err != nil {
			t.Fatalf("RevokeUserSessions #%d: %v", i+1, err)
		}
	}
	if revoked, err := store.IsRevoked(ctx, seeded.FamilyID, seeded.SessionID); err != nil || !revoked {
		t.Fatalf("family revoked = %v, %v; want true", revoked, err)
	}
}

func createAdmin(t *testing.T, ctx context.Context, store *Store, login string) UserRecord {
	t.Helper()
	record, _, err := store.CreateUser(ctx, domain.User{Login: login, Role: domain.RoleAdmin, AuthSource: "local"}, "")
	if err != nil {
		t.Fatalf("create admin %s: %v", login, err)
	}
	return record
}

func removeAdmin(ctx context.Context, store *Store, id string, deleteUser bool) error {
	if deleteUser {
		_, err := store.DeleteUser(ctx, id, 0)
		return err
	}
	_, _, err := store.UpdateUserRole(ctx, id, domain.RoleMember, 0)
	return err
}

// runConcurrently starts every function at once and returns their errors in
// argument order. A barrier makes the two calls overlap.
func runConcurrently(fns ...func() error) []error {
	errs := make([]error, len(fns))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func() error) {
			defer wg.Done()
			<-start
			errs[i] = fn()
		}(i, fn)
	}
	close(start)
	wg.Wait()
	return errs
}

func countAdmins(t *testing.T, ctx context.Context, store *Store) int {
	t.Helper()
	users, err := store.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	count := 0
	for _, user := range users {
		if user.Role == domain.RoleAdmin {
			count++
		}
	}
	return count
}

// seedFamily stores one refresh token for a fresh session in the given family
// and returns its record.
func seedFamily(t *testing.T, ctx context.Context, store *Store, userID, name string, now time.Time) auth.RefreshRecord {
	t.Helper()
	record := auth.RefreshRecord{
		SessionID: "sess_" + name,
		FamilyID:  "fam_" + name,
		UserID:    userID,
		TokenHash: "hash_" + name,
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
	}
	if err := store.CreateRefresh(ctx, record); err != nil {
		t.Fatalf("CreateRefresh %s: %v", name, err)
	}
	return record
}
