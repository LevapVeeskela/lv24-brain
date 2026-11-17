package fsstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	// fileLockStaleAfter is the default lock TTL. A lock is stale only after
	// this age and only when its owner can no longer hold it: the pid is dead,
	// the machine rebooted, or the lock came from another host. A live owner is
	// never broken, however long the write takes.
	fileLockStaleAfter = 60 * time.Second
	// fileLockRetryMin and fileLockRetryMax bound the exponential backoff.
	fileLockRetryMin = 5 * time.Millisecond
	fileLockRetryMax = 100 * time.Millisecond
)

// errLockTimeout reports that a live holder kept a lock past fileLockTimeout.
var errLockTimeout = errors.New("fsstore: lock timed out")

// fileLock is the lock payload. It records enough to decide whether the owner
// can still be running: pid, boot id and host. Token is random per acquisition
// and makes release delete only the lock it created.
type fileLock struct {
	PID       int    `json:"pid"`
	BootID    string `json:"boot_id"`
	Host      string `json:"host"`
	CreatedAt string `json:"created_at"`
	TTL       int64  `json:"ttl"`
	Token     string `json:"token"`
}

// acquireFileLock takes the cross-process lock for target and returns the
// function that releases it. The lock is a file created with
// O_CREATE|O_EXCL, so it works the same across processes and on Windows
// without extra dependencies. Acquisition retries with exponential backoff
// until fileLockTimeout.
//
// A lock is broken only when it is provably not held: its pid is dead on this
// host and the lock is older than its TTL, or it was written by another host
// (a pulled lock cannot guard this checkout). A live owner is never broken, so
// a slow write does not lose its lock. The release function must always be
// called (defer it); it removes the lock only while the file still carries
// this acquisition's token.
func acquireFileLock(ctx context.Context, target string) (func(), error) {
	return acquireFileLockPath(ctx, target+lockFileSuffix)
}

// AcquirePathLock takes the owner-aware lock file at lockPath verbatim (it must
// already end in ".lock"). It is the shared primitive behind Store.LockSync and
// the git provider's publish lock, so a data-store sync and a publish contend
// on the same file with the same pid, host and TTL rules and neither can steal
// or delete the other's lock.
func AcquirePathLock(ctx context.Context, lockPath string) (func(), error) {
	if !strings.HasSuffix(lockPath, lockFileSuffix) {
		return nil, fmt.Errorf("fsstore: lock path %s must end in %s", lockPath, lockFileSuffix)
	}
	return acquireFileLockPath(ctx, lockPath)
}

// acquireFileLockPath is the implementation of every owner-aware lock.
func acquireFileLockPath(ctx context.Context, lockPath string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("fsstore: create lock directory for %s: %w", lockPath, err)
	}
	owner, err := newFileLock()
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(owner)
	if err != nil {
		return nil, fmt.Errorf("fsstore: encode lock for %s: %w", lockPath, err)
	}
	deadline := time.Now().Add(fileLockTimeout)
	backoff := fileLockRetryMin
	for {
		file, openErr := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if openErr == nil {
			// The payload is small and only read while the file exists; the
			// lock is not a durability record, so it is not fsynced. A crash
			// makes the lock stale, which is exactly what the TTL handles.
			_, writeErr := file.Write(payload)
			closeErr := file.Close()
			if err := errors.Join(writeErr, closeErr); err != nil {
				_ = removeFile(lockPath)
				return nil, fmt.Errorf("fsstore: write lock %s: %w", lockPath, err)
			}
			return func() { releaseFileLock(lockPath, owner.Token) }, nil
		}
		// On Windows, opening a name whose previous lock is still being deleted
		// fails with "access denied" rather than "exists". Treat both as busy and
		// retry; a persistent permission error still ends at the timeout.
		busy := os.IsExist(openErr) || errors.Is(openErr, fs.ErrPermission)
		if !busy {
			return nil, fmt.Errorf("fsstore: create lock %s: %w", lockPath, openErr)
		}
		if staleLockRemoved(lockPath) {
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("fsstore: lock %s after %s (last error: %v): %w", lockPath, fileLockTimeout, openErr, errLockTimeout)
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

// staleLockRemoved removes lockPath when it is stale and unchanged since it was
// read. It reports true when the caller should retry immediately.
func staleLockRemoved(lockPath string) bool {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return os.IsNotExist(err)
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		return os.IsNotExist(err)
	}
	if !lockStale(decodeFileLock(data, info.ModTime()), info.ModTime(), time.Now()) {
		return false
	}
	// Read once more immediately before removing: only an unchanged stale lock
	// may be removed, so a fresh lock created by another writer is left alone.
	current, err := os.ReadFile(lockPath)
	if err != nil {
		return os.IsNotExist(err)
	}
	if !bytes.Equal(current, data) {
		return false
	}
	if err := removeFile(lockPath); err != nil {
		return false
	}
	return true
}

// releaseFileLock removes lockPath only when it still belongs to token.
func releaseFileLock(lockPath, token string) {
	if token == "" {
		return
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return
	}
	if decodeFileLock(data, time.Time{}).Token != token {
		return
	}
	_ = removeFile(lockPath)
}

// newFileLock builds the payload of one acquisition.
func newFileLock() (fileLock, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return fileLock{}, fmt.Errorf("fsstore: generate lock token: %w", err)
	}
	return fileLock{
		PID:       os.Getpid(),
		BootID:    bootID(),
		Host:      localHost(),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		TTL:       int64(fileLockStaleAfter / time.Second),
		Token:     hex.EncodeToString(token[:]),
	}, nil
}

// decodeFileLock parses a lock file. An unreadable payload falls back to the
// legacy "pid <n>" first line, which has no host or token: it is treated as a
// lock of this host inside the TTL.
func decodeFileLock(data []byte, modTime time.Time) fileLock {
	var owner fileLock
	if err := json.Unmarshal(data, &owner); err == nil && owner.PID != 0 {
		return owner
	}
	legacy := fileLock{CreatedAt: modTime.UTC().Format(time.RFC3339Nano)}
	if _, err := fmt.Sscanf(string(data), "pid %d", &legacy.PID); err != nil {
		legacy.PID = 0
	}
	if lines := strings.Split(string(data), "\n"); len(lines) > 1 {
		if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(lines[1])); err == nil {
			legacy.CreatedAt = strings.TrimSpace(lines[1])
		}
	}
	return legacy
}

// lockStale reports whether a lock no longer has a live owner.
func lockStale(owner fileLock, modTime, now time.Time) bool {
	ttl := fileLockStaleAfter
	if owner.TTL > 0 {
		ttl = time.Duration(owner.TTL) * time.Second
	}
	created := modTime
	if parsed, err := time.Parse(time.RFC3339Nano, owner.CreatedAt); err == nil {
		created = parsed
	}
	age := now.Sub(created)

	// A lock pulled from another machine cannot guard this checkout: its pid
	// cannot be probed here and this engine never publishes lock files.
	if owner.Host != "" && !strings.EqualFold(owner.Host, localHost()) {
		return true
	}
	if owner.PID <= 0 {
		return age > ttl
	}
	if ours := bootID(); owner.BootID != "" && ours != "" && owner.BootID != ours {
		// The owner ran before a reboot; its pid now names an unrelated process.
		return age > ttl
	}
	if pidAlive(owner.PID) {
		return false
	}
	return age > ttl
}

// isRecordLockPayload reports whether data is a lock this package wrote: the
// JSON payload or the legacy "pid <n>" first line. OS lock files, which carry
// no owner payload, are not record locks.
func isRecordLockPayload(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '{' {
		var owner fileLock
		return json.Unmarshal(trimmed, &owner) == nil && owner.PID > 0
	}
	return bytes.HasPrefix(trimmed, []byte("pid "))
}

// BreakStaleLock removes one lock file when it is provably stale: the owner is
// dead past the TTL, another host wrote it, or a previous boot did. It reports
// whether the file was removed. A lock whose owner is alive is never removed,
// so this is not a way to steal a held lock; a `--break-lock` command, when
// one is added, should call this instead of deleting the file itself.
func BreakStaleLock(lockPath string) (bool, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("fsstore: read lock %s: %w", lockPath, err)
	}
	info, err := os.Stat(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("fsstore: inspect lock %s: %w", lockPath, err)
	}
	if !lockStale(decodeFileLock(data, info.ModTime()), info.ModTime(), time.Now()) {
		return false, nil
	}
	current, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("fsstore: reread lock %s: %w", lockPath, err)
	}
	if !bytes.Equal(current, data) {
		// Another writer refreshed it between the checks: not ours to break.
		return false, nil
	}
	if err := removeFile(lockPath); err != nil {
		return false, fmt.Errorf("fsstore: remove stale lock %s: %w", lockPath, err)
	}
	return true, nil
}

// localHost is the machine name written into lock files, resolved once.
var localHost = sync.OnceValue(func() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "unknown"
	}
	return name
})

// StaleLocks lists lock files under the data directory that a writer would
// treat as stale: the owner is dead and the TTL passed, the lock came from
// another host, or another boot wrote it. It never removes anything; doctor
// reports the paths so a human can inspect them.
func (s *Store) StaleLocks(ctx context.Context) ([]string, error) {
	stale := make([]string, 0)
	now := s.nowUTC()
	walkErr := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), lockFileSuffix) || !entry.Type().IsRegular() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if !isRecordLockPayload(data) {
			// OS locks (connectors/.locks) keep no owner payload and are
			// released by the kernel, so they are never reported here.
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		if lockStale(decodeFileLock(data, info.ModTime()), info.ModTime(), now) {
			stale = append(stale, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("fsstore: scan lock files: %w", walkErr)
	}
	sort.Strings(stale)
	return stale, nil
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

// LockSync takes the interprocess lock of the data directory, used to
// serialize git sync runs on one checkout. It reuses the owner-safe record
// lock: a live holder is never broken, while a dead pid past the TTL or a lock
// from another host is removed. The lock file is <data-dir>/sync.lock and is
// ignored by the managed .gitignore, so it is never published.
func (s *Store) LockSync(ctx context.Context) (func(), error) {
	return acquireFileLock(ctx, filepath.Join(s.root, "sync"))
}
