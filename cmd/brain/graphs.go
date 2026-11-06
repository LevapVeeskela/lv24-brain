package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
)

const (
	graphsDefaultTimeout = 10 * time.Minute
	// graphsLogLimit caps the tool output kept per command in the log file.
	graphsLogLimit = 64 << 10
)

// graphsRun executes one external tool in dir and returns its combined
// stdout and stderr. Tests replace it to record calls without running tools.
var graphsRun = execGraphsCommand

// graphsLookPath reports whether an external tool is installed.
var graphsLookPath = exec.LookPath

// graphsLogPath returns the log file for full tool output. It lives in the
// user cache directory, never inside the data repository.
var graphsLogPath = func() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "lv24-brain", "graphs.log"), nil
}

// execGraphsCommand runs name with args directly (no shell) inside dir.
func execGraphsCommand(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// graphsRunner carries the state of one `brain graphs` invocation.
type graphsRunner struct {
	stdout, stderr io.Writer
	timeout        time.Duration
	dryRun         bool
	tools          map[string]bool
	logFailed      bool
	failed         bool
}

// runGraphs refreshes the code-graph indexes (graphify-out/, .codegraph/)
// of registered projects. It is the Go port of the legacy PowerShell graphs
// command.
func runGraphs(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("graphs", stderr)
	changedOnly := fs.Bool("changed-only", false, "skip projects whose graph.json is newer than a clean HEAD commit")
	initMissing := fs.Bool("init", false, "create missing indexes with graphify extract / codegraph init")
	dryRun := fs.Bool("dry-run", false, "print the commands that would run and run nothing")
	timeout := fs.Duration("timeout", graphsDefaultTimeout, "timeout for each external command")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 1 {
		fmt.Fprintf(stderr, "brain graphs: unexpected arguments: %s\n", strings.Join(positional[1:], " "))
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "brain graphs: --timeout must be positive")
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "graphs", err)
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "graphs", err)
	}
	defer func() { _ = useCase.Close() }()
	projects, err := useCase.App.ListProjects(context.Background())
	if err != nil {
		return reportError(stderr, "graphs", err)
	}
	if len(positional) == 1 {
		want := strings.TrimSpace(positional[0])
		var selected []domain.Project
		for _, project := range projects {
			if project.ID == want {
				selected = append(selected, project)
			}
		}
		if len(selected) == 0 {
			fmt.Fprintf(stderr, "brain graphs: project %q not found\n", want)
			return 1
		}
		projects = selected
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })

	r := &graphsRunner{
		stdout:  stdout,
		stderr:  stderr,
		timeout: *timeout,
		dryRun:  *dryRun,
		tools:   map[string]bool{},
	}
	for _, project := range projects {
		r.refresh(project, *changedOnly, *initMissing)
	}
	if r.failed {
		return 1
	}
	return 0
}

// refresh updates the graph indexes of one project.
func (r *graphsRunner) refresh(project domain.Project, changedOnly, initMissing bool) {
	var paths []string
	for _, path := range app.LocalPaths(project.Paths) {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		fmt.Fprintf(r.stdout, "[skip] %s - no existing path\n", project.ID)
		return
	}
	first := paths[0]
	graphs := gitinfo.FindGraphs(paths)
	if changedOnly && graphsUnchanged(first, graphs) {
		fmt.Fprintf(r.stdout, "[skip] %s - unchanged\n", project.ID)
		return
	}
	fmt.Fprintf(r.stdout, "[%s] refreshing...\n", project.ID)

	hasGraphify, hasCodegraph := false, false
	for _, graph := range graphs {
		root := filepath.Dir(graph.Path)
		switch graph.Kind {
		case "graphify":
			hasGraphify = true
			if _, err := os.Stat(filepath.Join(graph.Path, "graph.json")); err != nil {
				fmt.Fprintf(r.stdout, "  graphify update %s: no graph.json; skipped\n", root)
				continue
			}
			r.command(project.ID, root, "graphify", "update", root)
		case "codegraph":
			hasCodegraph = true
			r.command(project.ID, root, "codegraph", "sync", root)
		}
	}
	if initMissing {
		if !hasGraphify {
			r.command(project.ID, first, "graphify", "extract", first, "--code-only")
		}
		if !hasCodegraph {
			r.command(project.ID, first, "codegraph", "init", first)
		}
	} else if len(graphs) == 0 {
		fmt.Fprintln(r.stdout, "  no graph index found (use --init to create one)")
	}
}

// graphsUnchanged reports whether dir is a clean git repository whose newest
// graph.json is not older than the HEAD commit. Without a graph.json, or when
// git is unavailable, the project is never considered unchanged.
func graphsUnchanged(dir string, graphs []gitinfo.Graph) bool {
	summary := gitinfo.Read(context.Background(), dir)
	if !summary.IsRepo || summary.Dirty != 0 {
		return false
	}
	var newest time.Time
	for _, graph := range graphs {
		if graph.Kind != "graphify" {
			continue
		}
		info, err := os.Stat(filepath.Join(graph.Path, "graph.json"))
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitinfo.Timeout)
	out, err := graphsRun(ctx, dir, "git", "log", "-1", "--format=%ct")
	cancel()
	if err != nil {
		return false
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return false
	}
	head := time.Unix(seconds, 0)
	return !newest.Before(head)
}

// available reports whether tool is installed, printing the notice once per
// tool for the whole run.
func (r *graphsRunner) available(tool string) bool {
	if ok, seen := r.tools[tool]; seen {
		return ok
	}
	_, err := graphsLookPath(tool)
	ok := err == nil
	r.tools[tool] = ok
	if !ok {
		fmt.Fprintf(r.stdout, "  %s not installed; skipped\n", tool)
	}
	return ok
}

// command runs (or, with --dry-run, announces) one external tool invocation.
func (r *graphsRunner) command(projectID, dir, name string, args ...string) {
	label := strings.Join(append([]string{name}, args...), " ")
	if !r.available(name) {
		return
	}
	if r.dryRun {
		fmt.Fprintf(r.stdout, "  would run: %s (in %s)\n", label, dir)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	out, err := graphsRun(ctx, dir, name, args...)
	timedOut := ctx.Err() == context.DeadlineExceeded
	cancel()
	status, ok := graphsStatus(err, timedOut)
	if !ok {
		r.failed = true
	}
	fmt.Fprintf(r.stdout, "  %s: %s\n", label, status)
	r.appendLog(projectID, dir, label, status, out)
}

// graphsStatus turns a command result into the printed status.
func graphsStatus(err error, timedOut bool) (string, bool) {
	if timedOut {
		return "timeout", false
	}
	if err == nil {
		return "ok", true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("exit %d", exitErr.ExitCode()), false
	}
	return "error: " + err.Error(), false
}

// appendLog records the command and its output. Logging problems are reported
// once on stderr and never change the command result.
func (r *graphsRunner) appendLog(projectID, dir, label, status string, out []byte) {
	path, err := graphsLogPath()
	if err == nil {
		err = writeGraphsLog(path, projectID, dir, label, status, out)
	}
	if err != nil && !r.logFailed {
		r.logFailed = true
		fmt.Fprintf(r.stderr, "brain graphs: cannot write log: %v\n", err)
	}
}

func writeGraphsLog(path, projectID, dir, label, status string, out []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s [%s] %s (in %s): %s\n", time.Now().UTC().Format(time.RFC3339), projectID, label, dir, status)
	body := out
	truncated := len(body) > graphsLogLimit
	if truncated {
		body = body[:graphsLogLimit]
	}
	b.Write(body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "[output truncated at %d bytes]\n", graphsLogLimit)
	}
	b.WriteByte('\n')
	_, writeErr := file.WriteString(b.String())
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
