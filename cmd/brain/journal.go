package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// runJournal prints the per-user activity journal, newest first.
func runJournal(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("journal", stderr)
	project := fs.String("project", "", "project id filter")
	user := fs.String("user", "", "user filter")
	event := fs.String("event", "", "event filter (note, decision, attempt, handoff, commit, ...)")
	since := fs.Duration("since", 0, "only entries newer than this duration, e.g. 24h")
	limit := fs.Int("limit", 20, "maximum entries")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain journal: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *limit < 0 || *since < 0 {
		fmt.Fprintln(stderr, "brain journal: --limit and --since must not be negative")
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "journal", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "journal", err)
	}
	defer func() { _ = runtime.Close() }()
	query := ports.JournalQuery{Project: *project, User: *user, Event: *event, Limit: *limit}
	if *since > 0 {
		query.Since = time.Now().Add(-*since)
	}
	entries, err := runtime.App.ReadJournal(context.Background(), query)
	if err != nil {
		return reportError(stderr, "journal", err)
	}
	for _, entry := range entries {
		fmt.Fprintln(stdout, formatJournalEntry(entry))
	}
	return 0
}

func formatJournalEntry(entry ports.JournalEntry) string {
	parts := []string{entry.Time.Local().Format("2006-01-02 15:04"), entry.User, entry.Event}
	if entry.Project != "" {
		parts = append(parts, entry.Project)
	}
	switch {
	case entry.Event == app.JournalCommit:
		parts = append(parts, strings.TrimSpace(entry.Fields["head"]+" "+entry.Fields["subject"]))
	case entry.Title != "":
		parts = append(parts, entry.Title)
	case entry.ID != "":
		parts = append(parts, entry.ID)
	}
	return strings.Join(parts, "  ")
}

// runEvent records an external event in the journal. `brain event commit`
// is meant for a git post-commit hook: it reads branch, head, subject and
// remote from the repository and resolves the project from the registry.
func runEvent(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "commit" {
		fmt.Fprintln(stderr, "usage: brain event commit [--project <id>] [--repo <path>]")
		return 2
	}
	fs := newFlagSet("event commit", stderr)
	project := fs.String("project", "", "project id (default: the registered project for --repo)")
	repo := fs.String("repo", ".", "git repository the commit was made in")
	quiet := fs.Bool("quiet", false, "print nothing on success (for hooks)")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain event commit: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	commit, err := readGitCommit(*repo)
	if err != nil {
		return reportError(stderr, "event commit", err)
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "event commit", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "event commit", err)
	}
	defer func() { _ = runtime.Close() }()
	ctx := context.Background()
	commit.ProjectID = strings.TrimSpace(*project)
	if commit.ProjectID == "" {
		projects, err := runtime.App.ListProjects(ctx)
		if err != nil {
			return reportError(stderr, "event commit", err)
		}
		commit.ProjectID = projectForRepo(projects, commit.toplevel)
	}
	entry, err := runtime.App.RecordCommit(ctx, commit.CommitInput)
	if err != nil {
		return reportError(stderr, "event commit", err)
	}
	if !*quiet {
		fmt.Fprintln(stdout, formatJournalEntry(entry))
	}
	return 0
}

type gitCommit struct {
	app.CommitInput
	toplevel string
}

func readGitCommit(repo string) (gitCommit, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.Output()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
				return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)))
			}
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	toplevel, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return gitCommit{}, fmt.Errorf("%w: %s is not a git repository (%v)", domain.ErrValidation, repo, err)
	}
	head, err := git("rev-parse", "--short", "HEAD")
	if err != nil {
		return gitCommit{}, err
	}
	branch, _ := git("rev-parse", "--abbrev-ref", "HEAD")
	subject, _ := git("log", "-1", "--format=%s")
	remote, _ := git("remote", "get-url", "origin")
	return gitCommit{
		CommitInput: app.CommitInput{Branch: branch, Head: head, Subject: subject, Remote: redactRemote(remote)},
		toplevel:    toplevel,
	}, nil
}

// redactRemote drops credentials embedded in a remote URL
// (https://user:token@host/...) before it is written to the journal.
func redactRemote(remote string) string {
	scheme := strings.Index(remote, "://")
	if scheme < 0 {
		return remote
	}
	rest := remote[scheme+3:]
	if at := strings.LastIndex(rest[:strings.IndexAny(rest+"/", "/")], "@"); at >= 0 {
		return remote[:scheme+3] + rest[at+1:]
	}
	return remote
}

// projectForRepo picks the registered project whose paths contain toplevel,
// else the project named like the repository directory, else "".
func projectForRepo(projects []domain.Project, toplevel string) string {
	clean := func(path string) string {
		return strings.ToLower(filepath.Clean(filepath.FromSlash(strings.TrimSpace(path))))
	}
	want := clean(toplevel)
	sorted := append([]domain.Project(nil), projects...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, project := range sorted {
		for _, path := range project.Paths {
			if path != "" && clean(path) == want {
				return project.ID
			}
		}
	}
	base := strings.ToLower(filepath.Base(want))
	for _, project := range sorted {
		if strings.ToLower(project.ID) == base {
			return project.ID
		}
	}
	return ""
}
