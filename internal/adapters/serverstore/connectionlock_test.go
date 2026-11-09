package serverstore

import (
	"errors"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestConnectionLockSerializesDatabaseSessions(t *testing.T) {
	store := newTestStore(t)
	ctx := testContext(t)
	release, err := store.TryLock(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := store.TryLock(ctx, "work"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("second lock: %v", err)
	}
	other, err := store.TryLock(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	_ = other()
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	next, err := store.TryLock(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	_ = next()
}
