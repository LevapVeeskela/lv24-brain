package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
)

// runPrompt dispatches the opt-in, machine-local prompt zone commands.
func runPrompt(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printPromptUsage(stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		printPromptUsage(stdout)
		return 0
	case "save":
		return runPromptSave(args[1:], stdout, stderr)
	case "list":
		return runPromptList(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain prompt: unknown subcommand %q\n\n", args[0])
		printPromptUsage(stderr)
		return 2
	}
}

func printPromptUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  brain prompt save --project <id> --text <text> [--session <id>]
  brain prompt list [--project <id>] [--limit <n>] [--json]

Prompts are opt-in (prompts.enabled: true), stored under
projects/<id>/prompts/, and never committed or indexed.
`)
}

func runPromptSave(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("prompt save", stderr)
	project := fs.String("project", "", "project id or alias (default: the working directory's project)")
	text := fs.String("text", "", "prompt text")
	session := fs.String("session", "", "optional session id the prompt belongs to")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if value := strings.TrimSpace(strings.Join(positional, " ")); value != "" {
		if strings.TrimSpace(*text) != "" {
			fmt.Fprintln(stderr, "brain prompt save: pass the text either positionally or with --text, not both")
			return 2
		}
		*text = value
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "prompt save", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "prompt save", err)
	}
	defer func() { _ = runtime.Close() }()
	prompt, err := runtime.App.SavePrompt(context.Background(), app.PromptInput{
		ProjectID: *project,
		Text:      *text,
		Session:   *session,
	})
	if err != nil {
		return reportError(stderr, "prompt save", err)
	}
	fmt.Fprintf(stdout, "prompt %s %s\n", prompt.ProjectID, prompt.At.UTC().Format("2006-01-02T15:04:05Z"))
	return 0
}

func runPromptList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("prompt list", stderr)
	project := fs.String("project", "", "project id or alias; empty lists every registered project")
	limit := fs.Int("limit", 0, "maximum prompts; 0 selects the default of 20")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain prompt list: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "brain prompt list: --limit must not be negative")
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "prompt list", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "prompt list", err)
	}
	defer func() { _ = runtime.Close() }()
	prompts, err := runtime.App.ListPrompts(context.Background(), app.PromptListOptions{
		ProjectID: *project,
		Limit:     *limit,
	})
	if err != nil {
		return reportError(stderr, "prompt list", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "prompt list", prompts)
	}
	for _, prompt := range prompts {
		session := ""
		if prompt.Session != "" {
			session = " session=" + prompt.Session
		}
		fmt.Fprintf(stdout, "%s %s%s\n  %s\n", prompt.ProjectID,
			prompt.At.UTC().Format("2006-01-02T15:04:05Z"), session, prompt.Text)
	}
	return 0
}

// unchangedSuffix marks an idempotent topic upsert in a write command output.
func unchangedSuffix(already bool) string {
	if already {
		return " (unchanged)"
	}
	return ""
}
