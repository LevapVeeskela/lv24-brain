package gitstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

func TestPushRetriesAfterNonFastForward(t *testing.T) {
	provider, repo, other := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, "projects/demo/local.md", "local\n")
	if _, err := provider.Commit(ctx, "brain: local"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	writeFile(t, other.dir, "projects/demo/remote.md", "remote\n")
	other.run("add", "--all")
	other.run("commit", "--quiet", "-m", "remote")
	other.run("push", "--quiet", "origin", "main")

	result, err := provider.Push(ctx)
	if err != nil {
		t.Fatalf("Push after a rejected first attempt: %v", err)
	}
	if result.Revision.Value == "" {
		t.Fatal("Revision.Value is empty")
	}
	repo.run("fetch", "--quiet", "origin")
	log := repo.run("log", "origin/main", "--format=%s")
	for _, want := range []string{"brain: local", "remote"} {
		if !strings.Contains(log, want) {
			t.Fatalf("remote history misses %q after the retry:\n%s", want, log)
		}
	}
}

func TestPushConflictReturnsPathsAndAbortsRebase(t *testing.T) {
	provider, repo, other := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, "projects/demo/state.md", "goal: ours\n")
	if _, err := provider.Commit(ctx, "brain: ours"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	writeFile(t, other.dir, "projects/demo/state.md", "goal: theirs\n")
	other.run("add", "--all")
	other.run("commit", "--quiet", "-m", "theirs")
	other.run("push", "--quiet", "origin", "main")

	result, err := provider.Push(ctx)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Push error = %v, want ErrConflict", err)
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0] != "projects/demo/state.md" {
		t.Fatalf("Conflicts = %v, want [projects/demo/state.md]", result.Conflicts)
	}
	if _, statErr := os.Stat(filepath.Join(repo.dir, ".git", "rebase-merge")); !os.IsNotExist(statErr) {
		t.Fatalf(".git/rebase-merge exists after the conflicted retry: %v", statErr)
	}
	if log := repo.run("log", "--format=%s"); !strings.Contains(log, "brain: ours") {
		t.Fatalf("the local commit was lost:\n%s", log)
	}
}

func TestPullAutostashRestoresDirtyTree(t *testing.T) {
	provider, repo, other := setup(t)
	ctx := context.Background()

	writeFile(t, other.dir, "projects/demo/remote.md", "remote\n")
	other.run("add", "--all")
	other.run("commit", "--quiet", "-m", "remote")
	other.run("push", "--quiet", "origin", "main")

	writeFile(t, repo.dir, "projects/demo/dirty.md", "concurrent write\n")
	result, err := provider.Pull(ctx, ports.SyncOptions{Autostash: true})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if len(result.Conflicts) > 0 {
		t.Fatalf("conflicts = %v", result.Conflicts)
	}
	if got := readFile(t, repo.dir, "projects/demo/dirty.md"); got != "concurrent write\n" {
		t.Fatalf("dirty file = %q, want it restored", got)
	}
	if got := readFile(t, repo.dir, "projects/demo/remote.md"); got != "remote\n" {
		t.Fatalf("remote file = %q", got)
	}
	if stashes := repo.run("stash", "list"); strings.TrimSpace(stashes) != "" {
		t.Fatalf("autostash was not restored:\n%s", stashes)
	}
}

// TestFailedStashPopKeepsTheStash proves a conflicted pop keeps the only copy
// of the local changes: the error names the ref and the paths, and the stash
// entry is still listed for a manual retry.
func TestFailedStashPopKeepsTheStash(t *testing.T) {
	provider, repo, _ := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, "projects/demo/state.md", "goal: stashed\n")
	repo.run("stash", "push", "--message=brain-sync-autostash test")
	writeFile(t, repo.dir, "projects/demo/state.md", "goal: newer\n")
	repo.run("add", "--all")
	repo.run("commit", "-qm", "newer")

	restored, conflicts, err := provider.restoreAutostash(ctx, true)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("restoreAutostash error = %v, want ErrConflict", err)
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1", restored)
	}
	if len(conflicts) != 1 || conflicts[0] != "projects/demo/state.md" {
		t.Fatalf("conflicts = %v, want the state file", conflicts)
	}
	if !strings.Contains(err.Error(), "stash@{0}") {
		t.Fatalf("error does not name the stash ref: %v", err)
	}
	if !strings.Contains(err.Error(), "projects/demo/state.md") {
		t.Fatalf("error does not name the conflicting path: %v", err)
	}
	if stashes := repo.run("stash", "list"); strings.TrimSpace(stashes) == "" {
		t.Fatal("the failed pop dropped the stash")
	}
}

func TestPullRecoversInterruptedRebaseAndAutostash(t *testing.T) {
	provider, repo, other := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, "projects/demo/state.md", "goal: ours\n")
	if _, err := provider.Commit(ctx, "brain: ours"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	writeFile(t, other.dir, "projects/demo/state.md", "goal: theirs\n")
	other.run("add", "--all")
	other.run("commit", "--quiet", "-m", "theirs")
	other.run("push", "--quiet", "origin", "main")

	// Simulate a run killed between the autostash and the rebase: a stash
	// created with our marker, then a conflicted rebase left in progress.
	writeFile(t, repo.dir, "projects/demo/dirty.md", "survives a crash\n")
	repo.run("stash", "push", "--include-untracked", "--message=brain-sync-autostash crashed")
	repo.runAllowFailure("pull", "--rebase")
	if _, err := os.Stat(filepath.Join(repo.dir, ".git", "rebase-merge")); err != nil {
		t.Fatalf("fixture did not leave a rebase in progress: %v", err)
	}

	result, err := provider.Pull(ctx, ports.SyncOptions{Autostash: true})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Pull error = %v, want ErrConflict after replaying the conflict", err)
	}
	if !result.Recovered {
		t.Fatal("Recovered = false, want the interrupted rebase and stash repaired")
	}
	if _, statErr := os.Stat(filepath.Join(repo.dir, ".git", "rebase-merge")); !os.IsNotExist(statErr) {
		t.Fatalf(".git/rebase-merge still exists: %v", statErr)
	}
	if got := readFile(t, repo.dir, "projects/demo/dirty.md"); got != "survives a crash\n" {
		t.Fatalf("stashed file = %q, want it restored", got)
	}
	if stashes := repo.run("stash", "list"); strings.TrimSpace(stashes) != "" {
		t.Fatalf("autostash was not restored:\n%s", stashes)
	}
	if log := repo.run("log", "--format=%s"); !strings.Contains(log, "brain: ours") {
		t.Fatalf("the local commit was lost:\n%s", log)
	}
}

func TestStatusReportsLastSyncedRevision(t *testing.T) {
	provider, repo, other := setup(t)
	ctx := context.Background()

	writeFile(t, other.dir, "projects/demo/remote.md", "remote\n")
	other.run("add", "--all")
	other.run("commit", "--quiet", "-m", "remote")
	other.run("push", "--quiet", "origin", "main")

	if _, err := provider.Pull(ctx, ports.SyncOptions{Autostash: true}); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	head := repo.run("rev-parse", "HEAD")
	status, err := provider.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.LastSynced.Kind != domain.SyncKindGit || status.LastSynced.Value != head {
		t.Fatalf("LastSynced = %+v, want %s", status.LastSynced, head)
	}
	if status.Dirty {
		t.Fatalf("Status.Dirty = true after a successful pull")
	}
}

// runAllowFailure runs git and returns the combined output without failing the
// test, for fixtures that intentionally stop on a conflict.
func (r *testRepo) runAllowFailure(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}
