package gitstore

import (
	"context"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Status reports branch, divergence, dirtiness, and conflicts without touching
// the network or the working tree. Divergence is measured against the last
// fetched remote-tracking state, so callers that want fresh numbers should run
// Pull first or fetch explicitly.
func (p *Provider) Status(ctx context.Context) (ports.SyncStatus, error) {
	branch, err := p.branch(ctx)
	if err != nil {
		return ports.SyncStatus{}, err
	}
	dirty, err := p.isDirty(ctx)
	if err != nil {
		return ports.SyncStatus{}, err
	}
	conflicts, err := p.conflictedPaths(ctx)
	if err != nil {
		return ports.SyncStatus{}, err
	}
	ahead, behind, err := p.divergence(ctx, branch)
	if err != nil {
		return ports.SyncStatus{}, err
	}
	return ports.SyncStatus{
		Provider:  ProviderName,
		Branch:    branch,
		Ahead:     ahead,
		Behind:    behind,
		Dirty:     dirty,
		Conflicts: conflicts,
	}, nil
}

// Pull fetches the remote and rebases local commits on top of the upstream
// branch. It never forces, merges, or resets. A rebase conflict returns a
// ConflictError that wraps domain.ErrConflict together with the unmerged paths;
// the repository is left in the conflicted state for manual resolution.
//
// opts.Push pushes local commits after a successful rebase. Pull never creates
// a commit: opts.CommitMessage is ignored because a rebase requires a clean
// working tree. Callers that need to commit run Commit before or after Pull.
func (p *Provider) Pull(ctx context.Context, opts ports.SyncOptions) (ports.SyncResult, error) {
	if _, err := p.run(ctx, "fetch", "--prune", p.remote); err != nil {
		return ports.SyncResult{}, err
	}
	branch, err := p.branch(ctx)
	if err != nil {
		return ports.SyncResult{}, err
	}
	_, behind, err := p.divergence(ctx, branch)
	if err != nil {
		return ports.SyncResult{}, err
	}

	if _, err := p.run(ctx, "pull", "--rebase"); err != nil {
		conflicts, conflictErr := p.conflictedPaths(ctx)
		if conflictErr != nil {
			return ports.SyncResult{}, err
		}
		if len(conflicts) > 0 {
			return ports.SyncResult{Conflicts: conflicts}, &ConflictError{
				Operation: "pull",
				Paths:     conflicts,
			}
		}
		return ports.SyncResult{}, err
	}

	revision, err := p.revision(ctx)
	if err != nil {
		return ports.SyncResult{}, err
	}
	result := ports.SyncResult{Revision: revision, Changed: behind}
	if !opts.Push {
		return result, nil
	}

	pushed, err := p.Push(ctx)
	if err != nil {
		return result, err
	}
	result.Revision = pushed.Revision
	result.Changed += pushed.Changed
	result.Conflicts = pushed.Conflicts
	return result, nil
}

// Commit stages every non-ignored change under the repository root and creates
// one commit. When nothing is staged it returns the current revision without
// creating an empty commit. Commit messages are passed as a single argv value,
// so they cannot be interpreted as flags or shell syntax.
func (p *Provider) Commit(ctx context.Context, message string) (domain.SyncRevision, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return domain.SyncRevision{}, domain.NewValidationError("gitstore.message", "is required")
	}

	if _, err := p.run(ctx, "add", "--all", "--"); err != nil {
		return domain.SyncRevision{}, err
	}
	staged, err := p.run(ctx, "diff", "--cached", "--name-only", "-z", "--")
	if err != nil {
		return domain.SyncRevision{}, err
	}
	if len(splitNul(staged)) == 0 {
		revision, err := p.revision(ctx)
		if err != nil {
			return domain.SyncRevision{}, err
		}
		return revision, nil
	}

	if _, err := p.run(ctx, "commit", "--message="+message); err != nil {
		return domain.SyncRevision{}, err
	}
	return p.revision(ctx)
}

// Push publishes local commits to the configured upstream. It never forces and
// does not set an upstream automatically; a branch without tracking
// information fails with a sanitized git error. Changed reports the number of
// commits that were ahead before the push.
func (p *Provider) Push(ctx context.Context) (ports.SyncResult, error) {
	branch, err := p.branch(ctx)
	if err != nil {
		return ports.SyncResult{}, err
	}
	ahead, _, err := p.divergence(ctx, branch)
	if err != nil {
		return ports.SyncResult{}, err
	}

	if _, err := p.run(ctx, "push"); err != nil {
		return ports.SyncResult{}, err
	}
	revision, err := p.revision(ctx)
	if err != nil {
		return ports.SyncResult{}, err
	}
	return ports.SyncResult{Revision: revision, Changed: ahead}, nil
}
