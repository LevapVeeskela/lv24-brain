package fsstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeLockFile(t *testing.T, path string, owner fileLock) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create lock directory: %v", err)
	}
	raw, err := json.Marshal(owner)
	if err != nil {
		t.Fatalf("encode lock: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
}

// deadPID returns a pid no process on this host can own.
func deadPID(t *testing.T) int {
	t.Helper()
	for pid := 999999; pid < 1000099; pid++ {
		if !pidAlive(pid) {
			return pid
		}
	}
	t.Skip("no dead pid available on this host")
	return 0
}

func TestPulledForeignLockDoesNotBlockWriter(t *testing.T) {
	withShortLockTimings(t, 2*time.Second)
	target := filepath.Join(t.TempDir(), "state.md")
	now := time.Now().UTC()
	writeLockFile(t, target+lockFileSuffix, fileLock{
		PID:       os.Getpid(), // alive here, but owned by another machine
		BootID:    bootID(),
		Host:      "other-machine",
		CreatedAt: now.Format(time.RFC3339Nano),
		TTL:       60,
		Token:     "pulled",
	})

	started := time.Now()
	release, err := acquireFileLock(context.Background(), target)
	if err != nil {
		t.Fatalf("a pulled lock blocked the writer: %v", err)
	}
	release()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("acquiring over a pulled lock took %s, want no wait", elapsed)
	}
}

func TestReleaseDoesNotRemoveForeignLock(t *testing.T) {
	withShortLockTimings(t, time.Second)
	target := filepath.Join(t.TempDir(), "state.md")
	release, err := acquireFileLock(context.Background(), target)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lockPath := target + lockFileSuffix
	writeLockFile(t, lockPath, fileLock{
		PID:       os.Getpid(),
		BootID:    bootID(),
		Host:      localHost(),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		TTL:       60,
		Token:     "someone-else",
	})
	release()

	raw, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("release removed a lock it did not own: %v", err)
	}
	if token := decodeFileLock(raw, time.Time{}).Token; token != "someone-else" {
		t.Fatalf("lock token = %q after foreign release, want someone-else", token)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestDeadOwnerLockIsStaleOnlyAfterTTL(t *testing.T) {
	oldTimeout, oldStale := fileLockTimeout, fileLockStaleAfter
	fileLockTimeout = 100 * time.Millisecond
	fileLockStaleAfter = 2 * time.Second
	t.Cleanup(func() {
		fileLockTimeout = oldTimeout
		fileLockStaleAfter = oldStale
	})

	target := filepath.Join(t.TempDir(), "state.md")
	lockPath := target + lockFileSuffix
	now := time.Now().UTC()
	dead := fileLock{
		PID:       deadPID(t),
		BootID:    bootID(),
		Host:      localHost(),
		CreatedAt: now.Format(time.RFC3339Nano),
		Token:     "dead-owner",
	}
	// A dead owner inside its TTL must still block: the process may be a
	// short-lived holder that has not released yet.
	writeLockFile(t, lockPath, dead)
	if _, err := acquireFileLock(context.Background(), target); !errors.Is(err, errLockTimeout) {
		t.Fatalf("fresh dead-owner lock: err = %v, want errLockTimeout", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("dead-owner lock inside TTL was removed: %v", err)
	}

	// Past the TTL the owner is presumed crashed and the lock is broken.
	dead.CreatedAt = now.Add(-3 * time.Second).Format(time.RFC3339Nano)
	writeLockFile(t, lockPath, dead)
	release, err := acquireFileLock(context.Background(), target)
	if err != nil {
		t.Fatalf("stale dead-owner lock was not broken: %v", err)
	}
	release()
}

func TestLiveOwnerLockIsNotBrokenAfterTTL(t *testing.T) {
	oldTimeout, oldStale := fileLockTimeout, fileLockStaleAfter
	fileLockTimeout = 200 * time.Millisecond
	fileLockStaleAfter = 50 * time.Millisecond
	t.Cleanup(func() {
		fileLockTimeout = oldTimeout
		fileLockStaleAfter = oldStale
	})

	target := filepath.Join(t.TempDir(), "state.md")
	lockPath := target + lockFileSuffix
	writeLockFile(t, lockPath, fileLock{
		PID:       os.Getpid(),
		BootID:    bootID(),
		Host:      localHost(),
		CreatedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
		TTL:       1,
		Token:     "live-owner",
	})

	if _, err := acquireFileLock(context.Background(), target); !errors.Is(err, errLockTimeout) {
		t.Fatalf("live long-held lock: err = %v, want errLockTimeout", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("live owner's lock was broken after the TTL: %v", err)
	}
}

func TestBreakStaleLockRefusesLiveOwner(t *testing.T) {
	withShortLockTimings(t, time.Second)
	target := filepath.Join(t.TempDir(), "state.md")
	release, err := acquireFileLock(context.Background(), target)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lockPath := target + lockFileSuffix
	removed, err := BreakStaleLock(lockPath)
	if err != nil {
		t.Fatalf("BreakStaleLock: %v", err)
	}
	if removed {
		t.Fatalf("BreakStaleLock removed a live owner's lock")
	}
	release()

	writeLockFile(t, lockPath, fileLock{
		PID:       deadPID(t),
		BootID:    bootID(),
		Host:      localHost(),
		CreatedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
		Token:     "dead",
	})
	removed, err = BreakStaleLock(lockPath)
	if err != nil {
		t.Fatalf("BreakStaleLock stale: %v", err)
	}
	if !removed {
		t.Fatalf("BreakStaleLock did not remove a dead owner's lock")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock still exists: %v", err)
	}
}

func TestStaleLocksReportsOnlyUnownedLocks(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	projectDir := filepath.Join(store.Root(), "projects", project.ID)

	release, err := acquireFileLock(context.Background(), filepath.Join(projectDir, "state.md"))
	if err != nil {
		t.Fatalf("acquire live lock: %v", err)
	}
	defer release()

	foreignPath := filepath.Join(projectDir, "notes", "2026-11-05.md.lock")
	writeLockFile(t, foreignPath, fileLock{
		PID:       os.Getpid(),
		BootID:    "another-boot",
		Host:      "other-machine",
		CreatedAt: testClock.Format(time.RFC3339Nano),
		TTL:       60,
		Token:     "pulled",
	})
	deadPath := filepath.Join(projectDir, "dead.md.lock")
	writeLockFile(t, deadPath, fileLock{
		PID:       deadPID(t),
		BootID:    bootID(),
		Host:      localHost(),
		CreatedAt: testClock.Add(-time.Hour).Format(time.RFC3339Nano),
		Token:     "dead",
	})

	stale, err := store.StaleLocks(context.Background())
	if err != nil {
		t.Fatalf("StaleLocks: %v", err)
	}
	if !reflect.DeepEqual(stale, []string{deadPath, foreignPath}) {
		t.Fatalf("StaleLocks = %v, want [%s %s]", stale, deadPath, foreignPath)
	}
}
