package gitstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

type testRepo struct {
	t   *testing.T
	dir string
}

func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not available")
	}
	if out, err := exec.Command(path, "--version").CombinedOutput(); err != nil {
		t.Skipf("git is not usable: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global.gitconfig"))
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	return path
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

func initRepo(t *testing.T, dir string, bare bool) *testRepo {
	t.Helper()
	args := []string{"init", "--quiet"}
	if bare {
		args = append(args, "--bare")
	}
	args = append(args, dir)
	runGit(t, filepath.Dir(dir), args...)
	runGit(t, dir, "symbolic-ref", "HEAD", "refs/heads/main")

	repo := &testRepo{t: t, dir: dir}
	if !bare {
		repo.configure()
	}
	return repo
}

func (r *testRepo) configure() {
	r.t.Helper()
	r.run("config", "user.name", "Brain Test")
	r.run("config", "user.email", "brain@example.test")
	r.run("config", "commit.gpgsign", "false")
	r.run("config", "core.autocrlf", "false")
	r.run("config", "core.safecrlf", "false")
}

func (r *testRepo) run(args ...string) string {
	r.t.Helper()
	return runGit(r.t, r.dir, args...)
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func newTestProvider(t *testing.T, dir string) *Provider {
	t.Helper()
	provider, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	return provider
}

// setup creates a bare remote with one seed commit plus two clones: the
// provider's working clone and a second clone used to publish remote changes.
func setup(t *testing.T) (*Provider, *testRepo, *testRepo) {
	t.Helper()
	requireGit(t)

	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	initRepo(t, bare, true)

	seed := initRepo(t, filepath.Join(root, "seed"), false)
	writeFile(t, seed.dir, "brain.yaml", "mode: git\n")
	writeFile(t, seed.dir, "projects/demo/state.md", "goal: base\n")
	seed.run("add", "--all")
	seed.run("commit", "--quiet", "-m", "seed")
	seed.run("remote", "add", "origin", bare)
	seed.run("push", "--quiet", "-u", "origin", "main")

	clone := filepath.Join(root, "clone")
	runGit(t, root, "clone", "--quiet", bare, clone)
	providerRepo := &testRepo{t: t, dir: clone}
	providerRepo.configure()

	other := filepath.Join(root, "other")
	runGit(t, root, "clone", "--quiet", bare, other)
	otherRepo := &testRepo{t: t, dir: other}
	otherRepo.configure()

	return newTestProvider(t, clone), providerRepo, otherRepo
}

func TestStatusClean(t *testing.T) {
	provider, _, _ := setup(t)

	status, err := provider.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Provider != ProviderName {
		t.Errorf("Provider = %q, want %q", status.Provider, ProviderName)
	}
	if status.Branch != "main" {
		t.Errorf("Branch = %q, want main", status.Branch)
	}
	if status.Dirty {
		t.Error("Dirty = true, want false")
	}
	if status.Ahead != 0 || status.Behind != 0 {
		t.Errorf("divergence = %d/%d, want 0/0", status.Ahead, status.Behind)
	}
	if len(status.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none", status.Conflicts)
	}
}

func TestStatusDirty(t *testing.T) {
	provider, repo, _ := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, "projects/demo/note.md", "note\n")
	status, err := provider.Status(ctx)
	if err != nil {
		t.Fatalf("Status after write: %v", err)
	}
	if !status.Dirty {
		t.Error("Dirty = false, want true for untracked file")
	}

	if _, err := provider.Commit(ctx, "brain: note"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	status, err = provider.Status(ctx)
	if err != nil {
		t.Fatalf("Status after commit: %v", err)
	}
	if status.Dirty {
		t.Error("Dirty = true, want false after commit")
	}
}

func TestStatusAheadAndBehind(t *testing.T) {
	provider, repo, other := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, "projects/demo/local.md", "local\n")
	if _, err := provider.Commit(ctx, "brain: local"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	status, err := provider.Status(ctx)
	if err != nil {
		t.Fatalf("Status ahead: %v", err)
	}
	if status.Ahead != 1 || status.Behind != 0 {
		t.Errorf("divergence = %d/%d, want 1/0", status.Ahead, status.Behind)
	}

	writeFile(t, other.dir, "projects/demo/remote.md", "remote\n")
	other.run("add", "--all")
	other.run("commit", "--quiet", "-m", "remote change")
	other.run("push", "--quiet", "origin", "main")
	repo.run("fetch", "--quiet", "origin")

	status, err = provider.Status(ctx)
	if err != nil {
		t.Fatalf("Status behind: %v", err)
	}
	if status.Ahead != 1 || status.Behind != 1 {
		t.Errorf("divergence = %d/%d, want 1/1", status.Ahead, status.Behind)
	}
}

func TestCommitStagesDataAndSkipsCleanTree(t *testing.T) {
	provider, repo, _ := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, "projects/demo/state.md", "goal: updated\n")
	writeFile(t, repo.dir, "journal/alice.jsonl", "{\"event\":\"note\"}\n")

	revision, err := provider.Commit(ctx, "brain: update state")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if revision.Kind != domain.SyncKindGit {
		t.Errorf("Kind = %q, want %q", revision.Kind, domain.SyncKindGit)
	}
	if revision.Value == "" {
		t.Error("Value is empty")
	}
	if revision.At.IsZero() || revision.At.After(time.Now().Add(time.Minute)) {
		t.Errorf("At = %v, want a recent commit time", revision.At)
	}
	if got := repo.run("log", "-1", "--format=%s"); got != "brain: update state" {
		t.Errorf("commit subject = %q", got)
	}

	files := repo.run("ls-files")
	for _, want := range []string{"projects/demo/state.md", "journal/alice.jsonl"} {
		if !strings.Contains(files, want) {
			t.Errorf("ls-files missing %q:\n%s", want, files)
		}
	}

	again, err := provider.Commit(ctx, "brain: no changes")
	if err != nil {
		t.Fatalf("Commit without changes: %v", err)
	}
	if again.Value != revision.Value {
		t.Errorf("clean Commit moved HEAD from %s to %s", revision.Value, again.Value)
	}
	if got := repo.run("rev-list", "--count", "HEAD"); got != "2" {
		t.Errorf("commit count = %s, want 2 (seed + update)", got)
	}
}

func TestCommitRespectsGitignore(t *testing.T) {
	provider, repo, _ := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, ".gitignore", "index/\nsecrets/\n")
	writeFile(t, repo.dir, "index/cache.db", "derived")
	writeFile(t, repo.dir, "secrets/token.txt", "must-not-commit")
	writeFile(t, repo.dir, "projects/demo/state.md", "goal: ignored-files\n")

	if _, err := provider.Commit(ctx, "brain: ignored files"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	files := repo.run("ls-files")
	for _, unwanted := range []string{"index/cache.db", "secrets/token.txt"} {
		if strings.Contains(files, unwanted) {
			t.Errorf("ls-files contains ignored path %q:\n%s", unwanted, files)
		}
	}
}

func TestCommitRejectsEmptyMessage(t *testing.T) {
	provider, _, _ := setup(t)

	if _, err := provider.Commit(context.Background(), "   "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Commit(empty) error = %v, want ErrValidation", err)
	}
}

func TestPullRebasesRemoteChanges(t *testing.T) {
	provider, repo, other := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, "projects/demo/local.md", "local\n")
	if _, err := provider.Commit(ctx, "brain: local"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	writeFile(t, other.dir, "projects/demo/remote.md", "remote\n")
	other.run("add", "--all")
	other.run("commit", "--quiet", "-m", "remote change")
	other.run("push", "--quiet", "origin", "main")

	result, err := provider.Pull(ctx, ports.SyncOptions{})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if result.Changed != 1 {
		t.Errorf("Changed = %d, want 1", result.Changed)
	}
	if len(result.Conflicts) != 0 {
		t.Errorf("Conflicts = %v, want none", result.Conflicts)
	}
	if result.Revision.Kind != domain.SyncKindGit || result.Revision.Value == "" {
		t.Errorf("Revision = %+v, want a git revision", result.Revision)
	}
	if got := readFile(t, repo.dir, "projects/demo/remote.md"); got != "remote\n" {
		t.Errorf("remote file = %q", got)
	}
	log := repo.run("log", "--format=%s")
	if !strings.Contains(log, "brain: local") {
		t.Errorf("rebase lost the local commit:\n%s", log)
	}

	status, err := provider.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	// The rebased local commit is still unpushed, so ahead stays 1.
	if status.Ahead != 1 || status.Behind != 0 || status.Dirty {
		t.Errorf("status after pull = %+v, want ahead/behind 1/0 and clean", status)
	}
}

func TestPullWithPushOptionPublishesLocalCommit(t *testing.T) {
	provider, repo, other := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, "projects/demo/local.md", "local\n")
	if _, err := provider.Commit(ctx, "brain: local"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	result, err := provider.Pull(ctx, ports.SyncOptions{Push: true})
	if err != nil {
		t.Fatalf("Pull(Push): %v", err)
	}
	if result.Changed != 1 {
		t.Errorf("Changed = %d, want 1 pushed commit", result.Changed)
	}

	other.run("fetch", "--quiet", "origin")
	log := other.run("log", "origin/main", "--format=%s")
	if !strings.Contains(log, "brain: local") {
		t.Errorf("remote did not receive the local commit:\n%s", log)
	}
}

func TestPushPublishesAheadCommits(t *testing.T) {
	provider, repo, other := setup(t)
	ctx := context.Background()

	writeFile(t, repo.dir, "projects/demo/local.md", "local\n")
	if _, err := provider.Commit(ctx, "brain: push me"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	result, err := provider.Push(ctx)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if result.Changed != 1 {
		t.Errorf("Changed = %d, want 1", result.Changed)
	}
	if result.Revision.Value == "" {
		t.Error("Revision.Value is empty")
	}

	other.run("fetch", "--quiet", "origin")
	if log := other.run("log", "origin/main", "--format=%s"); !strings.Contains(log, "brain: push me") {
		t.Errorf("remote did not receive the commit:\n%s", log)
	}

	repeat, err := provider.Push(ctx)
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if repeat.Changed != 0 {
		t.Errorf("second Push Changed = %d, want 0", repeat.Changed)
	}
}

func TestPullConflictReportsPaths(t *testing.T) {
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

	result, err := provider.Pull(ctx, ports.SyncOptions{})
	if err == nil {
		t.Fatal("Pull succeeded, want conflict")
	}
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("Pull error = %v, want ErrConflict", err)
	}
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) {
		t.Fatalf("Pull error type = %T, want *ConflictError", err)
	}
	if len(result.Conflicts) != 1 || result.Conflicts[0] != "projects/demo/state.md" {
		t.Errorf("Conflicts = %v, want [projects/demo/state.md]", result.Conflicts)
	}

	content := readFile(t, repo.dir, "projects/demo/state.md")
	if !strings.Contains(content, "<<<<<<<") {
		t.Errorf("working tree has no conflict markers:\n%s", content)
	}

	status, statusErr := provider.Status(ctx)
	if statusErr != nil {
		t.Fatalf("Status: %v", statusErr)
	}
	if len(status.Conflicts) != 1 || status.Conflicts[0] != "projects/demo/state.md" {
		t.Errorf("Status.Conflicts = %v, want the unmerged path", status.Conflicts)
	}
	if !status.Dirty {
		t.Error("Status.Dirty = false, want true during a conflict")
	}
}

func TestStatusOutsideRepository(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	provider := newTestProvider(t, dir)

	if _, err := provider.Status(context.Background()); err == nil {
		t.Fatal("Status outside a repository succeeded, want error")
	}
}

func TestOpenValidation(t *testing.T) {
	dir := t.TempDir()

	if _, err := Open("   ", Options{}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Open(empty) error = %v, want ErrValidation", err)
	}
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(file, Options{}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Open(file) error = %v, want ErrValidation", err)
	}
	if _, err := Open(dir, Options{Timeout: -time.Second}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Open(negative timeout) error = %v, want ErrValidation", err)
	}
	if _, err := Open(dir, Options{Remote: "--upload-pack=evil"}); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("Open(flag-like remote) error = %v, want ErrValidation", err)
	}
}

func TestSanitizeRedactsCredentials(t *testing.T) {
	fakeGitHubToken := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789"
	fakeGitLabToken := "glpat-" + "ABCDEFGHIJKLMNOPQRST"
	cases := []struct {
		name string
		in   string
		bad  []string
		want []string
	}{
		{
			name: "url userinfo",
			in:   "fatal: unable to access 'https://alice:s3cret@example.com/repo.git/'",
			bad:  []string{"alice", "s3cret"},
			want: []string{"https://***@example.com/repo.git/"},
		},
		{
			name: "token query parameter",
			in:   "https://example.com/repo?private_token=abc123&ref=main",
			bad:  []string{"abc123"},
			want: []string{"private_token=***", "ref=main"},
		},
		{
			name: "known token shape",
			in:   "remote: invalid token " + fakeGitHubToken,
			bad:  []string{fakeGitHubToken},
			want: []string{"***"},
		},
		{
			name: "gitlab token",
			in:   "auth failed for " + fakeGitLabToken,
			bad:  []string{fakeGitLabToken},
			want: []string{"***"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitize(tc.in)
			for _, bad := range tc.bad {
				if strings.Contains(got, bad) {
					t.Errorf("sanitize(%q) leaked %q: %q", tc.in, bad, got)
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("sanitize(%q) = %q, want substring %q", tc.in, got, want)
				}
			}
		})
	}
}

func TestSanitizeCapsLength(t *testing.T) {
	got := sanitize(strings.Repeat("x", maxErrorBytes+100))
	if len(got) > maxErrorBytes+3 {
		t.Errorf("sanitized length = %d, want cap near %d", len(got), maxErrorBytes)
	}
}
