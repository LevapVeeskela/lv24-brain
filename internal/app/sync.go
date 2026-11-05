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
func (a *App) Sync(ctx context.Context, opts SyncOptions) (SyncResult, error) {
	if err := a.requireSync("sync"); err != nil {
		return SyncResult{}, err
	}
	status, err := a.deps.Sync.Status(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	result := SyncResult{
		Provider:  status.Provider,
		Ahead:     status.Ahead,
		Behind:    status.Behind,
		Conflicts: status.Conflicts,
	}
	if opts.StatusOnly {
		return result, nil
	}
	if len(status.Conflicts) > 0 {
		return result, fmt.Errorf("sync: %w: unresolved conflicts in %s", domain.ErrConflict, strings.Join(status.Conflicts, ", "))
	}

	// Commit before pulling: git rebase requires a clean working tree, and the
	// data repository has no stash workflow.
	message := "brain sync: " + a.now().Format(time.RFC3339)
	if status.Dirty {
		if _, err := a.deps.Sync.Commit(ctx, message); err != nil {
			return result, err
		}
		result.Changed++
	}

	pulled, err := a.deps.Sync.Pull(ctx, ports.SyncOptions{})
	if err != nil {
		result.Conflicts = pulled.Conflicts
		return result, err
	}
	result.Changed += pulled.Changed

	if opts.Push {
		pushed, err := a.deps.Sync.Push(ctx)
		if err != nil {
			return result, err
		}
		result.Conflicts = pushed.Conflicts
	}

	if after, afterErr := a.deps.Sync.Status(ctx); afterErr == nil {
		result.Ahead = after.Ahead
		result.Behind = after.Behind
		result.Conflicts = after.Conflicts
	}
	return result, nil
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
