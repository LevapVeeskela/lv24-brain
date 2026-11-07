package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// runState implements `brain state get|set`: agents keep a project's goal,
// next action, blockers and open questions current without editing Markdown.
func runState(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		fmt.Fprint(w, `Usage:
  brain state get [project] [--json]
  brain state set [project] [--goal <text>] [--next <text>]
                  [--active <item>]... [--blocked <item>]... [--question <item>]...
                  [--clear-active] [--clear-blocked] [--clear-questions]
                  [--fresh-for <duration, e.g. 72h>] [--json]

[project] is an id, alias or path (default: the working directory's project).
Repeating --active, --blocked or --question replaces that list; the --clear
flags empty it. Omitted fields keep their stored values.
`)
	}
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "get":
		return runStateGet(args[1:], stdout, stderr)
	case "set":
		return runStateSet(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "brain state: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func runStateGet(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("state get", stderr)
	project := fs.String("project", "", "project id or alias")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, positional, stderr, "state get")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	_, state, err := runtime.App.GetState(ctx, target.Project.ID)
	if err != nil {
		return reportError(stderr, "state get", err)
	}
	return printState(stdout, stderr, state, *asJSON)
}

func runStateSet(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("state set", stderr)
	project := fs.String("project", "", "project id or alias")
	goal := fs.String("goal", "", "project goal")
	next := fs.String("next", "", "next action")
	freshFor := fs.Duration("fresh-for", 0, "mark the state current for this long (e.g. 72h); 0 keeps the stored value")
	_ = fs.Bool("clear-active", false, "clear the active list")
	_ = fs.Bool("clear-blocked", false, "clear the blocked list")
	_ = fs.Bool("clear-questions", false, "clear the open questions")
	asJSON := fs.Bool("json", false, "print the resulting state as JSON")
	var active, blocked, questions []string
	repeat := func(target *[]string) func(string) error {
		return func(value string) error {
			if value = strings.TrimSpace(value); value == "" {
				return errors.New("must not be empty")
			}
			*target = append(*target, value)
			return nil
		}
	}
	fs.Func("active", "active work item (repeatable; replaces the list)", repeat(&active))
	fs.Func("blocked", "blocker (repeatable; replaces the list)", repeat(&blocked))
	fs.Func("question", "open question (repeatable; replaces the list)", repeat(&questions))
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	for _, pair := range [][2]string{{"active", "clear-active"}, {"blocked", "clear-blocked"}, {"question", "clear-questions"}} {
		if seen[pair[0]] && seen[pair[1]] {
			fmt.Fprintf(stderr, "brain state set: --%s and --%s cannot be combined\n", pair[0], pair[1])
			return 2
		}
	}
	if *freshFor < 0 {
		fmt.Fprintln(stderr, "brain state set: --fresh-for must not be negative")
		return 2
	}

	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, positional, stderr, "state set")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()

	input := app.StateInput{ProjectID: target.Project.ID}
	if seen["goal"] {
		input.Goal = goal
	}
	if seen["next"] {
		input.NextAction = next
	}
	list := func(flagName, clearName string, values []string) *[]string {
		switch {
		case seen[flagName]:
			return &values
		case seen[clearName]:
			empty := []string{}
			return &empty
		default:
			return nil
		}
	}
	input.Active = list("active", "clear-active", active)
	input.Blocked = list("blocked", "clear-blocked", blocked)
	input.OpenQuestions = list("question", "clear-questions", questions)
	if *freshFor > 0 {
		until := time.Now().Add(*freshFor)
		input.FreshUntil = &until
	}
	state, err := runtime.App.SetState(ctx, input)
	if err != nil {
		return reportError(stderr, "state set", err)
	}
	return printState(stdout, stderr, state, *asJSON)
}

// stateTarget loads the brain and resolves the project like other commands:
// --project (id/alias), else a positional id/alias/path, else the working
// directory.
func stateTarget(ctx context.Context, cfgPath, project string, positional []string, stderr io.Writer, name string) (*bootstrap.Runtime, app.Resolved, int) {
	if len(positional) > 1 {
		fmt.Fprintf(stderr, "brain %s: unexpected arguments: %s\n", name, strings.Join(positional[1:], " "))
		return nil, app.Resolved{}, 2
	}
	selector := ""
	if len(positional) == 1 {
		selector = positional[0]
	}
	cfg, err := loadRequiredConfig(cfgPath)
	if err != nil {
		return nil, app.Resolved{}, reportError(stderr, name, err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return nil, app.Resolved{}, reportError(stderr, name, err)
	}
	target, err := resolveTarget(ctx, runtime.App, project, selector)
	if err != nil {
		_ = runtime.Close()
		return nil, app.Resolved{}, reportError(stderr, name, err)
	}
	return runtime, target, 0
}

func printState(stdout, stderr io.Writer, state domain.State, asJSON bool) int {
	if asJSON {
		return writeJSONReport(stdout, stderr, "state", state)
	}
	fmt.Fprintf(stdout, "project: %s\n", state.ProjectID)
	fmt.Fprintf(stdout, "goal: %s\n", valueOr(state.Goal, "not set"))
	fmt.Fprintf(stdout, "next_action: %s\n", valueOr(state.NextAction, "not set"))
	printList(stdout, "active", state.Active)
	printList(stdout, "blocked", state.Blocked)
	printList(stdout, "open_questions", state.OpenQuestions)
	if !state.FreshUntil.IsZero() {
		fmt.Fprintf(stdout, "fresh_until: %s\n", state.FreshUntil.UTC().Format(time.RFC3339))
	}
	if !state.UpdatedAt.IsZero() {
		fmt.Fprintf(stdout, "updated: %s\n", state.UpdatedAt.UTC().Format(time.RFC3339))
	}
	return 0
}
