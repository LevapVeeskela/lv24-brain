// Package gitstore implements ports.SyncProvider on top of the git CLI.
//
// The adapter shells out to git with an explicit argument vector, so no shell
// is involved and user input never becomes part of a command string. Every git
// call is bounded by the caller context and a per-command timeout, and error
// text is sanitized before it is returned so remote URLs with embedded
// credentials and access tokens do not leak into logs.
//
// The provider assumes dir is the private, data-only brain repository described
// by docs/product-design.md. Commit stages every non-ignored file under that
// root (the repository's .gitignore keeps secrets, indexes, and backups out),
// which makes the adapter safe for ordinary Markdown data but unsuitable for
// mixed product/data repositories.
package gitstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// ProviderName identifies this adapter in ports.SyncStatus.
	ProviderName = "git"
	// DefaultRemote is used when Options.Remote is empty.
	DefaultRemote = "origin"
	// DefaultTimeout bounds one git invocation when Options.Timeout is zero.
	DefaultTimeout = 60 * time.Second

	// waitDelay releases pipes when a killed git process leaves children
	// behind (for example git-remote-https after a timeout).
	waitDelay = 5 * time.Second
)

// Options tunes a git-backed sync provider. Zero values select defaults.
type Options struct {
	// Remote is the git remote used for fetch and push. Defaults to origin.
	Remote string
	// GitPath is the git executable. Defaults to "git" from PATH.
	GitPath string
	// Timeout bounds each git invocation. Zero uses DefaultTimeout.
	Timeout time.Duration
}

// Provider synchronizes a brain data repository through the git CLI.
type Provider struct {
	dir     string
	remote  string
	gitPath string
	timeout time.Duration
}

var _ ports.SyncProvider = (*Provider)(nil)

// Open validates dir and builds a provider for it. It does not require the
// directory to be a git repository yet; Status and the sync calls report that
// condition with a sanitized command error.
func Open(dir string, opts Options) (*Provider, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, domain.NewValidationError("gitstore.dir", "is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("gitstore: resolve directory: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("gitstore: open %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, domain.NewValidationError("gitstore.dir", "is not a directory")
	}

	remote := strings.TrimSpace(opts.Remote)
	if remote == "" {
		remote = DefaultRemote
	}
	if strings.HasPrefix(remote, "-") {
		return nil, domain.NewValidationError("gitstore.remote", "must not start with '-'")
	}
	gitPath := strings.TrimSpace(opts.GitPath)
	if gitPath == "" {
		gitPath = "git"
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout < 0 {
		return nil, domain.NewValidationError("gitstore.timeout", "must not be negative")
	}

	return &Provider{
		dir:     abs,
		remote:  remote,
		gitPath: gitPath,
		timeout: timeout,
	}, nil
}

// run executes one git command in the repository root and returns raw stdout.
// A non-zero exit becomes a *CommandError with sanitized stderr. The argument
// vector is passed to the executable as-is; no shell interpolation happens.
func (p *Provider) run(ctx context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("gitstore: empty git command")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, p.gitPath, args...)
	cmd.Dir = p.dir
	cmd.WaitDelay = waitDelay
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("gitstore: git %s: %w", args[0], ctxErr)
		}
		return "", newCommandError(args[0], stderr.String(), err)
	}
	return stdout.String(), nil
}

// branch returns the current branch, or an empty string for a detached HEAD.
func (p *Provider) branch(ctx context.Context) (string, error) {
	out, err := p.run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		var cmdErr *CommandError
		if errors.As(err, &cmdErr) && cmdErr.ExitCode == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// isDirty reports whether the working tree has staged, unstaged, or untracked
// changes. Ignored files do not count.
func (p *Provider) isDirty(ctx context.Context) (bool, error) {
	out, err := p.run(ctx, "status", "--porcelain=v1", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// conflictedPaths lists unmerged paths left by a merge or rebase.
func (p *Provider) conflictedPaths(ctx context.Context) ([]string, error) {
	out, err := p.run(ctx, "diff", "--name-only", "--diff-filter=U", "-z", "--")
	if err != nil {
		return nil, err
	}
	return splitNul(out), nil
}

// divergence counts commits only in HEAD (ahead) and only in the upstream
// branch (behind). A branch without an upstream falls back to the configured
// remote-tracking branch and finally to zero/zero.
func (p *Provider) divergence(ctx context.Context, branch string) (ahead, behind int, err error) {
	upstream := "@{upstream}"
	if _, statErr := p.run(ctx, "rev-parse", "--verify", "--quiet", upstream); statErr != nil {
		if branch == "" {
			return 0, 0, nil
		}
		remoteRef := "refs/remotes/" + p.remote + "/" + branch
		if _, refErr := p.run(ctx, "rev-parse", "--verify", "--quiet", remoteRef); refErr != nil {
			return 0, 0, nil
		}
		upstream = remoteRef
	}
	if _, headErr := p.run(ctx, "rev-parse", "--verify", "--quiet", "HEAD"); headErr != nil {
		return 0, 0, nil
	}

	out, err := p.run(ctx, "rev-list", "--left-right", "--count", "HEAD..."+upstream)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("gitstore: unexpected rev-list output %q", strings.TrimSpace(out))
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("gitstore: parse ahead count: %w", err)
	}
	behind, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("gitstore: parse behind count: %w", err)
	}
	return ahead, behind, nil
}

// revision reads the current HEAD commit identity.
func (p *Provider) revision(ctx context.Context) (domain.SyncRevision, error) {
	out, err := p.run(ctx, "log", "-1", "--format=%H%x00%ct")
	if err != nil {
		return domain.SyncRevision{}, err
	}
	parts := strings.SplitN(strings.TrimSpace(out), "\x00", 2)
	if len(parts) != 2 || parts[0] == "" {
		return domain.SyncRevision{}, fmt.Errorf("gitstore: unexpected log output %q", strings.TrimSpace(out))
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return domain.SyncRevision{}, fmt.Errorf("gitstore: parse commit time: %w", err)
	}
	return domain.SyncRevision{
		Kind:  domain.SyncKindGit,
		Value: parts[0],
		At:    time.Unix(seconds, 0).UTC(),
	}, nil
}

func splitNul(out string) []string {
	parts := strings.Split(out, "\x00")
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			paths = append(paths, part)
		}
	}
	return paths
}
