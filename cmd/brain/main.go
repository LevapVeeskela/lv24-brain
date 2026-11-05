// Command brain is the LV24 Brain CLI.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/adapters/secrets"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/migrate"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/server"
	"github.com/levapveeskela/lv24-brain/internal/update"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

// buildRuntime constructs the CLI composition root. Tests replace it to inject
// a fake secret store; production always uses the default adapters.
var buildRuntime = bootstrap.Build

// osSecrets returns the OS secret store without environment shadowing. The
// environment-token logout path must inspect only what was persisted: with
// BRAIN_SECRET_SERVER_TOKEN set, the composite store would return the
// environment value for server-token and mistake it for a stored credential.
// Tests replace it alongside buildRuntime.
var osSecrets = func() ports.SecretStore {
	return secrets.New(secrets.Options{LookupEnv: func(string) (string, bool) { return "", false }})
}

const (
	// serverTokenEnv is the legacy static-token environment variable.
	serverTokenEnv = "BRAIN_TOKEN"
	// serverSecretTokenEnv is the environment form of the server-token secret.
	serverSecretTokenEnv = "BRAIN_SECRET_SERVER_TOKEN"
)

func main() {
	os.Exit(runWithInput(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithInput(args, os.Stdin, stdout, stderr)
}

func runWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	command, rest := args[0], args[1:]
	maybeDailyUpdateNotice(command, rest, stderr)
	switch command {
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	case "version", "-v", "--version":
		printVersion(stdout)
		return 0
	case "init":
		return runInit(rest, stdout, stderr)
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
	case "index":
		return runIndex(rest, stdout, stderr)
	case "search":
		return runSearch(rest, stdout, stderr)
	case "connector":
		return runConnector(rest, stdout, stderr)
	case "migrate":
		return runMigrate(rest, stdout, stderr)
	case "secret":
		return runSecret(rest, stdin, stdout, stderr)
	case "login":
		return runLogin(rest, stdin, stdout, stderr)
	case "logout":
		return runLogout(rest, stdout, stderr)
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
  index      Rebuild or refresh the local search index
  search     Search the local index
  connector  Run a read-only GitHub, GitLab, or IMAP connector
  migrate    Import a legacy .brain data directory
  secret     Manage OS-backed secrets (set, list, remove)
  login      Authenticate against a server-mode brain
  logout     Revoke the server session and clear stored tokens
  serve      Run the HTTP server
  update     Check for, apply, or roll back engine updates
  version    Print version information
  help       Show this help

Global flags:
  --config <path>   Path to brain.yaml (default: $BRAIN_CONFIG or brain.yaml)

Server mode authentication:
  Store a session with:
    brain login --login <name> --password-stdin
  Or use an administrator token from the environment:
    BRAIN_TOKEN=<token> brain status
  Or store a static API token in the OS secret store:
    brain secret set server-token
  The token value is read from stdin. BRAIN_TOKEN or
  BRAIN_SECRET_SERVER_TOKEN takes precedence and disables login/logout for
  that process. A static API token has no refresh token and cannot be
  revoked by brain logout.

Update:
  brain update --check      Check for a newer release
  brain update --yes        Apply the newest release
  brain update --rollback   Restore the previous engine binary

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

func initConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", os.Getenv("BRAIN_CONFIG"), "path to brain.yaml (default: <data-dir>/brain.yaml)")
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

func loadRequiredConfig(path string) (config.BrainConfig, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if config.IsNotExist(err) {
		return config.BrainConfig{}, fmt.Errorf("no brain instance at %s; run brain init first", path)
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

// staticServerTokenFromEnv reports whether a static server token shadows the
// stored credentials. Both names must disable login and logout.
func staticServerTokenFromEnv() bool {
	return strings.TrimSpace(os.Getenv(serverTokenEnv)) != "" ||
		strings.TrimSpace(os.Getenv(serverSecretTokenEnv)) != ""
}

// warnPlainHTTPLogin warns when credentials would cross the network without
// TLS. Loopback hosts are intentionally exempt.
func warnPlainHTTPLogin(w io.Writer, serverURL string) {
	parsed, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil || parsed.Scheme != "http" {
		return
	}
	host := parsed.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	fmt.Fprintf(w, "brain login: warning: credentials will be sent over plain HTTP to %s; use HTTPS\n", host)
}

func runInit(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("init", stderr)
	mode := fs.String("mode", config.ModeLocal, "install mode: local, git, or server")
	dataDir := fs.String("data-dir", "", "brain data directory")
	serverURL := fs.String("server-url", "", "brain server URL (server mode)")
	cfgPath := initConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := config.Default()
	cfg.Mode = *mode
	cfg.DataDir = *dataDir
	cfg.ServerURL = *serverURL
	useCase := app.New(cfg, app.Dependencies{})
	result, err := useCase.Init(context.Background(), app.InitOptions{
		ConfigPath: *cfgPath,
		Mode:       *mode,
		DataDir:    *dataDir,
		ServerURL:  *serverURL,
	})
	if err != nil {
		return reportError(stderr, "init", err)
	}
	fmt.Fprintf(stdout, "initialized %s brain\n", result.Mode)
	fmt.Fprintf(stdout, "config: %s\n", result.ConfigPath)
	if result.DataDir != "" {
		fmt.Fprintf(stdout, "data_dir: %s\n", result.DataDir)
	}
	if result.Mode == config.ModeServer {
		fmt.Fprintf(stdout, "next: run brain login --config %s (or set BRAIN_TOKEN)\n", result.ConfigPath)
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
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "doctor", err)
	}
	defer func() { _ = useCase.Close() }()
	report, err := useCase.App.Doctor(context.Background(), app.DoctorOptions{ConfigPath: *cfgPath})
	if err != nil {
		return reportError(stderr, "doctor", err)
	}
	failed := false
	for _, check := range report.Checks {
		fmt.Fprintf(stdout, "%s: %s (%s)\n", check.Status, check.Name, check.Message)
		if check.Status == "fail" {
			failed = true
		}
	}
	if failed {
		return 1
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
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "open", err)
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "open", err)
	}
	defer func() { _ = useCase.Close() }()
	result, err := useCase.App.Open(context.Background(), app.OpenOptions{ProjectID: *project})
	if err != nil {
		return reportError(stderr, "open", err)
	}
	printOpenResult(stdout, result)
	return 0
}

func printOpenResult(w io.Writer, result app.OpenResult) {
	name := result.Project.Name
	if name == "" {
		name = result.Project.ID
	}
	fmt.Fprintf(w, "project: %s (%s)\n", name, result.Project.ID)
	fmt.Fprintf(w, "kind: %s\n", result.Project.Kind)
	fmt.Fprintf(w, "goal: %s\n", valueOr(result.State.Goal, "not set"))
	printList(w, "active", result.State.Active)
	printList(w, "blocked", result.State.Blocked)
	fmt.Fprintf(w, "next_action: %s\n", valueOr(result.State.NextAction, "not set"))
	printList(w, "open_questions", result.State.OpenQuestions)
	if result.State.UpdatedAt.IsZero() {
		fmt.Fprintln(w, "state_updated: unknown")
	} else {
		fmt.Fprintf(w, "state_updated: %s\n", result.State.UpdatedAt.UTC().Format(time.RFC3339))
	}
	if result.State.FreshUntil.IsZero() {
		fmt.Fprintln(w, "fresh_until: not set")
	} else {
		fmt.Fprintf(w, "fresh_until: %s\n", result.State.FreshUntil.UTC().Format(time.RFC3339))
	}
}

func printList(w io.Writer, label string, values []string) {
	if len(values) == 0 {
		fmt.Fprintf(w, "%s: none\n", label)
		return
	}
	fmt.Fprintf(w, "%s:\n", label)
	for _, value := range values {
		fmt.Fprintf(w, "- %s\n", value)
	}
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func runBootstrap(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bootstrap", stderr)
	project := fs.String("project", "", "project id")
	budget := fs.Int("budget", app.DefaultBootstrapBudget, "summary size budget in runes")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "bootstrap", err)
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "bootstrap", err)
	}
	defer func() { _ = useCase.Close() }()
	result, err := useCase.App.Bootstrap(context.Background(), app.BootstrapOptions{
		ProjectID: *project,
		Budget:    *budget,
	})
	if err != nil {
		return reportError(stderr, "bootstrap", err)
	}
	printSummary(stdout, result.Summary)
	return 0
}

func printSummary(w io.Writer, summary string) {
	fmt.Fprint(w, summary)
	if !strings.HasSuffix(summary, "\n") {
		fmt.Fprintln(w)
	}
}

func runNote(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("note", stderr)
	project := fs.String("project", "", "project id")
	title := fs.String("title", "", "note title")
	body := fs.String("body", "", "note body in Markdown")
	tags := fs.String("tags", "", "comma-separated tags")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "note", err)
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "note", err)
	}
	defer func() { _ = useCase.Close() }()
	result, err := useCase.App.Note(context.Background(), app.NoteInput{
		ProjectID: *project,
		Title:     *title,
		Body:      *body,
		Tags:      splitList(*tags),
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
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "decision", err)
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "decision", err)
	}
	defer func() { _ = useCase.Close() }()
	result, err := useCase.App.Decision(context.Background(), app.DecisionInput{
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
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "attempt", err)
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "attempt", err)
	}
	defer func() { _ = useCase.Close() }()
	result, err := useCase.App.Attempt(context.Background(), app.AttemptInput{
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
	gitContext := fs.String("git-context", "", "git context for the handoff")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "handoff", err)
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "handoff", err)
	}
	defer func() { _ = useCase.Close() }()
	result, err := useCase.App.Handoff(context.Background(), app.HandoffInput{
		ProjectID:  *project,
		Summary:    *summary,
		GitContext: *gitContext,
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
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "review", err)
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "review", err)
	}
	defer func() { _ = useCase.Close() }()
	result, err := useCase.App.Review(context.Background(), app.ReviewOptions{
		ProjectID: *project,
		Since:     *since,
	})
	if err != nil {
		return reportError(stderr, "review", err)
	}
	printSummary(stdout, result.Summary)
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
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "sync", err)
	}
	useCase, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "sync", err)
	}
	defer func() { _ = useCase.Close() }()
	result, err := useCase.App.Sync(context.Background(), app.SyncOptions{
		StatusOnly: *statusOnly,
		Push:       *push,
	})
	if err != nil {
		return reportError(stderr, "sync", err)
	}
	if result.Provider != "" {
		fmt.Fprintf(stdout, "provider: %s\n", result.Provider)
	}
	fmt.Fprintf(stdout, "ahead: %d\nbehind: %d\nchanged: %d\n", result.Ahead, result.Behind, result.Changed)
	for _, conflict := range result.Conflicts {
		fmt.Fprintf(stdout, "conflict: %s\n", conflict)
	}
	return 0
}

func runIndex(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("index", stderr)
	rebuild := fs.Bool("rebuild", false, "delete and rebuild the local index from Markdown")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain index: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "index", err)
	}
	var runtime *bootstrap.Runtime
	if *rebuild {
		runtime, err = bootstrap.ResetIndex(cfg)
	} else {
		runtime, err = bootstrap.Build(cfg)
	}
	if err != nil {
		return reportError(stderr, "index", err)
	}
	defer func() { _ = runtime.Close() }()
	result, err := runtime.App.Reindex(context.Background(), app.IndexOptions{Rebuild: *rebuild})
	if err != nil {
		return reportError(stderr, "index", err)
	}
	fmt.Fprintf(stdout, "projects: %d\n", result.Projects)
	fmt.Fprintf(stdout, "states: %d\n", result.States)
	fmt.Fprintf(stdout, "notes: %d\n", result.Notes)
	fmt.Fprintf(stdout, "decisions: %d\n", result.Decisions)
	fmt.Fprintf(stdout, "attempts: %d\n", result.Attempts)
	fmt.Fprintf(stdout, "handoffs: %d\n", result.Handoffs)
	fmt.Fprintf(stdout, "documents: %d\n", result.Documents)
	fmt.Fprintf(stdout, "rebuilt: %t\n", result.Rebuild)
	return 0
}

func runSearch(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("search", stderr)
	project := fs.String("project", "", "project id filter")
	limit := fs.Int("limit", app.DefaultSearchLimit, "maximum results")
	cfgPath := configFlag(fs)
	flagArgs, queryParts := splitValueFlags(args, map[string]bool{
		"project": true,
		"limit":   true,
		"config":  true,
	})
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if len(queryParts) == 0 {
		fmt.Fprintln(stderr, "brain search: query is required")
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "brain search: --limit must not be negative")
		return 2
	}
	query := strings.TrimSpace(strings.Join(queryParts, " "))
	if query == "" {
		fmt.Fprintln(stderr, "brain search: query is required")
		return 1
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "search", err)
	}
	runtime, err := bootstrap.Build(cfg)
	if err != nil {
		return reportError(stderr, "search", err)
	}
	defer func() { _ = runtime.Close() }()
	hits, err := runtime.App.Search(context.Background(), query, *project, *limit)
	if err != nil {
		return reportError(stderr, "search", err)
	}
	for _, hit := range hits {
		fmt.Fprintf(stdout, "%s/%s/%s\n  %s\n", hit.Kind, hit.ProjectID, hit.Title, hit.Snippet)
	}
	return 0
}

func runConnector(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printConnectorUsage(stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		printConnectorUsage(stdout)
		return 0
	case "run":
		return runConnectorRun(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain connector: unknown subcommand %q\n\n", args[0])
		printConnectorUsage(stderr)
		return 2
	}
}

func runConnectorRun(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("connector run", stderr)
	name := fs.String("name", "", "github, gitlab, or imap")
	project := fs.String("project", "", "configured repository/project filter (ignored for imap)")
	sinceRaw := fs.String("since", "", "only items updated at or after this RFC3339 time")
	limit := fs.Int("limit", 0, "per-collection item limit (0 uses the connector default)")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain connector run: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	connectorName := strings.ToLower(strings.TrimSpace(*name))
	switch connectorName {
	case "github", "gitlab", "imap":
	default:
		if connectorName == "" {
			fmt.Fprintln(stderr, "brain connector run: --name is required")
		} else {
			fmt.Fprintf(stderr, "brain connector run: --name must be github, gitlab, or imap (got %q)\n", *name)
		}
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "brain connector run: --limit must not be negative")
		return 2
	}
	var since *time.Time
	if raw := strings.TrimSpace(*sinceRaw); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			fmt.Fprintf(stderr, "brain connector run: --since must be RFC3339: %v\n", err)
			return 2
		}
		utc := parsed.UTC()
		since = &utc
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "connector run", err)
	}
	connector, err := bootstrap.BuildConnector(cfg, bootstrap.ConnectorOptions{
		Name:    connectorName,
		Project: strings.TrimSpace(*project),
		Limit:   *limit,
	}, bootstrap.Options{})
	if err != nil {
		return reportError(stderr, "connector run", err)
	}
	useCase, err := bootstrap.OpenRunStore(cfg)
	if err != nil {
		return reportError(stderr, "connector run", err)
	}
	if useCase != nil {
		connector = connectors.Recording{Inner: connector, Runs: useCase}
	}
	runOptions := ports.ConnectorOptions{Since: since, Limit: *limit}
	if connectorName != "imap" && strings.TrimSpace(*project) != "" {
		runOptions.Projects = []string{strings.TrimSpace(*project)}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	run, err := connector.Run(ctx, runOptions)
	printConnectorRun(stdout, run)
	if err != nil {
		return reportError(stderr, "connector run", err)
	}
	if run.Status != domain.ConnectorSuccess {
		return 1
	}
	return 0
}

func printConnectorRun(w io.Writer, run domain.ConnectorRun) {
	if run.Source != "" {
		fmt.Fprintf(w, "source: %s\n", run.Source)
	}
	if run.Status != "" {
		fmt.Fprintf(w, "status: %s\n", run.Status)
	}
	keys := make([]string, 0, len(run.Counts))
	for key := range run.Counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(w, "count %s: %d\n", key, run.Counts[key])
	}
	if run.Error != "" {
		fmt.Fprintf(w, "error: %s\n", run.Error)
	}
}

func printConnectorUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  brain connector run --name github|gitlab|imap [flags]

Flags:
  --project <value>   GitHub owner/repository or GitLab project filter; ignored for imap
  --since <RFC3339>   Only collect items updated at or after this time
  --limit <n>         Per-collection item limit (0 uses the connector default)
  --config <path>     Path to brain.yaml

Environment:
  GitHub: BRAIN_GITHUB_REPOS (comma-separated owner/repository),
          BRAIN_GITHUB_BASE_URL (optional); token key github/token
  GitLab: BRAIN_GITLAB_PROJECTS (comma-separated), BRAIN_GITLAB_BASE_URL
          (optional); token key gitlab/token
  IMAP:   BRAIN_IMAP_ACCOUNT, BRAIN_IMAP_ADDRESS (host:port),
          BRAIN_IMAP_USERNAME, BRAIN_IMAP_PASSWORD_KEY (optional; default
          imap/<account-slug>/password), BRAIN_IMAP_FOLDERS (optional
          comma-separated), BRAIN_IMAP_STATE (optional cursor path)

For github/gitlab, --project also seeds the repository list when the matching
BRAIN_GITHUB_REPOS/BRAIN_GITLAB_PROJECTS variable is empty. For imap it is
ignored.

Credentials are resolved from BRAIN_SECRET_* environment variables or the OS
secret store. Store one with: brain secret set <token-key>
Connector dumps are written below the brain data directory; values are never
printed.
`)
}

func runMigrate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("migrate", stderr)
	from := fs.String("from", "", "legacy .brain data directory")
	dataDir := fs.String("data-dir", "", "target data directory")
	dryRun := fs.Bool("dry-run", false, "report without writing")
	cfgPath := initConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain migrate: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if strings.TrimSpace(*from) == "" {
		fmt.Fprintln(stderr, "brain migrate: --from is required")
		return 2
	}

	target := strings.TrimSpace(*dataDir)
	configPath := strings.TrimSpace(*cfgPath)
	if target == "" {
		loadPath := configPath
		if loadPath == "" {
			loadPath = config.FileName
		}
		cfg, err := config.Load(loadPath)
		if err != nil {
			return reportError(stderr, "migrate", err)
		}
		target = strings.TrimSpace(cfg.DataDir)
		if configPath == "" {
			configPath = loadPath
		}
	}
	if target == "" {
		fmt.Fprintln(stderr, "brain migrate: target data directory is required; pass --data-dir or set data_dir in brain.yaml")
		return 1
	}

	result, runErr := migrate.Run(context.Background(), migrate.Options{
		SourceDir: *from,
		TargetDir: target,
		DryRun:    *dryRun,
	})
	printMigrateResult(stdout, result)
	if runErr != nil {
		return reportError(stderr, "migrate", runErr)
	}
	if *dryRun {
		return 0
	}
	if err := registerMigratedProjects(result.TargetDir, result); err != nil {
		return reportError(stderr, "migrate", err)
	}
	if configPath == "" {
		configPath = filepath.Join(result.TargetDir, config.FileName)
	}
	if err := setMigratedSchema(configPath, result.TargetDir); err != nil {
		return reportError(stderr, "migrate", err)
	}
	return 0
}

func registerMigratedProjects(targetDir string, result migrate.Result) error {
	succeeded := make(map[string]struct{})
	for _, file := range result.Files {
		if file.Status == migrate.StatusCreated || file.Status == migrate.StatusUnchanged {
			succeeded[file.ProjectID] = struct{}{}
		}
	}
	store, err := fsstore.New(targetDir)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, projectID := range result.Projects {
		if _, ok := succeeded[projectID]; !ok {
			continue
		}
		_, getErr := store.GetProject(ctx, projectID)
		if getErr == nil {
			continue
		}
		if !errors.Is(getErr, domain.ErrNotFound) {
			return getErr
		}
		if err := store.PutProject(ctx, domain.Project{
			ID:   projectID,
			Name: projectID,
			Kind: domain.ProjectKindOther,
		}); err != nil {
			return err
		}
	}
	return nil
}

func printMigrateResult(w io.Writer, result migrate.Result) {
	fmt.Fprintf(w, "created   %d\n", result.Created)
	fmt.Fprintf(w, "unchanged %d\n", result.Unchanged)
	fmt.Fprintf(w, "conflicts %d\n", result.Conflicts)
	fmt.Fprintf(w, "failed    %d\n", result.Failed)
	for _, file := range result.Files {
		if file.Status != migrate.StatusConflict && file.Status != migrate.StatusFailed {
			continue
		}
		detail := file.SourcePath
		if file.TargetPath != "" {
			detail += " -> " + file.TargetPath
		}
		if file.Error != "" {
			detail += ": " + file.Error
		}
		fmt.Fprintf(w, "%s: %s\n", file.Status, detail)
	}
}

func setMigratedSchema(configPath, targetDir string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		if !config.IsNotExist(err) {
			return err
		}
		cfg = config.Default()
		cfg.Mode = migratedMode(targetDir)
		cfg.DataDir = targetDir
	}
	if cfg.DataDir == "" && cfg.Mode != config.ModeServer {
		cfg.DataDir = targetDir
	}
	if cfg.UpdateChannel == "" {
		cfg.UpdateChannel = config.ChannelStable
	}
	cfg.SchemaVersion = 1
	return config.Save(configPath, cfg)
}

func migratedMode(targetDir string) string {
	if _, err := os.Stat(filepath.Join(targetDir, ".git")); err == nil {
		return config.ModeGit
	}
	return config.ModeLocal
}

func runSecret(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printSecretUsage(stderr)
		return 2
	}
	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "help", "-h", "--help":
		printSecretUsage(stdout)
		return 0
	case "set":
		if len(rest) != 1 {
			printSecretUsage(stderr)
			return 2
		}
		value, err := readSecretValue(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "brain secret set: %v\n", err)
			return 1
		}
		useCase, closeRuntime, err := secretRuntime()
		if err != nil {
			return reportError(stderr, "secret set", err)
		}
		defer closeRuntime()
		name, err := useCase.SecretSet(context.Background(), rest[0], value)
		if err != nil {
			return reportError(stderr, "secret set", err)
		}
		fmt.Fprintln(stdout, name)
		return 0
	case "list":
		if len(rest) != 0 {
			printSecretUsage(stderr)
			return 2
		}
		useCase, closeRuntime, err := secretRuntime()
		if err != nil {
			return reportError(stderr, "secret list", err)
		}
		defer closeRuntime()
		names, err := useCase.SecretList(context.Background())
		if err != nil {
			return reportError(stderr, "secret list", err)
		}
		for _, name := range names {
			fmt.Fprintln(stdout, name)
		}
		return 0
	case "remove":
		if len(rest) != 1 {
			printSecretUsage(stderr)
			return 2
		}
		useCase, closeRuntime, err := secretRuntime()
		if err != nil {
			return reportError(stderr, "secret remove", err)
		}
		defer closeRuntime()
		if err := useCase.SecretRemove(context.Background(), rest[0]); err != nil {
			return reportError(stderr, "secret remove", err)
		}
		fmt.Fprintln(stdout, strings.TrimSpace(rest[0]))
		return 0
	default:
		fmt.Fprintf(stderr, "brain secret: unknown subcommand %q\n\n", subcommand)
		printSecretUsage(stderr)
		return 2
	}
}

func secretRuntime() (*app.App, func(), error) {
	runtime, err := bootstrap.Build(config.Default())
	if err != nil {
		return nil, func() {}, err
	}
	return runtime.App, func() { _ = runtime.Close() }, nil
}

func printSecretUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  brain secret set <name>      Read the value from stdin and store it
  brain secret list            List secret names
  brain secret remove <name>   Remove a secret from the OS store

Secret values are never printed. Environment-supplied secrets cannot be
overwritten or removed through this command.
`)
}

func readSecretValue(stdin io.Reader) (string, error) {
	if stdin == nil {
		return "", errors.New("stdin is not available")
	}
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	value := strings.TrimRight(line, "\r\n")
	if value == "" {
		return "", errors.New("value is required on stdin")
	}
	return value, nil
}

func runLogin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("login", stderr)
	login := fs.String("login", strings.TrimSpace(os.Getenv("BRAIN_LOGIN")), "user login (default: $BRAIN_LOGIN)")
	password := fs.String("password", "", "password; prefer --password-stdin")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from one stdin line")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain login: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *passwordStdin && *password != "" {
		fmt.Fprintln(stderr, "brain login: --password and --password-stdin are mutually exclusive")
		return 2
	}
	secret := *password
	if *passwordStdin {
		value, err := readSecretValue(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "brain login: %v\n", err)
			return 1
		}
		secret = value
	} else if secret == "" {
		secret = os.Getenv("BRAIN_PASSWORD")
	}
	if *password != "" {
		fmt.Fprintln(stderr, "brain login: warning: --password exposes the password in process arguments; prefer --password-stdin")
	}
	if strings.TrimSpace(*login) == "" {
		fmt.Fprintln(stderr, "brain login: --login is required")
		return 2
	}
	if secret == "" {
		fmt.Fprintln(stderr, "brain login: a password is required; use --password-stdin")
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "login", err)
	}
	if err := cfg.Validate(); err != nil {
		return reportError(stderr, "login", err)
	}
	if cfg.Mode != config.ModeServer {
		fmt.Fprintln(stderr, "brain login: server mode is required (set mode: server in brain.yaml)")
		return 1
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		fmt.Fprintln(stderr, "brain login: config.server_url is required")
		return 1
	}
	if staticServerTokenFromEnv() {
		fmt.Fprintln(stderr, "brain login: a static server token is set in the environment (BRAIN_TOKEN or BRAIN_SECRET_SERVER_TOKEN); unset it to log in with a user account")
		return 1
	}
	warnPlainHTTPLogin(stderr, cfg.ServerURL)
	useCase, err := buildRuntime(cfg)
	if err != nil {
		return reportError(stderr, "login", err)
	}
	defer func() { _ = useCase.Close() }()
	result, err := useCase.App.Login(context.Background(), app.LoginOptions{
		Login:    *login,
		Password: secret,
	})
	if err != nil {
		if errors.Is(err, httpclient.ErrUnauthorized) {
			fmt.Fprintln(stderr, "brain login: invalid login or password")
			return 1
		}
		return reportError(stderr, "login", err)
	}
	expires := "unknown"
	if !result.ExpiresAt.IsZero() {
		expires = result.ExpiresAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(stdout, "logged in as %s (%s), expires %s\n", result.User.Login, result.Role, expires)
	return 0
}

func runLogout(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("logout", stderr)
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain logout: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "logout", err)
	}
	if err := cfg.Validate(); err != nil {
		return reportError(stderr, "logout", err)
	}
	if cfg.Mode != config.ModeServer {
		fmt.Fprintln(stderr, "brain logout: server mode is required (set mode: server in brain.yaml)")
		return 1
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		fmt.Fprintln(stderr, "brain logout: config.server_url is required")
		return 1
	}
	if staticServerTokenFromEnv() {
		stored, err := storedCredentials(context.Background(), osSecrets())
		if err != nil {
			return reportError(stderr, "logout", err)
		}
		if !stored {
			fmt.Fprintln(stdout, "logged out: a static server token is set in the environment and takes precedence; no stored tokens to clear")
			return 0
		}
		fmt.Fprintln(stderr, "brain logout: warning: a static server token is set in the environment (BRAIN_TOKEN or BRAIN_SECRET_SERVER_TOKEN) and takes precedence; stored tokens were left unchanged. Unset it and rerun to log out stored credentials")
		return 1
	}
	useCase, err := buildRuntime(cfg)
	if err != nil {
		return reportError(stderr, "logout", err)
	}
	defer func() { _ = useCase.Close() }()
	if err := useCase.App.Logout(context.Background()); err != nil {
		switch {
		case errors.Is(err, httpclient.ErrWrongServer):
			fmt.Fprintf(stderr, "brain logout: warning: stored credentials belong to a different server; tokens were left unchanged: %v\n", err)
			return 1
		case errors.Is(err, httpclient.ErrStaticTokenNotRevoked):
			if err == httpclient.ErrStaticTokenNotRevoked {
				fmt.Fprintln(stderr, "brain logout: warning: local tokens were cleared, but a static API token cannot be revoked by this command; revoke the API token on the server")
			} else {
				fmt.Fprintf(stderr, "brain logout: warning: a static API token cannot be revoked by this command and local tokens were not fully cleared: %v\n", err)
			}
			return 1
		default:
			return reportError(stderr, "logout", err)
		}
	}
	fmt.Fprintln(stdout, "logged out")
	return 0
}

// storedCredentials reports whether the secret store still holds a session or
// a static token. The environment-token logout path must not claim success
// while a previously stored credential remains valid.
func storedCredentials(ctx context.Context, store ports.SecretStore) (bool, error) {
	if store == nil {
		return false, nil
	}
	for _, key := range []string{httpclient.SecretSession, httpclient.SecretAccessToken, httpclient.SecretRefreshToken} {
		value, err := store.Get(ctx, key)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			return false, fmt.Errorf("read %s: %w", key, err)
		}
		if strings.TrimSpace(value) != "" {
			return true, nil
		}
	}
	return false, nil
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
	rollback := fs.Bool("rollback", false, "restore the previous binary")
	channel := fs.String("channel", "", "release channel: stable or beta (default: brain.yaml)")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain update: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *rollback && strings.TrimSpace(*channel) != "" {
		fmt.Fprintln(stderr, "brain update: --channel cannot be combined with --rollback")
		return 2
	}
	if *rollback && (*check || *yes) {
		fmt.Fprintln(stderr, "brain update: --rollback cannot be combined with --check or --yes")
		return 2
	}
	cfg, err := loadConfigOrDefault(*cfgPath)
	if err != nil {
		return reportError(stderr, "update", err)
	}
	useCase, err := buildRuntime(cfg)
	if err != nil {
		return reportError(stderr, "update", err)
	}
	defer func() { _ = useCase.Close() }()
	result, err := useCase.App.Update(context.Background(), app.UpdateOptions{
		Check:    *check,
		Yes:      *yes,
		Channel:  *channel,
		Rollback: *rollback,
	})
	if err != nil {
		return reportError(stderr, "update", err)
	}
	if result.RolledBack {
		fmt.Fprintln(stdout, "rolled back to the previous binary")
		return 0
	}
	fmt.Fprintf(stdout, "current: %s\nlatest: %s\napplied: %t\n", result.Current.Version, result.Latest.Version, result.Applied)
	return 0
}

// maybeDailyUpdateNotice performs at most one notify-only release lookup per
// day before a command runs. Every failure is silent: the daily check must
// never block or change the exit code of the command the user asked for.
func maybeDailyUpdateNotice(command string, args []string, stderr io.Writer) {
	switch command {
	case "serve", "init", "help", "-h", "--help", "version", "-v", "--version", "update":
		return
	}
	cfg, err := loadConfigOrDefault(configPathFromArgs(args))
	if err != nil {
		return
	}
	channel := strings.ToLower(strings.TrimSpace(cfg.UpdateChannel))
	if channel == "" {
		channel = config.ChannelStable
	}
	result, err := update.MaybeDailyCheck(context.Background(), update.DailyOptions{
		Dir:     update.DefaultUpdateDir(runtime.GOOS, os.Getenv),
		Timeout: 3 * time.Second,
		Check: func(ctx context.Context) (ports.ReleaseInfo, error) {
			updater, updaterErr := update.New(update.Options{IgnoreDisabled: true})
			if updaterErr != nil {
				return ports.ReleaseInfo{}, updaterErr
			}
			return updater.Check(ctx, channel)
		},
	})
	if err != nil || !result.Checked || result.Latest == nil {
		return
	}
	latest := strings.TrimSpace(result.Latest.Version)
	if latest == "" || update.CompareVersions(latest, version.Version) <= 0 {
		return
	}
	fmt.Fprintf(stderr, "brain %s is available; run: brain update --yes\n", latest)
}

// configPathFromArgs finds --config before flag parsing so the daily check uses
// the same configuration as the command that follows.
func configPathFromArgs(args []string) string {
	path := os.Getenv("BRAIN_CONFIG")
	if path == "" {
		path = config.FileName
	}
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--config" || args[i] == "-config") && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(args[i], "--config="):
			return strings.TrimPrefix(args[i], "--config=")
		case strings.HasPrefix(args[i], "-config="):
			return strings.TrimPrefix(args[i], "-config=")
		}
	}
	return path
}

// splitValueFlags separates flags from positional arguments so a query may be
// written before or after flags. valueFlags names flags that consume a value.
func splitValueFlags(args []string, valueFlags map[string]bool) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if idx := strings.IndexByte(name, '='); idx >= 0 {
			name = name[:idx]
			continue
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, positional
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
