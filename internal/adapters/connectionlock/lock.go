// Package connectionlock provides non-blocking, cross-process connection locks.
package connectionlock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Locker stores one stable lock file per connection under dataDir. OS locks
// are released by the kernel if the owning process exits unexpectedly.
type Locker struct {
	dir string
}

// lockIgnoreContent keeps the lock directory out of the data repository: the
// files are machine-local OS locks, not user data. The pattern also ignores
// this .gitignore itself, so no lock artifact is ever published.
const lockIgnoreContent = "# lv24-brain: local OS lock files; nothing in this directory is committed.\n*\n"

// New creates the lock directory under dataDir. Lock files are intentionally
// retained: unlinking a locked file can let another process lock a new inode.
func New(dataDir string) (*Locker, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, domain.NewValidationError("connectionLock.dataDir", "is required")
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("connectionlock: resolve data directory: %w", err)
	}
	dir := filepath.Join(root, "connectors", ".locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("connectionlock: create lock directory: %w", err)
	}
	ignorePath := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(ignorePath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(ignorePath, []byte(lockIgnoreContent), 0o600); err != nil {
			return nil, fmt.Errorf("connectionlock: write lock ignore file: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("connectionlock: inspect lock ignore file: %w", err)
	}
	return &Locker{dir: dir}, nil
}

// TryLock acquires a connection's lock without waiting. A held lock returns
// domain.ErrConflict. The returned release function is idempotent.
func (l *Locker) TryLock(ctx context.Context, connectionID string) (func() error, error) {
	if l == nil {
		return nil, domain.NewValidationError("connectionLock", "is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(l.dir, connectionID+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("connectionlock: open lock: %w", err)
	}
	unlock, err := lockFile(file)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, errLockBusy) {
			return nil, fmt.Errorf("%w: connection %q is already running", domain.ErrConflict, connectionID)
		}
		return nil, fmt.Errorf("connectionlock: acquire %q: %w", connectionID, err)
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			unlockErr := unlock()
			closeErr := file.Close()
			releaseErr = errors.Join(unlockErr, closeErr)
		})
		return releaseErr
	}, nil
}
