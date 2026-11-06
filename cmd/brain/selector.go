package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
)

// parseInterspersed parses fs from args while allowing flags after
// positional arguments (`brain open . --config x`, `brain note text --project
// p`), which the standard flag package stops at. It returns the positional
// arguments in order.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	valueFlags := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
			return
		}
		valueFlags[f.Name] = true
	})
	flags, positional := splitValueFlags(args, valueFlags)
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	return append(positional, fs.Args()...), nil
}

// resolveTarget picks the project for a command: --project (id or alias)
// wins, then a positional selector (id, alias or path), then the working
// directory. The resolved checkout path is used for git context.
func resolveTarget(ctx context.Context, a *app.App, projectFlag, selector string) (app.Resolved, error) {
	switch {
	case strings.TrimSpace(projectFlag) != "":
		return a.ResolveSelector(ctx, projectFlag)
	case strings.TrimSpace(selector) != "":
		return a.ResolveSelector(ctx, selector)
	default:
		resolved, err := a.ResolveSelector(ctx, "")
		if err == nil {
			return resolved, nil
		}
		// Keep the single-project convenience of earlier releases.
		projects, listErr := a.ListProjects(ctx)
		if listErr == nil && len(projects) == 1 {
			return app.Resolved{Project: projects[0]}, nil
		}
		return app.Resolved{}, err
	}
}

// splitTitle derives a title from free text the way the legacy tool did: the
// first line, at most 80 runes.
func splitTitle(text string) string {
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	if runes := []rune(first); len(runes) > 80 {
		first = strings.TrimSpace(string(runes[:80]))
	}
	return first
}

// gitDir is where git context is read: the resolved project checkout, or ""
// (reported as "not a repository") when the project has no existing path.
// The working directory is deliberately not used: it may belong to another
// repository than the project being opened.
func gitDir(resolved app.Resolved) string {
	if resolved.Path != "" {
		if info, err := os.Stat(resolved.Path); err == nil && info.IsDir() {
			return resolved.Path
		}
	}
	return ""
}

// printGitAndGraphs appends the ## Git and ## Graphs sections of `brain open`.
func printGitAndGraphs(ctx context.Context, w io.Writer, resolved app.Resolved) {
	summary := gitinfo.Read(ctx, gitDir(resolved))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Git")
	if !summary.IsRepo {
		fmt.Fprintln(w, "(not a repository)")
	} else {
		fmt.Fprintf(w, "branch: %s\nhead: %s\ndirty: %d\nremote: %s\n", summary.Branch, summary.Head, summary.Dirty, summary.Remote)
		for _, line := range summary.Log {
			fmt.Fprintln(w, line)
		}
	}
	roots := resolved.Project.Paths
	if len(roots) == 0 && resolved.Path != "" {
		roots = []string{resolved.Path}
	}
	graphs := gitinfo.FindGraphs(roots)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Graphs")
	if len(graphs) == 0 {
		fmt.Fprintln(w, "(none)")
		return
	}
	for _, graph := range graphs {
		fmt.Fprintf(w, "- %s: %s\n", graph.Kind, graph.Path)
	}
}
