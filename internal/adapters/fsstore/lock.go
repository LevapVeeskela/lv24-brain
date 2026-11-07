package fsstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// lockFileSuffix names the lock file that guards a shared file. The lock for
// "<file>" is "<file>.lock", created next to it.
//
// Lock files are never record files: listing only accepts names ending in
// ".md" and CurrentRevision skips names ending in ".lock", so a lock held
// during a write does not appear in listings or change the revision.
const lockFileSuffix = ".lock"

var (
	// fileLockTimeout bounds how long acquireFileLock waits for a live holder.
	fileLockTimeout = 10 * time.Second
	// fileLockStaleAfter is the lock age (by mtime) after which the holder is
	// presumed dead and the lock is broken.
	fileLockStaleAfter = 60 * time.Second
	// fileLockRetryMin and fileLockRetryMax bound the exponential backoff.
	fileLockRetryMin = 5 * time.Millisecond
	fileLockRetryMax = 100 * time.Millisecond
)

// errLockTimeout reports that a live holder kept a lock past fileLockTimeout.
var errLockTimeout = errors.New("fsstore: lock timed out")

// acquireFileLock takes the cross-process lock for target and returns the
// function that releases it. The lock is a file created with O_CREATE|O_EXCL,
// so it works the same across processes and on Windows without extra
// dependencies. Acquisition retries with exponential backoff until
// fileLockTimeout. A lock whose mtime is older than fileLockStaleAfter is
// treated as left by a crashed process and removed.
//
// The release function must always be called (defer it). Breaking a stale lock
// is not atomic: two processes can race to break the same stale lock. That
// window only exists once the lock is older than a minute, so it is accepted.
func acquireFileLock(ctx context.Context, target string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lockPath := target + lockFileSuffix
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("fsstore: create lock directory for %s: %w", target, err)
	}
	deadline := time.Now().Add(fileLockTimeout)
	backoff := fileLockRetryMin
	for {
		file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_, writeErr := fmt.Fprintf(file, "pid %d\n%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("fsstore: write lock %s: %w", lockPath, errors.Join(writeErr, closeErr))
			}
			return func() { _ = os.Remove(lockPath) }, nil
		}
		// On Windows, opening a name whose previous lock is still being deleted
		// fails with "access denied" rather than "exists". Treat both as busy and
		// retry; a persistent permission error still ends at the timeout.
		busy := os.IsExist(err) || errors.Is(err, fs.ErrPermission)
		if !busy {
			return nil, fmt.Errorf("fsstore: create lock %s: %w", lockPath, err)
		}
		if breakStaleLock(lockPath) {
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("fsstore: lock %s after %s (last error: %v): %w", lockPath, fileLockTimeout, err, errLockTimeout)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > fileLockRetryMax {
			backoff = fileLockRetryMax
		}
	}
}

// breakStaleLock removes lockPath when it is older than fileLockStaleAfter. It
// reports true when the caller should retry immediately: the lock was removed
// or disappeared meanwhile.
func breakStaleLock(lockPath string) bool {
	info, err := os.Stat(lockPath)
	if err != nil {
		return os.IsNotExist(err)
	}
	if time.Since(info.ModTime()) < fileLockStaleAfter {
		return false
	}
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		return false
	}
	return true
}

// withFileLock runs fn while holding the lock for target. It wraps every
// read-modify-write of a shared file so that concurrent writers serialize.
func withFileLock(ctx context.Context, target string, fn func() error) error {
	release, err := acquireFileLock(ctx, target)
	if err != nil {
		return err
	}
	defer release()
	return fn()
}
