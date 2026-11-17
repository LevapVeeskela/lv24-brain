package gitstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// syncStateFile records the revision of the last completed pull or push.
	// It lives inside .git, so it is never tracked and never dirties the tree.
	syncStateFile = "brain-sync.json"
	// autostashPrefix marks the stashes this package creates, so recovery
	// never restores a stash written by a human.
	autostashPrefix = "brain-sync-autostash"
	// maxRecoveredStashes bounds how many of our stashes one recovery run
	// restores; a crash loop cannot produce more than a handful.
	maxRecoveredStashes = 10
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
	records, err := p.conflictRecordPaths(ctx)
	if err != nil {
		return ports.SyncStatus{}, err
	}
	return ports.SyncStatus{
		Provider:        ProviderName,
		Branch:          branch,
		Ahead:           ahead,
		Behind:          behind,
		Dirty:           dirty,
		Conflicts:       conflicts,
		ConflictRecords: records,
		LastSynced:      p.lastSyncedRevision(ctx),
	}, nil
}

// RecoverInterruptedSync is the public entry point used by App.Sync before its
// conflict pre-check. Pull still calls the private helper so a direct Pull
// recovers a killed earlier run too.
func (p *Provider) RecoverInterruptedSync(ctx context.Context) (bool, error) {
	return p.recoverInterruptedSync(ctx)
}

// Pull repairs an interrupted earlier sync, fetches the remote and rebases
// local commits on top of the upstream branch. It never forces, merges, or
// resets. A dirty working tree is stashed before the rebase and restored after
// it. A rebase conflict aborts the rebase (so the repository is never stuck),
// restores the stash, and returns a ConflictError that wraps domain.ErrConflict
// together with the paths that conflicted.
//
// opts.Push pushes local commits after a successful rebase. Pull never creates
// a commit: opts.CommitMessage is ignored because a rebase requires a clean
// working tree. Callers that need to commit run Commit before or after Pull.
func (p *Provider) Pull(ctx context.Context, opts ports.SyncOptions) (ports.SyncResult, error) {
	recovered, err := p.recoverInterruptedSync(ctx)
	if err != nil {
		return ports.SyncResult{Recovered: recovered}, err
	}
	if _, err := p.run(ctx, "fetch", "--prune", p.remote); err != nil {
		return ports.SyncResult{Recovered: recovered}, err
	}
	branch, err := p.branch(ctx)
	if err != nil {
		return ports.SyncResult{Recovered: recovered}, err
	}
	_, behind, err := p.divergence(ctx, branch)
	if err != nil {
		return ports.SyncResult{Recovered: recovered}, err
	}

	// A new, empty data repository has no remote branch yet: there is
	// nothing to pull, and the first push creates it.
	if branch != "" && !p.remoteBranchExists(ctx, branch) {
		revision, err := p.revision(ctx)
		if err != nil {
			return ports.SyncResult{Recovered: recovered}, err
		}
		if !opts.Push {
			return ports.SyncResult{Revision: revision, Recovered: recovered}, nil
		}
		result, err := p.Push(ctx)
		result.Recovered = recovered
		return result, err
	}

	result, err := p.rebasePull(ctx, opts.Autostash, behind)
	result.Recovered = recovered
	if err != nil {
		return result, err
	}
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

// rebasePull stashes a dirty tree, runs the rebase, and restores the stash.
// On a rebase conflict it captures the unmerged paths, aborts the rebase, and
// restores the stash, so the caller always gets an usable repository back.
func (p *Provider) rebasePull(ctx context.Context, autostash bool, behind int) (ports.SyncResult, error) {
	stashed, err := p.stashIfDirty(ctx, autostash)
	if err != nil {
		return ports.SyncResult{}, err
	}
	if _, err := p.run(ctx, "pull", "--rebase"); err != nil {
		conflicts, _ := p.conflictedPaths(ctx)
		rebase := p.rebaseInProgress(ctx)
		if rebase {
			if _, abortErr := p.run(ctx, "rebase", "--abort"); abortErr != nil {
				return ports.SyncResult{Conflicts: conflicts}, fmt.Errorf("gitstore: abort the conflicted rebase: %w", abortErr)
			}
		}
		_, stashConflicts, restoreErr := p.restoreAutostash(ctx, stashed)
		paths := uniquePaths(append(conflicts, stashConflicts...))
		if restoreErr != nil {
			// The autostash was kept: its error names the ref and the paths.
			return ports.SyncResult{Conflicts: paths}, fmt.Errorf("pull stopped and the autostash was kept: %w", restoreErr)
		}
		if len(paths) > 0 || rebase {
			return ports.SyncResult{Conflicts: paths}, &ConflictError{Operation: "pull", Paths: paths, Aborted: rebase}
		}
		return ports.SyncResult{}, err
	}

	_, stashConflicts, err := p.restoreAutostash(ctx, stashed)
	if err != nil {
		return ports.SyncResult{Conflicts: stashConflicts}, err
	}
	revision, err := p.revision(ctx)
	if err != nil {
		return ports.SyncResult{}, err
	}
	p.recordSyncedRevision(ctx, revision)
	return ports.SyncResult{Revision: revision, Changed: behind}, nil
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

// Push publishes local commits to the configured upstream. It never forces.
// A rejected push is retried up to the configured attempts (3 by default, with
// backoff), fetching and rebasing onto the upstream between attempts. A rebase
// conflict during a retry returns a ConflictError, so the caller can report
// exit code 3 instead of a generic failure. Changed reports the number of
// commits that were ahead before the successful push.
func (p *Provider) Push(ctx context.Context) (ports.SyncResult, error) {
	branch, err := p.branch(ctx)
	if err != nil {
		return ports.SyncResult{}, err
	}
	attempts := p.pushAttempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		ahead, _, err := p.divergence(ctx, branch)
		if err != nil {
			return ports.SyncResult{}, err
		}
		if _, err := p.run(ctx, "push"); err != nil {
			lastErr = err
			if attempt == attempts || !retryablePushError(err) {
				break
			}
			if err := sleepContext(ctx, time.Duration(attempt)*p.pushBackoff); err != nil {
				return ports.SyncResult{}, err
			}
			rebaseResult, err := p.rebaseAfterRejectedPush(ctx)
			if err != nil {
				return rebaseResult, err
			}
			continue
		}
		revision, err := p.revision(ctx)
		if err != nil {
			return ports.SyncResult{}, err
		}
		p.recordSyncedRevision(ctx, revision)
		return ports.SyncResult{Revision: revision, Changed: ahead}, nil
	}
	return ports.SyncResult{}, lastErr
}

// rebaseAfterRejectedPush fetches the upstream and rebases local commits on
// top of it before another push attempt. A conflict aborts the rebase and
// returns the unmerged paths.
func (p *Provider) rebaseAfterRejectedPush(ctx context.Context) (ports.SyncResult, error) {
	if _, err := p.run(ctx, "fetch", "--prune", p.remote); err != nil {
		return ports.SyncResult{}, err
	}
	branch, err := p.branch(ctx)
	if err != nil {
		return ports.SyncResult{}, err
	}
	if branch == "" || !p.remoteBranchExists(ctx, branch) {
		return ports.SyncResult{}, nil
	}
	return p.rebasePull(ctx, true, 0)
}

// recoverInterruptedSync repairs a run killed between stash and push: an
// in-progress rebase is aborted and a stash created by this package is
// restored. A user-created stash is never touched.
func (p *Provider) recoverInterruptedSync(ctx context.Context) (bool, error) {
	recovered := false
	if p.rebaseInProgress(ctx) {
		if _, err := p.run(ctx, "rebase", "--abort"); err != nil {
			return recovered, fmt.Errorf("gitstore: abort the interrupted rebase: %w", err)
		}
		recovered = true
	}
	restored, conflicts, err := p.restoreAutostash(ctx, true)
	if restored > 0 {
		recovered = true
	}
	if len(conflicts) > 0 {
		// err already names the kept stash ref and the conflicting paths.
		if err != nil {
			return recovered, err
		}
		return recovered, &ConflictError{Operation: "stash restore", Paths: conflicts}
	}
	return recovered, err
}

// stashIfDirty saves the working tree when autostash is enabled and the tree
// has changes, so the rebase can run. It reports whether a stash was created.
func (p *Provider) stashIfDirty(ctx context.Context, autostash bool) (bool, error) {
	if !autostash {
		return false, nil
	}
	dirty, err := p.isDirty(ctx)
	if err != nil {
		return false, err
	}
	if !dirty {
		return false, nil
	}
	message := autostashPrefix + " " + time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := p.run(ctx, "stash", "push", "--include-untracked", "--message="+message); err != nil {
		return false, fmt.Errorf("gitstore: stash the working tree: %w", err)
	}
	return true, nil
}

// restoreAutostash pops our stashes until none is left. When enabled is false
// it does nothing. A failed pop never drops the stash: the entry keeps the only
// copy of the local changes, so the error names the ref and the conflicting
// paths and asks for a manual resolution.
func (p *Provider) restoreAutostash(ctx context.Context, enabled bool) (int, []string, error) {
	if !enabled {
		return 0, nil, nil
	}
	for restored := 0; restored < maxRecoveredStashes; restored++ {
		ref, ok, err := p.autostashRef(ctx)
		if err != nil {
			return restored, nil, err
		}
		if !ok {
			return restored, nil, nil
		}
		if _, err := p.run(ctx, "stash", "pop", ref); err != nil {
			conflicts, _ := p.conflictedPaths(ctx)
			message := fmt.Sprintf("gitstore: git stash pop %s failed (%v); the stash %s was kept because it still holds the local changes",
				ref, err, ref)
			if len(conflicts) > 0 {
				message += " conflicting with " + strings.Join(conflicts, ", ")
			}
			message += fmt.Sprintf("; resolve them, then restore it with `git stash pop %s` or drop it with `git stash drop %s`: ",
				ref, ref)
			return restored + 1, conflicts, fmt.Errorf("%s%w", message,
				&ConflictError{Operation: "stash restore", Paths: conflicts})
		}
	}
	return maxRecoveredStashes, nil, nil
}

// lockSync takes the interprocess sync lock of the checkout through the same
// owner-aware implementation the data store uses (pid, host, TTL, token), so a
// publish and a sync contend on <root>/sync.lock and neither can break or
// delete the other's lock.
func (p *Provider) lockSync(ctx context.Context) (func(), error) {
	return fsstore.AcquirePathLock(ctx, filepath.Join(p.dir, "sync.lock"))
}

// autostashRef finds the newest stash created by this package.
func (p *Provider) autostashRef(ctx context.Context) (string, bool, error) {
	out, err := p.run(ctx, "stash", "list", "--format=%gd%x09%s")
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(out, "\n") {
		ref, message, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || strings.TrimSpace(ref) == "" {
			continue
		}
		// %s is the stash subject, "On <branch>: <message>", so the marker is
		// matched anywhere in it rather than at the start.
		if strings.Contains(message, autostashPrefix) {
			return strings.TrimSpace(ref), true, nil
		}
	}
	return "", false, nil
}

// rebaseInProgress reports whether .git holds a rebase state directory.
func (p *Provider) rebaseInProgress(ctx context.Context) bool {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		out, err := p.run(ctx, "rev-parse", "--git-path", name)
		if err != nil {
			return false
		}
		path := strings.TrimSpace(out)
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(p.dir, path)
		}
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// remoteBranchExists reports whether the last fetch saw branch on the remote.
func (p *Provider) remoteBranchExists(ctx context.Context, branch string) bool {
	_, err := p.run(ctx, "rev-parse", "--verify", "--quiet", "refs/remotes/"+p.remote+"/"+branch)
	return err == nil
}

// recordSyncedRevision stores the revision of a completed sync for
// `brain sync --status`. The write is best effort: a failed status file never
// fails a successful sync.
func (p *Provider) recordSyncedRevision(ctx context.Context, revision domain.SyncRevision) {
	if revision.Value == "" {
		return
	}
	payload, err := json.Marshal(struct {
		Revision string    `json:"revision"`
		At       time.Time `json:"at"`
	}{Revision: revision.Value, At: revision.At})
	if err != nil {
		return
	}
	path := p.syncStatePath(ctx)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, append(payload, '\n'), 0o644)
}

// syncStatePath resolves <git-dir>/brain-sync.json. A worktree puts the git
// directory elsewhere, so the path comes from git rather than from p.dir.
func (p *Provider) syncStatePath(ctx context.Context) string {
	out, err := p.run(ctx, "rev-parse", "--git-path", syncStateFile)
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(out)
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(p.dir, path)
	}
	return path
}

func (p *Provider) lastSyncedRevision(ctx context.Context) domain.SyncRevision {
	path := p.syncStatePath(ctx)
	if path == "" {
		return domain.SyncRevision{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.SyncRevision{}
	}
	var state struct {
		Revision string    `json:"revision"`
		At       time.Time `json:"at"`
	}
	if err := json.Unmarshal(raw, &state); err != nil || strings.TrimSpace(state.Revision) == "" {
		return domain.SyncRevision{}
	}
	return domain.SyncRevision{Kind: domain.SyncKindGit, Value: state.Revision, At: state.At.UTC()}
}

// retryablePushError reports whether a rejected push may succeed after a fetch
// and rebase. Git gives no typed errors, so this inspects the sanitized stderr
// for the non-fast-forward and transport failures that a retry can fix.
func retryablePushError(err error) bool {
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	stderr := strings.ToLower(commandErr.Stderr)
	for _, needle := range []string{
		"non-fast-forward",
		"fetch first",
		"failed to push some refs",
		"cannot lock ref",
		"could not resolve host",
		"unable to access",
		"failed to connect",
		"connection timed out",
		"connection reset",
		"connection refused",
		"network is unreachable",
		"temporary failure in name resolution",
		"rpc failed",
		"remote end hung up",
		"early eof",
		"operation timed out",
		"tls",
		"ssl",
	} {
		if strings.Contains(stderr, needle) {
			return true
		}
	}
	return false
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}
