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

	// Repair a run killed between stash and push before reading conflicts.
	// A leftover rebase otherwise surfaces as unmerged paths with a detached
	// HEAD, and the old pre-check returned exit 3 without ever recovering.
	var recovered bool
	if !opts.StatusOnly {
		if recoverer, ok := a.deps.Sync.(ports.SyncRecoverer); ok {
			recovered, err = recoverer.RecoverInterruptedSync(ctx)
			if err != nil {
				return SyncResult{Recovered: recovered}, err
			}
		}
	}
	// Make the clone safe before pulling. Preflight can upgrade .gitattributes,
	// so it runs before the status read that decides what to commit. --status is
	// read-only and skips both preflight and recovery.
	if !opts.StatusOnly {
		if err := a.syncPreflight(ctx); err != nil {
			return SyncResult{Recovered: recovered}, err
		}
	}
	status, err := a.deps.Sync.Status(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	result := SyncResult{
		Provider:        status.Provider,
		Branch:          status.Branch,
		Ahead:           status.Ahead,
		Behind:          status.Behind,
		Dirty:           status.Dirty,
		Conflicts:       status.Conflicts,
		ConflictRecords: status.ConflictRecords,
		LastSynced:      status.LastSynced,
		Recovered:       recovered,
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
	result.ConflictRecords = appendUnique(result.ConflictRecords, pulled.ConflictRecords...)
	result.Recovered = result.Recovered || pulled.Recovered
	if err != nil {
		result.Dirty = dirtyAfterSync(ctx, a)
		return result, err
	}
	result.Changed += pulled.Changed

	// Merge drivers write conflict records during the rebase, after the commit
	// above. Commit them now so both machines receive them in one sync.
	recordMessage := "brain sync: conflict records " + a.now().Format(time.RFC3339)
	if err := a.commitConflictRecords(ctx, &result, recordMessage); err != nil {
		return result, err
	}
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
		result.ConflictRecords = appendUnique(result.ConflictRecords, pushed.ConflictRecords...)
		if err != nil {
			result.Dirty = dirtyAfterSync(ctx, a)
			return result, err
		}
		// A rejected push rebases and may write new conflict records. Commit
		// and push those too so nothing is left dirty after a successful sync.
		before := len(result.ConflictRecords)
		if err := a.commitConflictRecords(ctx, &result, recordMessage); err != nil {
			return result, err
		}
		if len(result.ConflictRecords) > before {
			again, pushErr := a.deps.Sync.Push(ctx)
			if pushErr != nil {
				result.Dirty = dirtyAfterSync(ctx, a)
				return result, pushErr
			}
			result.Changed += again.Changed
			result.Conflicts = again.Conflicts
		}
	}

	if after, afterErr := a.deps.Sync.Status(ctx); afterErr == nil {
		result.Ahead = after.Ahead
		result.Behind = after.Behind
		result.Dirty = after.Dirty
		result.Conflicts = after.Conflicts
		result.ConflictRecords = after.ConflictRecords
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
