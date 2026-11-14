package fsstore

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func putTestProposal(t *testing.T, store *Store, projectID string) string {
	t.Helper()
	stored, err := store.Put(context.Background(), testProposal(projectID))
	if err != nil {
		t.Fatalf("Put proposal: %v", err)
	}
	return stored.ID
}

// TestProposalLockSerializesOneProposal checks the promotion lock excludes
// concurrent holders of the same (project, proposal) pair.
func TestProposalLockSerializesOneProposal(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposalID := putTestProposal(t, store, project.ID)

	const callers = 20
	var active, peak int32
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = store.WithProposalLock(ctx, project.ID, proposalID, func() error {
				current := atomic.AddInt32(&active, 1)
				for {
					seen := atomic.LoadInt32(&peak)
					if current <= seen || atomic.CompareAndSwapInt32(&peak, seen, current) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt32(&active, -1)
				return nil
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("lock caller %d: %v", i, err)
		}
	}
	if peak != 1 {
		t.Fatalf("peak concurrent holders = %d, want 1", peak)
	}
}

// TestProposalLockAllowsIndependentProposals verifies the lock is per proposal
// and per project, not one global promotion lock.
func TestProposalLockAllowsIndependentProposals(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	first := putTestProposal(t, store, project.ID)
	second := testProposal(project.ID)
	second.ExternalID = "acme/widgets#issue-8"
	second.ID = ""
	stored, err := store.Put(ctx, second)
	if err != nil {
		t.Fatalf("Put second proposal: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.WithProposalLock(ctx, project.ID, first, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	// A different proposal must not wait for the first lock.
	err = store.WithProposalLock(ctx, project.ID, stored.ID, func() error { return nil })
	close(release)
	if err != nil {
		t.Fatalf("independent proposal was blocked: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("first lock: %v", err)
	}
}

// TestProposalLockReleasesOnErrorAndPanic checks the lock file is gone after
// every exit path so a failed promotion never blocks the next run.
func TestProposalLockReleasesOnErrorAndPanic(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposalID := putTestProposal(t, store, project.ID)
	path, err := store.proposalPath(project.ID, proposalID)
	if err != nil {
		t.Fatalf("proposalPath: %v", err)
	}
	lockPath := path + proposalLockSuffix + lockFileSuffix

	failure := errors.New("promotion failed")
	if err := store.WithProposalLock(ctx, project.ID, proposalID, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("callback error = %v, want %v unchanged", err, failure)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file after error: %v, want not exist", err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("callback panic did not propagate")
			}
		}()
		_ = store.WithProposalLock(ctx, project.ID, proposalID, func() error { panic(failure) })
	}()
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock file after panic: %v, want not exist", err)
	}
	if err := store.WithProposalLock(ctx, project.ID, proposalID, func() error { return nil }); err != nil {
		t.Fatalf("lock after panic: %v", err)
	}
}

// TestProposalLockRejectsMissingCallback keeps a nil callback from silently
// doing nothing while appearing to hold the lock.
func TestProposalLockRejectsMissingCallback(t *testing.T) {
	store := newTestStore(t)
	project := seedProject(t, store)
	proposalID := putTestProposal(t, store, project.ID)

	err := store.WithProposalLock(context.Background(), project.ID, proposalID, nil)
	if err == nil {
		t.Fatal("nil callback was accepted")
	}
}

// TestProposalLockFileIsReportedAsStale confirms the promotion lock keeps the
// shared lock payload, so brain doctor's stale-lock walk sees a crashed
// promotion and never reports a live one.
func TestProposalLockFileIsReportedAsStale(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	project := seedProject(t, store)
	proposalID := putTestProposal(t, store, project.ID)
	path, err := store.proposalPath(project.ID, proposalID)
	if err != nil {
		t.Fatalf("proposalPath: %v", err)
	}
	lockPath := path + proposalLockSuffix + lockFileSuffix
	writeLockFile(t, lockPath, fileLock{
		PID:       deadPID(t),
		BootID:    bootID(),
		Host:      localHost(),
		CreatedAt: testClock.Add(-time.Hour).Format(time.RFC3339Nano),
		TTL:       1,
		Token:     "dead-promotion",
	})

	stale, err := store.StaleLocks(ctx)
	if err != nil {
		t.Fatalf("StaleLocks: %v", err)
	}
	if !containsPath(stale, lockPath) {
		t.Fatalf("StaleLocks = %v, want %s", stale, lockPath)
	}

	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.WithProposalLock(ctx, project.ID, proposalID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	stale, err = store.StaleLocks(ctx)
	close(release)
	if err != nil {
		t.Fatalf("StaleLocks while held: %v", err)
	}
	if containsPath(stale, lockPath) {
		t.Fatalf("a live promotion lock was reported stale: %v", stale)
	}
	if err := <-done; err != nil {
		t.Fatalf("held lock: %v", err)
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}
