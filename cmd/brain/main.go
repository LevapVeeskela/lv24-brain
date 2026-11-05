// Command brain is the LV24 Brain CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/server"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	command, rest := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	case "version", "-v", "--version":
		printVersion(stdout)
		return 0
	case "init":
		return runInit(rest, stderr)
	case "doctor":
		return runDoctor(rest, stdout, stderr)
	case "open":
		return runOpen(rest, stdout, stderr)
	case "bootstrap":
		return runBootstrap(rest, stdout, stderr)
	case "note":
		return runNote(rest, stdout, stderr)
	case "decision":
		return runDecision(rest, stdout, stderr)
	case "attempt":
		return runAttempt(rest, stdout, stderr)
	case "handoff":
		return runHandoff(rest, stdout, stderr)
	case "status":
		return runStatus(rest, stdout, stderr)
	case "review":
		return runReview(rest, stdout, stderr)
	case "sync":
		return runSync(rest, stdout, stderr)
	case "serve":
		return runServe(rest, stdout, stderr)
	case "update":
		return runUpdate(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain: unknown command %q\n\n", command)
		printUsage(stderr)
		return 2
	}
}

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "brain %s (commit %s, built %s, %s %s/%s)\n",
		version.Version, version.Commit, version.Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `LV24 Brain — local, git-backed, or server-backed project memory.

Usage:
  brain <command> [flags]

Commands:
  init       Create a new brain instance
  doctor     Check engine and data schema compatibility
  open       Open a project context
  bootstrap  Build agent context for a project
  note       Append a dated note
  decision   Append a decision record
  attempt    Append an attempt record
  handoff    Append a session handoff
  status     Show the configured instance
  review     Build a review report
  sync       Pull, commit, and push brain data
  serve      Run the HTTP server
  update     Check for or apply engine updates
  version    Print version information
  help       Show this help

Global flags:
  --config <path>   Path to brain.yaml (default: $BRAIN_CONFIG or brain.yaml)

Run "brain <command> --help" for command flags.
`)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func configFlag(fs *flag.FlagSet) *string {
	defaultPath := os.Getenv("BRAIN_CONFIG")
	if defaultPath == "" {
		defaultPath = config.FileName
	}
	return fs.String("config", defaultPath, "path to brain.yaml")
}

func loadConfigOrDefault(path string) (config.BrainConfig, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if config.IsNotExist(err) {
		return config.Default(), nil
	}
	return config.BrainConfig{}, err
}

func reportError(w io.Writer, command string, err error) int {
	if errors.Is(err, app.ErrNotImplemented) {
		fmt.Fprintf(w, "brain %s: %v (planned for a later work package)\n", command, err)
		return 1
	}
	fmt.Fprintf(w, "brain %s: %v\n", command, err)
	return 1
}

func runInit(args []string, stderr io.Writer) int {
	fs := newFlagSet("init", stderr)
	mode := fs.String("mode", config.ModeLocal, "install mode: local, git, or server")
	dataDir := fs.String("data-dir", "", "brain data directory")
	serverURL := fs.String("server-url", "", "brain server URL (server mode)")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := config.Default()
	cfg.Mode = *mode
	cfg.DataDir = *dataDir
	cfg.ServerURL = *serverURL
	useCase := app.New(cfg, app.Dependencies{})
	if _, err := useCase.Init(context.Background(), app.InitOptions{
		ConfigPath: *cfgPath,
		Mode:       *mode,
		DataDir:    *dataDir,
		ServerURL:  *serverURL,
	}); err != nil {
		return reportError(stderr, "init", err)
	}
	return 0
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("doctor", stderr)
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "doctor", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	report, err := useCase.Doctor(context.Background(), app.DoctorOptions{})
	if err != nil {
		return reportError(stderr, "doctor", err)
	}
	for _, check := range report.Checks {
		fmt.Fprintf(stdout, "%s: %s (%s)\n", check.Status, check.Name, check.Message)
	}
	return 0
}

func runOpen(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("open", stderr)
	project := fs.String("project", "", "project id")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "open", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	result, err := useCase.Open(context.Background(), app.OpenOptions{ProjectID: *project})
	if err != nil {
		return reportError(stderr, "open", err)
	}
	fmt.Fprintf(stdout, "%s\n", result.Project.Name)
	return 0
}

func runBootstrap(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bootstrap", stderr)
	project := fs.String("project", "", "project id")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "bootstrap", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	result, err := useCase.Bootstrap(context.Background(), app.BootstrapOptions{ProjectID: *project})
	if err != nil {
		return reportError(stderr, "bootstrap", err)
	}
	fmt.Fprint(stdout, result.Summary)
	return 0
}

func runNote(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("note", stderr)
	project := fs.String("project", "", "project id")
	title := fs.String("title", "", "note title")
	body := fs.String("body", "", "note body in Markdown")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "note", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	result, err := useCase.Note(context.Background(), app.NoteInput{
		ProjectID: *project,
		Title:     *title,
		Body:      *body,
	})
	if err != nil {
		return reportError(stderr, "note", err)
	}
	fmt.Fprintf(stdout, "note %s\n", result.Note.ID)
	return 0
}

func runDecision(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("decision", stderr)
	project := fs.String("project", "", "project id")
	title := fs.String("title", "", "decision title")
	status := fs.String("status", string(domain.DecisionProposed), "proposed, accepted, rejected, or superseded")
	contextText := fs.String("context", "", "decision context")
	alternatives := fs.String("alternatives", "", "alternatives considered")
	consequences := fs.String("consequences", "", "consequences")
	evidence := fs.String("evidence", "", "supporting evidence")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "decision", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	result, err := useCase.Decision(context.Background(), app.DecisionInput{
		ProjectID:    *project,
		Title:        *title,
		Status:       domain.DecisionStatus(*status),
		Context:      *contextText,
		Alternatives: splitList(*alternatives),
		Consequences: splitList(*consequences),
		Evidence:     splitList(*evidence),
	})
	if err != nil {
		return reportError(stderr, "decision", err)
	}
	fmt.Fprintf(stdout, "decision %s\n", result.Decision.ID)
	return 0
}

func runAttempt(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("attempt", stderr)
	project := fs.String("project", "", "project id")
	title := fs.String("title", "", "attempt title")
	hypothesis := fs.String("hypothesis", "", "what was expected")
	resultText := fs.String("result", "", "what happened")
	reason := fs.String("reason", "", "why it failed")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "attempt", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	result, err := useCase.Attempt(context.Background(), app.AttemptInput{
		ProjectID:  *project,
		Title:      *title,
		Hypothesis: *hypothesis,
		Result:     *resultText,
		Reason:     *reason,
	})
	if err != nil {
		return reportError(stderr, "attempt", err)
	}
	fmt.Fprintf(stdout, "attempt %s\n", result.Attempt.ID)
	return 0
}

func runHandoff(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("handoff", stderr)
	project := fs.String("project", "", "project id")
	summary := fs.String("summary", "", "handoff summary")
	next := fs.String("next", "", "next action")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "handoff", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	result, err := useCase.Handoff(context.Background(), app.HandoffInput{
		ProjectID:  *project,
		Summary:    *summary,
		NextAction: *next,
	})
	if err != nil {
		return reportError(stderr, "handoff", err)
	}
	fmt.Fprintf(stdout, "handoff %s\n", result.Handoff.ID)
	return 0
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stderr)
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "status", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	status, err := useCase.Status(context.Background())
	if err != nil {
		return reportError(stderr, "status", err)
	}
	fmt.Fprintf(stdout, "mode: %s\n", status.Mode)
	fmt.Fprintf(stdout, "data_dir: %s\n", status.DataDir)
	fmt.Fprintf(stdout, "server_url: %s\n", status.ServerURL)
	fmt.Fprintf(stdout, "user: %s\n", status.User)
	fmt.Fprintf(stdout, "schema_version: %d\n", status.SchemaVersion)
	fmt.Fprintf(stdout, "engine_version: %s\n", status.EngineVersion)
	fmt.Fprintf(stdout, "update_channel: %s\n", status.UpdateChannel)
	return 0
}

func runReview(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("review", stderr)
	project := fs.String("project", "", "project id")
	since := fs.String("since", "", "review window, for example 7d")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "review", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	result, err := useCase.Review(context.Background(), app.ReviewOptions{
		ProjectID: *project,
		Since:     *since,
	})
	if err != nil {
		return reportError(stderr, "review", err)
	}
	fmt.Fprintln(stdout, result.Summary)
	return 0
}

func runSync(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("sync", stderr)
	statusOnly := fs.Bool("status", false, "only show divergence and conflicts")
	push := fs.Bool("push", false, "push after committing")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "sync", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	result, err := useCase.Sync(context.Background(), app.SyncOptions{
		StatusOnly: *statusOnly,
		Push:       *push,
	})
	if err != nil {
		return reportError(stderr, "sync", err)
	}
	fmt.Fprintf(stdout, "ahead: %d\nbehind: %d\nchanged: %d\n", result.Ahead, result.Behind, result.Changed)
	return 0
}

func runServe(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("serve", stderr)
	defaultAddr := os.Getenv("BRAIN_ADDR")
	if defaultAddr == "" {
		defaultAddr = "127.0.0.1:8080"
	}
	addr := fs.String("addr", defaultAddr, "listen address")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if _, err := loadConfigOrDefault(*cfgPath); err != nil {
		return reportError(stderr, "serve", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stdout, "brain server listening on %s\n", *addr)
	if err := server.Run(ctx, *addr); err != nil {
		return reportError(stderr, "serve", err)
	}
	return 0
}

func runUpdate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("update", stderr)
	check := fs.Bool("check", false, "only check for a newer release")
	yes := fs.Bool("yes", false, "apply without prompting")
	channel := fs.String("channel", config.ChannelStable, "release channel: stable or beta")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "update", err)
	}
	useCase := app.New(cfg, app.Dependencies{})
	result, err := useCase.Update(context.Background(), app.UpdateOptions{
		Check:   *check,
		Yes:     *yes,
		Channel: *channel,
	})
	if err != nil {
		return reportError(stderr, "update", err)
	}
	fmt.Fprintf(stdout, "current: %s\nlatest: %s\napplied: %t\n", result.Current.Version, result.Latest.Version, result.Applied)
	return 0
}

func splitList(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
