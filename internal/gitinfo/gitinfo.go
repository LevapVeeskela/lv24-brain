// Package gitinfo reads read-only repository context (branch, head, dirty
// count, remote, recent log) for open and handoff output, and locates code
// graph indexes (graphify-out/, .codegraph/) next to a project checkout.
package gitinfo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Timeout bounds every git invocation so a hung repository never blocks the
// CLI.
const Timeout = 5 * time.Second

// Summary is the repository context printed by open and recorded by handoff.
type Summary struct {
	IsRepo  bool
	Branch  string
	Head    string
	Dirty   int
	Remote  string
	Subject string
	// Log is up to five "- <short sha> <subject>" lines, newest first.
	Log []string
}

// Read collects a Summary for dir. A directory that is not inside a git
// repository (or a missing git binary) yields IsRepo=false and no error.
func Read(ctx context.Context, dir string) Summary {
	if strings.TrimSpace(dir) == "" {
		return Summary{}
	}
	run := func(args ...string) (string, bool) {
		runCtx, cancel := context.WithTimeout(ctx, Timeout)
		defer cancel()
		out, err := exec.CommandContext(runCtx, "git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return "", false
		}
		return strings.TrimRight(string(out), "\r\n"), true
	}
	if inside, ok := run("rev-parse", "--is-inside-work-tree"); !ok || strings.TrimSpace(inside) != "true" {
		return Summary{}
	}
	summary := Summary{IsRepo: true}
	summary.Branch, _ = run("rev-parse", "--abbrev-ref", "HEAD")
	summary.Head, _ = run("rev-parse", "--short", "HEAD")
	if status, ok := run("status", "--porcelain"); ok && strings.TrimSpace(status) != "" {
		summary.Dirty = len(strings.Split(strings.TrimSpace(status), "\n"))
	}
	remote, _ := run("remote", "get-url", "origin")
	summary.Remote = RedactRemote(strings.TrimSpace(remote))
	summary.Subject, _ = run("log", "-1", "--format=%s")
	if log, ok := run("log", "-5", "--format=- %h %s"); ok && strings.TrimSpace(log) != "" {
		summary.Log = strings.Split(strings.TrimSpace(log), "\n")
	}
	return summary
}

// Context renders the one-line-plus-log form stored in a handoff, matching
// the legacy tool: "branch=.. head=.. dirty=N remote=..\n<log>".
func (s Summary) Context() string {
	if !s.IsRepo {
		return ""
	}
	line := fmt.Sprintf("branch=%s head=%s dirty=%d remote=%s", s.Branch, s.Head, s.Dirty, s.Remote)
	if len(s.Log) == 0 {
		return line
	}
	return line + "\n" + strings.Join(s.Log, "\n")
}

// RedactRemote drops credentials embedded in a remote URL
// (https://user:token@host/...) so they never reach output or brain data.
func RedactRemote(remote string) string {
	scheme := strings.Index(remote, "://")
	if scheme < 0 {
		return remote
	}
	rest := remote[scheme+3:]
	hostEnd := strings.IndexByte(rest, '/')
	if hostEnd < 0 {
		hostEnd = len(rest)
	}
	if at := strings.LastIndexByte(rest[:hostEnd], '@'); at >= 0 {
		return remote[:scheme+3] + rest[at+1:]
	}
	return remote
}

// Graph is one code-graph index found for a project.
type Graph struct {
	// Kind is "graphify" or "codegraph".
	Kind string
	// Path is the absolute index directory.
	Path string
}

// FindGraphs looks for graphify-out/ and .codegraph/ in each root and in its
// immediate subdirectories (monorepos and worktree layouts keep them one level
// down). The scan is shallow and skips hidden and dependency directories.
func FindGraphs(roots []string) []Graph {
	seen := map[string]bool{}
	var out []Graph
	add := func(dir string) {
		for kind, name := range map[string]string{"graphify": "graphify-out", "codegraph": ".codegraph"} {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && info.IsDir() && !seen[candidate] {
				seen[candidate] = true
				out = append(out, Graph{Kind: kind, Path: candidate})
			}
		}
	}
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		add(root)
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() || strings.HasPrefix(name, ".") || skipDir[name] {
				continue
			}
			add(filepath.Join(root, name))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

var skipDir = map[string]bool{
	"node_modules": true, "dist": true, "build": true, "vendor": true, "venv": true,
	"__pycache__": true, "coverage": true, "graphify-out": true, "target": true,
}
