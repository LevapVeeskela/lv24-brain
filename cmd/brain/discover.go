package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
)

// discoverDefaultExcludes are child names never scanned, in addition to the
// --exclude list. Matching is case-insensitive.
var discoverDefaultExcludes = []string{"node_modules"}

// discoverCandidate is a workspace child that looks like a git repository.
type discoverCandidate struct {
	id   string
	path string
}

// runDiscover scans the immediate children of --root for git repositories
// and reports or registers them. Without --apply nothing is written.
func runDiscover(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("discover", stderr)
	root := fs.String("root", "", "workspace directory whose immediate children are scanned (required)")
	apply := fs.Bool("apply", false, "register new repositories (default: dry run)")
	exclude := fs.String("exclude", "", "comma-separated child names to skip")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain discover: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if strings.TrimSpace(*root) == "" {
		fmt.Fprintln(stderr, "brain discover: --root is required")
		return 2
	}
	absRoot, err := filepath.Abs(strings.TrimSpace(*root))
	if err != nil {
		return reportError(stderr, "discover", err)
	}
	if info, err := os.Stat(absRoot); err != nil || !info.IsDir() {
		fmt.Fprintf(stderr, "brain discover: --root %s is not a directory\n", absRoot)
		return 2
	}
	skip := discoverSkipSet(*exclude)
	candidates, err := discoverCandidates(absRoot, skip)
	if err != nil {
		return reportError(stderr, "discover", err)
	}

	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "discover", err)
	}
	rt, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "discover", err)
	}
	defer func() { _ = rt.Close() }()
	ctx := context.Background()
	projects, err := rt.App.ListProjects(ctx)
	if err != nil {
		return reportError(stderr, "discover", err)
	}

	caseInsensitive := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	pathKey := func(p string) string {
		p = filepath.Clean(p)
		if caseInsensitive {
			return strings.ToLower(p)
		}
		return p
	}
	byID := map[string]bool{}
	knownPaths := map[string]bool{}
	for _, project := range projects {
		byID[project.ID] = true
		for _, path := range project.Paths {
			knownPaths[pathKey(path)] = true
		}
	}

	claimed := map[string]bool{} // ids already assigned to a new candidate in this run
	newCount := 0
	for _, c := range candidates {
		switch {
		case knownPaths[pathKey(c.path)]:
			fmt.Fprintf(stdout, "known %s %s\n", c.id, c.path)
		case byID[c.id] || claimed[c.id]:
			fmt.Fprintf(stdout, "id-conflict %s %s (id used by another path)\n", c.id, c.path)
		case *apply:
			claimed[c.id] = true
			if err := discoverRegister(ctx, rt.App, c); err != nil {
				return reportError(stderr, "discover", err)
			}
			fmt.Fprintf(stdout, "registered %s\n", c.id)
		default:
			claimed[c.id] = true
			newCount++
			fmt.Fprintf(stdout, "new %s %s\n", c.id, c.path)
		}
	}
	if !*apply {
		fmt.Fprintf(stdout, "dry run: %d new project(s); rerun with --apply to register\n", newCount)
	}
	return 0
}

// discoverRegister upserts one new repository. Only called for candidates
// that are absent from the registry, so existing projects are never changed.
func discoverRegister(ctx context.Context, a *app.App, c discoverCandidate) error {
	repoURL, branch := discoverRepoInfo(ctx, c.path)
	name := c.id
	paths := []string{c.path}
	kind := domain.ProjectKindRepository
	_, err := a.SaveProject(ctx, app.ProjectInput{
		ID:            c.id,
		Name:          &name,
		Paths:         &paths,
		Kind:          &kind,
		RepoURL:       &repoURL,
		DefaultBranch: &branch,
	})
	return err
}

// discoverRepoInfo reads the redacted origin URL and the default branch of a
// repository. Missing values are returned empty.
func discoverRepoInfo(ctx context.Context, path string) (string, string) {
	var repoURL, branch string
	if out, err := discoverGit(ctx, path, "remote", "get-url", "origin"); err == nil {
		repoURL = gitinfo.RedactRemote(out)
	}
	if out, err := discoverGit(ctx, path, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		branch = strings.TrimPrefix(out, "origin/")
	}
	if branch == "" {
		branch = gitinfo.Read(ctx, path).Branch
		if branch == "HEAD" {
			branch = ""
		}
	}
	return repoURL, branch
}

func discoverGit(ctx context.Context, dir string, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, gitinfo.Timeout)
	defer cancel()
	out, err := exec.CommandContext(runCtx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// discoverCandidates lists repositories among the immediate children of root.
// A child is a repository when it contains .git (directory or worktree file);
// otherwise child/main/.git makes child/main the repository. Plain folders
// are skipped.
func discoverCandidates(root string, skip map[string]bool) ([]discoverCandidate, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []discoverCandidate
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || strings.HasPrefix(name, ".") || skip[strings.ToLower(name)] {
			continue
		}
		id := discoverSlug(name)
		if id == "" {
			continue
		}
		child := filepath.Join(root, name)
		switch {
		case discoverHasGit(child):
			out = append(out, discoverCandidate{id: id, path: child})
		case discoverHasGit(filepath.Join(child, "main")):
			out = append(out, discoverCandidate{id: id, path: filepath.Join(child, "main")})
		}
	}
	return out, nil
}

func discoverHasGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func discoverSkipSet(exclude string) map[string]bool {
	skip := map[string]bool{}
	for _, name := range discoverDefaultExcludes {
		skip[name] = true
	}
	for _, name := range strings.Split(exclude, ",") {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			skip[name] = true
		}
	}
	return skip
}

// discoverSlug turns a directory name into a project id: lower-case letters,
// digits and dashes; space, underscore and dot become dashes; other characters
// are dropped; repeated and edge dashes are removed.
func discoverSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '_' || r == '.':
			b.WriteRune('-')
		}
	}
	parts := strings.Split(b.String(), "-")
	kept := parts[:0]
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "-")
}
