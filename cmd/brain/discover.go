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
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "discover", err)
	}
	candidates, err := discoverCandidates(absRoot, skip)
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
	byAlias := map[string]string{} // alias → project id
	knownPaths := map[string]bool{}
	repoIndex := map[string][]string{}
	for _, project := range projects {
		byID[project.ID] = true
		for _, alias := range project.Aliases {
			key := strings.ToLower(strings.TrimSpace(alias))
			if key != "" {
				if _, taken := byAlias[key]; !taken {
					byAlias[key] = project.ID
				}
			}
		}
		for _, path := range project.Paths {
			knownPaths[pathKey(path)] = true
		}
		if key := gitinfo.NormalizeRepoURL(project.RepoURL); key != "" {
			repoIndex[key] = append(repoIndex[key], project.ID)
		}
	}

	claimed := map[string]bool{} // ids already assigned to a new candidate in this run
	newCount, adoptCount := 0, 0
	for _, c := range candidates {
		// A retired checkout is never registered, even when it was recreated
		// with its own .git; the project that replaced it owns the work.
		if entry, retired := app.RetiredPathMatch(cfg, c.path); retired {
			fmt.Fprintf(stdout, "retired %s %s (replaced by %s)\n", c.id, c.path, entry.ReplacedBy)
			continue
		}
		if knownPaths[pathKey(c.path)] {
			fmt.Fprintf(stdout, "known %s %s\n", c.id, c.path)
			continue
		}
		// Identity: the normalized repository URL first, then the id or
		// alias. A checkout of a registered repository at a new path (or
		// under a new folder name) is adopted into that project instead of
		// registering a second id for the same repository.
		owner := ""
		if byID[c.id] {
			owner = c.id
		} else if id, ok := byAlias[strings.ToLower(c.id)]; ok {
			owner = id
		}
		repoURL := discoverRemoteURL(ctx, c.path)
		repoOwners := repoIndex[gitinfo.NormalizeRepoURL(repoURL)]
		if len(repoOwners) > 1 {
			fmt.Fprintf(stdout, "repo-conflict %s %s (repository claimed by %s)\n",
				c.id, c.path, strings.Join(repoOwners, ", "))
			continue
		}
		adopt := ""
		if len(repoOwners) == 1 {
			adopt = repoOwners[0]
		}
		switch {
		case adopt != "":
			if *apply {
				_, branch := discoverRepoInfo(ctx, c.path)
				if err := discoverAdopt(ctx, rt.App, adopt, c, repoURL, branch); err != nil {
					return reportError(stderr, "discover", err)
				}
				fmt.Fprintf(stdout, "adopted %s %s\n", adopt, c.path)
			} else {
				adoptCount++
				fmt.Fprintf(stdout, "adopt %s %s\n", adopt, c.path)
			}
		case owner != "" || claimed[c.id]:
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
		if adoptCount > 0 {
			fmt.Fprintf(stdout, "dry run: %d new project(s), %d adoption(s); rerun with --apply to register\n", newCount, adoptCount)
		} else {
			fmt.Fprintf(stdout, "dry run: %d new project(s); rerun with --apply to register\n", newCount)
		}
	}
	return 0
}

// discoverAdopt attaches a checkout path to an existing project instead of
// registering a second id for the same repository. Only the path list (and a
// missing repo URL or branch) is written; provenance of the new path is
// stamped by SaveProject.
func discoverAdopt(ctx context.Context, a *app.App, id string, c discoverCandidate, repoURL, branch string) error {
	existing, err := a.GetProject(ctx, id)
	if err != nil {
		return err
	}
	paths := append([]string{}, existing.Paths...)
	paths = append(paths, c.path)
	input := app.ProjectInput{ID: id, Paths: &paths}
	if strings.TrimSpace(existing.RepoURL) == "" && strings.TrimSpace(repoURL) != "" {
		input.RepoURL = &repoURL
	}
	if strings.TrimSpace(existing.DefaultBranch) == "" && strings.TrimSpace(branch) != "" {
		input.DefaultBranch = &branch
	}
	_, err = a.SaveProject(ctx, input)
	return err
}

// discoverRemoteURL reads the redacted origin URL of a repository; a
// repository without a readable origin yields "".
func discoverRemoteURL(ctx context.Context, path string) string {
	out, err := discoverGit(ctx, path, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return gitinfo.RedactRemote(out)
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
