package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Sync reports divergence or commits local changes, rebases onto the remote and
// optionally pushes. Local mode has no sync provider; server mode is not wired.
//
// The whole run is serialized by the data-directory lock when the store
// implements ports.SyncLocker, so two syncs in one checkout cannot interleave
// commit, rebase, and push. A dirty working tree left by a concurrent writer is
// stashed around the rebase and restored afterwards; an interrupted rebase from
// an earlier run is aborted before this run starts. Conflicts never force and
// never discard data: the unmerged paths are returned and the caller maps them
// to exit code 3.
func (a *App) Sync(ctx context.Context, opts SyncOptions) (SyncResult, error) {
	if err := a.requireSync("sync"); err != nil {
		return SyncResult{}, err
	}
	release, err := a.lockSync(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	defer release()

	status, err := a.deps.Sync.Status(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	result := SyncResult{
		Provider:   status.Provider,
		Branch:     status.Branch,
		Ahead:      status.Ahead,
		Behind:     status.Behind,
		Dirty:      status.Dirty,
		Conflicts:  status.Conflicts,
		LastSynced: status.LastSynced,
	}
	if opts.StatusOnly {
		return result, nil
	}
	if len(status.Conflicts) > 0 {
		return result, fmt.Errorf("sync: %w: unresolved conflicts in %s", domain.ErrConflict, strings.Join(status.Conflicts, ", "))
	}

	// Commit before pulling: local writes belong to the shared history. The
	// stash in Pull covers a file written between this commit and the rebase.
	message := "brain sync: " + a.now().Format(time.RFC3339)
	if status.Dirty {
		if _, err := a.deps.Sync.Commit(ctx, message); err != nil {
			return result, err
		}
		result.Changed++
	}

	pulled, err := a.deps.Sync.Pull(ctx, ports.SyncOptions{Autostash: !opts.DisableAutostash})
	result.Conflicts = pulled.Conflicts
	result.Recovered = pulled.Recovered
	if err != nil {
		result.Dirty = dirtyAfterSync(ctx, a)
		return result, err
	}
	result.Changed += pulled.Changed
	if pulled.Changed > 0 {
		// Records written on other machines arrived as files; the search
		// projection must pick them up.
		a.markIndexDirty()
		if err := a.repairIndexIfDirty(ctx); err != nil {
			return result, fmt.Errorf("sync pulled %d commits but the search index refresh failed (run brain index): %w", pulled.Changed, err)
		}
		result.Reindexed = a.deps.Index != nil
	}

	if opts.Push {
		pushed, err := a.deps.Sync.Push(ctx)
		result.Conflicts = pushed.Conflicts
		if err != nil {
			result.Dirty = dirtyAfterSync(ctx, a)
			return result, err
		}
	}

	if after, afterErr := a.deps.Sync.Status(ctx); afterErr == nil {
		result.Ahead = after.Ahead
		result.Behind = after.Behind
		result.Dirty = after.Dirty
		result.Conflicts = after.Conflicts
		result.LastSynced = after.LastSynced
	}
	return result, nil
}

// lockSync serializes sync runs on the data directory when the store supports
// it. Stores without a lock keep working unchanged.
func (a *App) lockSync(ctx context.Context) (func(), error) {
	locker, ok := a.deps.Store.(ports.SyncLocker)
	if !ok {
		return func() {}, nil
	}
	return locker.LockSync(ctx)
}

func dirtyAfterSync(ctx context.Context, a *App) bool {
	status, err := a.deps.Sync.Status(ctx)
	if err != nil {
		return false
	}
	return status.Dirty
}

func (a *App) requireSync(feature string) error {
	if a.deps.Sync != nil {
		return nil
	}
	switch a.cfg.Mode {
	case config.ModeServer:
		return notImplemented(feature + " in server mode")
	case config.ModeLocal:
		return fmt.Errorf("%w: local mode has no remote; use git mode for sync", domain.ErrUnsupported)
	case config.ModeGit:
		return fmt.Errorf("%w: git sync provider is not configured", domain.ErrUnsupported)
	default:
		return fmt.Errorf("%w: unknown mode %q", domain.ErrUnsupported, a.cfg.Mode)
	}
}
