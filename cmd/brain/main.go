// Command brain is the LV24 Brain CLI.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

	"gopkg.in/yaml.v3"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/adapters/secrets"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
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

// runWithInput dispatches a command. A leaf `--help`/`-h` makes the standard
// flag package print the usage and fail with flag.ErrHelp, which the commands
// report as a usage error (2); asking for help is a success, so it maps to 0.
func runWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	code := runCommand(args, stdin, stdout, stderr)
	if code == 2 && len(args) > 1 && wantsHelp(args[1:]) {
		return 0
	}
	return code
}

func wantsHelp(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--":
			return false
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}

func runCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
	case "project":
		return runProject(rest, stdout, stderr)
	case "bootstrap":
		return runBootstrap(rest, stdout, stderr)
	case "context":
		return runContext(rest, stdout, stderr)
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
	case "promote":
		return runPromote(rest, stdout, stderr)
	case "sync":
		return runSync(rest, stdout, stderr)
	case "index":
		return runIndex(rest, stdout, stderr)
	case "search":
		return runSearch(rest, stdout, stderr)
	case "journal":
		return runJournal(rest, stdout, stderr)
	case "event":
		return runEvent(rest, stdout, stderr)
	case "hooks":
		return runHooks(rest, stdout, stderr)
	case "config":
		return runConfig(rest, stdout, stderr)
	case "discover":
		return runDiscover(rest, stdout, stderr)
	case "graphs":
		return runGraphs(rest, stdout, stderr)
	case "agents":
		return runAgents(rest, stdout, stderr)
	case "state":
		return runState(rest, stdout, stderr)
	case "record":
		return runRecord(rest, stdout, stderr)
	case "links":
		return runLinks(rest, stdout, stderr)
	case "catchup":
		return runCatchup(rest, stdout, stderr)
	case "digest":
		return runDigest(rest, stdout, stderr)
	case "session":
		return runSession(rest, stdin, stdout, stderr)
	case "mcp":
		return runMCP(rest, stdin, stdout, stderr)
	case "connector":
		return runConnector(rest, stdout, stderr)
	case "connection":
		return runConnection(rest, stdin, stdout, stderr)
	case "agent":
		return runAgent(rest, stdout, stderr)
	case "migrate":
		return runMigrate(rest, stdout, stderr)
	case "secret":
		return runSecret(rest, stdin, stdout, stderr)
	case "login":
		return runLogin(rest, stdin, stdout, stderr)
	case "logout":
		return runLogout(rest, stdout, stderr)
	case "password":
		return runPassword(rest, stdin, stdout, stderr)
	case "token":
		return runToken(rest, stdout, stderr)
	case "serve":
		return runServe(rest, stdout, stderr)
	case "update":
		return runUpdate(rest, stdout, stderr)
	case "completion":
		return runCompletion(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain: unknown command %q\n\n", command)
		printUsage(stderr)
		return 2
	}
}

func printVersion(w io.Writer) {
	via := ""
	if source := installSource(); source != "" {
		via = ", installed via " + source
	}
	fmt.Fprintf(w, "brain %s (commit %s, built %s, %s %s/%s%s)\n",
		version.Version, version.Commit, version.Date, runtime.Version(), runtime.GOOS, runtime.GOARCH, via)
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `LV24 Brain — local, git-backed, or server-backed project memory.

Usage:
  brain <command> [flags]

Commands:
  init       Create a new brain instance
  doctor     Check engine and data schema compatibility
  open       Open a project context
  project    Manage the project registry (list, show, set)
  bootstrap  Build agent context for a project
  context    Context for one task: state, findings, handoff, matching records
  note       Append a dated note
  decision   Append a decision record
  attempt    Append an attempt record
  handoff    Append a session handoff
  status     Show the configured instance
  review     Build a review report; --fix applies the safe fixes
  promote    Promote or dismiss a proposal candidate
  sync       Pull, commit, and push brain data
  index      Rebuild or refresh the local search index
  search     Search notes, decisions, attempts and handoffs (--dumps adds connector dumps)
  journal    Show the per-user activity journal (local/git)
  event      Record an external event, e.g. "event commit" from a git hook
  hooks      Install, remove or inspect the git post-commit journal hook
  config     Set or show the default brain.yaml (config use <path> | show | clear)
  discover   Find git checkouts under --root and register new projects (--apply)
  graphs     Refresh graphify/codegraph indexes of registered projects
  agents     Sync the shared agent instructions (agents sync [--check])
  state      Get or set a project's goal, next action, blockers and questions
  catchup    what changed since the latest handoff
  digest     Morning summary across projects: changes, and what needs you (--write saves it); --source <conn> previews proposals
  record     List, show, change status, trash and restore records
  links      [[id]] links and backlinks of a record; --broken lists dangling links
  session    start|end handlers for agent session hooks
  mcp        Serve the brain as an MCP server over stdio (for coding agents)
  connector  Run a read-only connector (see connector --help for providers)
  connection Manage local/git connections, credentials and provider descriptors
  agent      Run, inspect or stop the background connection agent
  migrate    Import a legacy .brain data directory; --rollback reverts an import
  secret     Manage OS-backed secrets (set, list, remove)
  login      Authenticate against a server-mode brain
  logout     Revoke the server session and clear stored tokens (--all: every session)
  password   Change your server password (reads current and new from stdin)
  token      List, create (scoped read/write/admin, projects, expiry) and revoke server API tokens
  serve      Run the HTTP server
  update     Check for, apply, or roll back engine updates
  completion Print a bash, zsh, fish or PowerShell completion script
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

Projects:
  brain project list [--json]
  brain project show <id> [--json]
  brain project set <id> [--name <name>] [--path <p>]... [--add-path <p>]... [--alias <a>]...
                    [--kind app|service|repository|infra|other]
                    [--repo <url>] [--branch <b>]
                    [--clear-paths] [--clear-aliases] [--json]

Update:
  brain update --check      Check for a newer release
  brain update --yes        Apply the newest release
  brain update --rollback   Restore the previous engine binary
  brain update --channel stable|beta --check
                            Check a specific release channel (default: brain.yaml)

Run "brain <command> --help" for command flags.
`)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", defaultConfigPath(), "path to brain.yaml (default: $BRAIN_CONFIG, ./brain.yaml, then the brain config use pointer)")
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
	asJSON := fs.Bool("json", false, "print JSON")
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
	payload := jsonDoctor{Checks: make([]jsonDoctorCheck, 0, len(report.Checks))}
	for _, check := range report.Checks {
		if !*asJSON {
			fmt.Fprintf(stdout, "%s: %s (%s)\n", check.Status, check.Name, check.Message)
		}
		payload.Checks = append(payload.Checks, jsonDoctorCheck{
			Name:   check.Name,
			OK:     check.Status != "fail",
			Status: check.Status,
			Detail: check.Message,
		})
		if check.Status == "fail" {
			failed = true
		}
	}
	if *asJSON {
		// The JSON document is printed even when a check fails; the exit
		// code still reports the failure.
		if code := writeJSONReport(stdout, stderr, "doctor", payload); code != 0 {
			return code
		}
	}
	if failed {
		return 1
	}
	return 0
}

func runOpen(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("open", stderr)
	project := fs.String("project", "", "project id or alias")
	budget := fs.Int("budget", app.DefaultBootstrapBudget, "summary size budget in runes")
	brief := fs.Bool("brief", false, "print only the project and state fields")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 1 {
		fmt.Fprintf(stderr, "brain open: unexpected arguments: %s\n", strings.Join(positional[1:], " "))
		return 2
	}
	selector := ""
	if len(positional) == 1 {
		selector = positional[0]
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
	ctx := context.Background()
	target, err := resolveTarget(ctx, useCase.App, *project, selector)
	if workspace, ok := app.AsWorkspace(err); ok && !*asJSON {
		// A workspace root: list its projects instead of failing.
		text, overviewErr := useCase.App.WorkspaceOverview(ctx, workspace)
		if overviewErr != nil {
			return reportError(stderr, "open", overviewErr)
		}
		fmt.Fprint(stdout, text)
		return 0
	}
	if err != nil {
		return reportError(stderr, "open", err)
	}
	if *brief {
		result, err := useCase.App.Open(ctx, app.OpenOptions{ProjectID: target.Project.ID})
		if err != nil {
			return reportError(stderr, "open", err)
		}
		if *asJSON {
			return writeJSONReport(stdout, stderr, "open", jsonOpenBrief{
				Project: toJSONProject(result.Project),
				State:   toJSONState(result.State),
			})
		}
		printOpenResult(stdout, result)
		return 0
	}
	result, err := useCase.App.Bootstrap(ctx, app.BootstrapOptions{ProjectID: target.Project.ID, Budget: *budget})
	if err != nil {
		return reportError(stderr, "open", err)
	}
	if *asJSON {
		opened, err := useCase.App.Open(ctx, app.OpenOptions{ProjectID: target.Project.ID})
		if err != nil {
			return reportError(stderr, "open", err)
		}
		decisions, attempts, handoffs, err := useCase.App.RecentRecords(ctx, target.Project.ID, 5)
		if err != nil {
			return reportError(stderr, "open", err)
		}
		summary, graphs := gitAndGraphsFor(ctx, target)
		return writeJSONReport(stdout, stderr, "open", jsonOpen{
			Recent: jsonRecent{
				Decisions: mapAll(decisions, toJSONDecision),
				Attempts:  mapAll(attempts, toJSONAttempt),
				Handoffs:  mapAll(handoffs, toJSONHandoff),
			},
			Project: toJSONProject(opened.Project),
			State:   toJSONState(opened.State),
			Git:     toJSONGit(summary),
			Graphs:  toJSONGraphs(graphs),
			Summary: result.Summary,
		})
	}
	printSummary(stdout, result.Summary)
	printGitAndGraphs(ctx, stdout, target)
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

// projectValueFlags lists flags that consume a value, so splitValueFlags can
// accept the positional project id before or after the flags.
var projectValueFlags = map[string]bool{
	"config":   true,
	"name":     true,
	"path":     true,
	"add-path": true,
	"alias":    true,
	"kind":     true,
	"repo":     true,
	"branch":   true,
}

func runProject(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printProjectUsage(stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		printProjectUsage(stdout)
		return 0
	case "list":
		return runProjectList(args[1:], stdout, stderr)
	case "show":
		return runProjectShow(args[1:], stdout, stderr)
	case "set":
		return runProjectSet(args[1:], stdout, stderr)
	case "members":
		return runProjectMembers(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "brain project: unknown subcommand %q\n\n", args[0])
		printProjectUsage(stderr)
		return 2
	}
}

func runProjectList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project list", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	flags, positional := splitValueFlags(args, projectValueFlags)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "brain project list: unexpected arguments: %s\n", strings.Join(positional, " "))
		return 2
	}
	useCase, closeRuntime, err := projectRuntime(*cfgPath)
	if err != nil {
		return reportError(stderr, "project list", err)
	}
	defer closeRuntime()
	projects, err := useCase.ListProjects(context.Background())
	if err != nil {
		return reportError(stderr, "project list", err)
	}
	if *asJSON {
		if projects == nil {
			projects = []domain.Project{}
		}
		if err := encodeJSON(stdout, projects); err != nil {
			return reportError(stderr, "project list", err)
		}
		return 0
	}
	for _, project := range projects {
		printProjectLine(stdout, project)
	}
	return 0
}

func runProjectShow(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project show", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	flags, positional := splitValueFlags(args, projectValueFlags)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if len(positional) != 1 {
		printProjectUsage(stderr)
		return 2
	}
	useCase, closeRuntime, err := projectRuntime(*cfgPath)
	if err != nil {
		return reportError(stderr, "project show", err)
	}
	defer closeRuntime()
	project, err := useCase.GetProject(context.Background(), positional[0])
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			fmt.Fprintf(stderr, "brain project show: project %q not found\n", strings.TrimSpace(positional[0]))
			return 1
		}
		return reportError(stderr, "project show", err)
	}
	if err := printProject(stdout, project, *asJSON); err != nil {
		return reportError(stderr, "project show", err)
	}
	return 0
}

func runProjectSet(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("project set", stderr)
	name := fs.String("name", "", "project display name")
	kind := fs.String("kind", "", "app, service, repository, infra, or other")
	repo := fs.String("repo", "", "repository URL")
	branch := fs.String("branch", "", "default branch")
	clearPaths := fs.Bool("clear-paths", false, "clear all paths")
	clearAliases := fs.Bool("clear-aliases", false, "clear all aliases")
	asJSON := fs.Bool("json", false, "print JSON")
	var paths, aliases []string
	seenPaths := map[string]bool{}
	seenAliases := map[string]bool{}
	var addPaths []string
	fs.Func("add-path", "append a filesystem path to the existing list (repeatable)", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("must not be empty")
		}
		addPaths = append(addPaths, value)
		return nil
	})
	fs.Func("path", "set the filesystem paths, replacing the stored list (repeatable; see --add-path)", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("must not be empty")
		}
		if seenPaths[value] {
			return nil
		}
		seenPaths[value] = true
		paths = append(paths, value)
		return nil
	})
	fs.Func("alias", "add a project alias (repeatable)", func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return errors.New("must not be empty")
		}
		if seenAliases[value] {
			return nil
		}
		seenAliases[value] = true
		aliases = append(aliases, value)
		return nil
	})
	cfgPath := configFlag(fs)
	flags, positional := splitValueFlags(args, projectValueFlags)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if len(positional) != 1 {
		printProjectUsage(stderr)
		return 2
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if *clearPaths && seen["path"] {
		fmt.Fprintln(stderr, "brain project set: --path cannot be combined with --clear-paths")
		return 2
	}
	if *clearAliases && seen["alias"] {
		fmt.Fprintln(stderr, "brain project set: --alias cannot be combined with --clear-aliases")
		return 2
	}
	if seen["kind"] && strings.TrimSpace(*kind) == "" {
		fmt.Fprintln(stderr, "brain project set: --kind must not be empty; use app, service, repository, infra, or other")
		return 2
	}

	input := app.ProjectInput{ID: positional[0]}
	if seen["name"] {
		value := *name
		input.Name = &value
	}
	if seen["path"] {
		value := append([]string{}, paths...)
		input.Paths = &value
	}
	if *clearPaths {
		value := []string{}
		input.Paths = &value
	}
	if seen["alias"] {
		value := append([]string{}, aliases...)
		input.Aliases = &value
	}
	if *clearAliases {
		value := []string{}
		input.Aliases = &value
	}
	if seen["kind"] {
		value := domain.ProjectKind(strings.TrimSpace(*kind))
		input.Kind = &value
	}
	if seen["repo"] {
		value := strings.TrimSpace(*repo)
		input.RepoURL = &value
	}
	if seen["branch"] {
		value := strings.TrimSpace(*branch)
		input.DefaultBranch = &value
	}

	useCase, closeRuntime, err := projectRuntime(*cfgPath)
	if err != nil {
		return reportError(stderr, "project set", err)
	}
	defer closeRuntime()
	if len(addPaths) > 0 {
		if *clearPaths {
			fmt.Fprintln(stderr, "brain project set: --add-path cannot be combined with --clear-paths")
			return 2
		}
		// Start from --path when given, else from the stored list, and
		// append the new paths once each.
		base := paths
		if !seen["path"] {
			if existing, err := useCase.GetProject(context.Background(), input.ID); err == nil {
				base = existing.Paths
			} else if !errors.Is(err, domain.ErrNotFound) {
				return reportError(stderr, "project set", err)
			}
		}
		merged := append([]string{}, base...)
		for _, path := range addPaths {
			if !containsString(merged, path) {
				merged = append(merged, path)
			}
		}
		input.Paths = &merged
	}
	project, err := useCase.SaveProject(context.Background(), input)
	if err != nil {
		return reportError(stderr, "project set", err)
	}
	if err := printProject(stdout, project, *asJSON); err != nil {
		return reportError(stderr, "project set", err)
	}
	return 0
}

func projectRuntime(cfgPath string) (*app.App, func(), error) {
	cfg, err := loadRequiredConfig(cfgPath)
	if err != nil {
		return nil, func() {}, err
	}
	runtime, err := buildRuntime(cfg)
	if err != nil {
		return nil, func() {}, err
	}
	return runtime.App, func() { _ = runtime.Close() }, nil
}

func printProject(w io.Writer, project domain.Project, asJSON bool) error {
	if asJSON {
		return encodeJSON(w, project)
	}
	printProjectDetail(w, project)
	return nil
}

func printProjectLine(w io.Writer, project domain.Project) {
	fmt.Fprintf(w, "%s  %s  %s  %s  %s  %s\n",
		project.ID,
		valueOr(string(project.Kind), "-"),
		valueOr(project.Name, "-"),
		joinValues(project.Paths),
		valueOr(project.RepoURL, "-"),
		valueOr(project.DefaultBranch, "-"),
	)
}

func printProjectDetail(w io.Writer, project domain.Project) {
	fmt.Fprintf(w, "id: %s\n", project.ID)
	fmt.Fprintf(w, "name: %s\n", valueOr(project.Name, "-"))
	fmt.Fprintf(w, "kind: %s\n", valueOr(string(project.Kind), "-"))
	fmt.Fprintf(w, "paths: %s\n", joinValues(project.Paths))
	fmt.Fprintf(w, "aliases: %s\n", joinValues(project.Aliases))
	fmt.Fprintf(w, "repo: %s\n", valueOr(project.RepoURL, "-"))
	fmt.Fprintf(w, "branch: %s\n", valueOr(project.DefaultBranch, "-"))
	if project.CreatedAt.IsZero() {
		fmt.Fprintln(w, "created_at: unknown")
	} else {
		fmt.Fprintf(w, "created_at: %s\n", project.CreatedAt.UTC().Format(time.RFC3339))
	}
	if project.UpdatedAt.IsZero() {
		fmt.Fprintln(w, "updated_at: unknown")
	} else {
		fmt.Fprintf(w, "updated_at: %s\n", project.UpdatedAt.UTC().Format(time.RFC3339))
	}
}

func joinValues(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ", ")
}

func printProjectUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  brain project list [--json]
  brain project show <id> [--json]
  brain project set <id> [--name <name>] [--path <p>]... [--add-path <p>]... [--alias <a>]...
                    [--kind app|service|repository|infra|other]
                    [--repo <url>] [--branch <b>]
                    [--clear-paths] [--clear-aliases] [--json]
  brain project members <id> [add <login> --role viewer|member|maintainer | remove <login> | restrict | open]

Repeat --path and --alias to replace the stored lists; --add-path appends to the
stored paths (one checkout per machine, e.g. Windows and macOS). --clear-paths and
--clear-aliases store an explicitly empty list and cannot be combined with the
corresponding repeatable flag. Creating a project without --name uses the id
as its name. Omitted flags keep their stored values.
`)
}

func runBootstrap(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("bootstrap", stderr)
	project := fs.String("project", "", "project id or alias")
	budget := fs.Int("budget", app.DefaultBootstrapBudget, "summary size budget in runes")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 1 {
		fmt.Fprintf(stderr, "brain bootstrap: unexpected arguments: %s\n", strings.Join(positional[1:], " "))
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
	selector := ""
	if len(positional) == 1 {
		selector = positional[0]
	}
	target, err := resolveTarget(context.Background(), useCase.App, *project, selector)
	if err != nil {
		return reportError(stderr, "bootstrap", err)
	}
	result, err := useCase.App.Bootstrap(context.Background(), app.BootstrapOptions{
		ProjectID: target.Project.ID,
		Budget:    *budget,
	})
	if err != nil {
		return reportError(stderr, "bootstrap", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "bootstrap", jsonBootstrap{
			ProjectID: result.ProjectID,
			Summary:   result.Summary,
			Budget:    effectiveBudget(*budget),
		})
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
	create := fs.Bool("create", false, "register --project first if it does not exist yet")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if text := strings.TrimSpace(strings.Join(positional, " ")); text != "" {
		if *body != "" {
			fmt.Fprintln(stderr, "brain note: pass the text either positionally or with --body, not both")
			return 2
		}
		*body = text
		if *title == "" {
			*title = splitTitle(text)
		}
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
		CreateProject: *create,
		ProjectID:     *project,
		Title:         *title,
		Body:          *body,
		Tags:          splitList(*tags),
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
	paths := fs.String("paths", "", "repo-relative files or directories this decision governs, comma-separated")
	create := fs.Bool("create", false, "register --project first if it does not exist yet")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if text := strings.TrimSpace(strings.Join(positional, " ")); text != "" {
		if *title != "" {
			fmt.Fprintln(stderr, "brain decision: pass the title either positionally or with --title, not both")
			return 2
		}
		*title = text
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
		CreateProject: *create,
		ProjectID:     *project,
		Title:         *title,
		Status:        domain.DecisionStatus(*status),
		Context:       *contextText,
		Alternatives:  splitList(*alternatives),
		Consequences:  splitList(*consequences),
		Evidence:      splitList(*evidence),
		Paths:         splitList(*paths),
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
	create := fs.Bool("create", false, "register --project first if it does not exist yet")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if text := strings.TrimSpace(strings.Join(positional, " ")); text != "" {
		if *title != "" {
			fmt.Fprintln(stderr, "brain attempt: pass the title either positionally or with --title, not both")
			return 2
		}
		*title = text
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
		CreateProject: *create,
		ProjectID:     *project,
		Title:         *title,
		Hypothesis:    *hypothesis,
		Result:        *resultText,
		Reason:        *reason,
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
	gitContext := fs.String("git-context", "", "git context for the handoff (default: read from the project checkout)")
	noGit := fs.Bool("no-git", false, "do not record git context")
	create := fs.Bool("create", false, "register --project first if it does not exist yet")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if text := strings.TrimSpace(strings.Join(positional, " ")); text != "" {
		if *summary != "" {
			fmt.Fprintln(stderr, "brain handoff: pass the summary either positionally or with --summary, not both")
			return 2
		}
		*summary = text
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
	ctx := context.Background()
	target, err := resolveTarget(ctx, useCase.App, *project, "")
	if err != nil && *create && strings.TrimSpace(*project) != "" {
		// A new project has no checkout yet; the app registers it below.
		target, err = app.Resolved{Project: domain.Project{ID: strings.TrimSpace(*project)}}, nil
	}
	if err != nil {
		return reportError(stderr, "handoff", err)
	}
	*project = target.Project.ID
	if *gitContext == "" && !*noGit {
		*gitContext = gitinfo.Read(ctx, gitDir(target)).Context()
	}
	result, err := useCase.App.Handoff(ctx, app.HandoffInput{
		CreateProject: *create,
		ProjectID:     *project,
		Summary:       *summary,
		GitContext:    *gitContext,
		NextAction:    *next,
	})
	if err != nil {
		return reportError(stderr, "handoff", err)
	}
	fmt.Fprintf(stdout, "handoff %s\n", result.Handoff.ID)
	return 0
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
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
	if *asJSON {
		return writeJSONReport(stdout, stderr, "status", jsonStatus{
			Mode:          status.Mode,
			DataDir:       status.DataDir,
			ServerURL:     status.ServerURL,
			User:          status.User,
			SchemaVersion: status.SchemaVersion,
			EngineVersion: status.EngineVersion,
			UpdateChannel: status.UpdateChannel,
		})
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
	project := fs.String("project", "", "project id or alias")
	since := fs.String("since", "", "review window, for example 7d")
	fix := fs.Bool("fix", false, "apply the fixable findings")
	fixKind := fs.String("fix-kind", "", "comma-separated finding kinds to fix (implies --fix)")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	kinds, err := app.ParseFindingKinds(*fixKind)
	if err != nil {
		return reportError(stderr, "review", err)
	}
	fixing := *fix || *fixKind != ""
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, positional, stderr, "review")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	projectID := target.Project.ID

	var applied []app.Finding
	if fixing {
		fixed, err := runtime.App.FixFindings(ctx, projectID, kinds)
		if err != nil {
			return reportError(stderr, "review", err)
		}
		applied = fixed.Applied
	}
	findings, err := runtime.App.Findings(ctx, projectID)
	if err != nil {
		return reportError(stderr, "review", err)
	}
	proposalFindings, err := runtime.App.ProposalFindings(ctx, projectID)
	if err != nil {
		return reportError(stderr, "review", err)
	}
	findings = append(findings, proposalFindings...)
	result, err := runtime.App.Review(ctx, app.ReviewOptions{
		ProjectID: projectID,
		Since:     *since,
	})
	if err != nil {
		return reportError(stderr, "review", err)
	}
	if *asJSON {
		items := result.Items
		if items == nil {
			items = []string{}
		}
		return writeJSONReport(stdout, stderr, "review", jsonReview{
			Summary:   result.Summary,
			Items:     items,
			Findings:  toJSONFindings(findings),
			Applied:   toJSONFindings(applied),
			Proposals: toJSONReviewProposals(result.Proposals),
		})
	}
	printSummary(stdout, result.Summary)
	if len(applied) > 0 {
		fmt.Fprintln(stdout, "## Applied")
		for _, finding := range applied {
			fmt.Fprintf(stdout, "- [%s] %s\n", finding.Kind, finding.Fix)
		}
	}
	printFindings(stdout, findings)
	if !fixing && anyFixable(findings) {
		fmt.Fprintln(stdout, "run `brain review --fix` to apply the fixable findings")
	}
	return 0
}

func printFindings(w io.Writer, findings []app.Finding) {
	fmt.Fprintln(w, "## Findings")
	if len(findings) == 0 {
		fmt.Fprintln(w, "- none")
		return
	}
	for _, finding := range findings {
		fmt.Fprintf(w, "- [%s] %s", finding.Kind, finding.Message)
		if finding.Evidence != "" {
			fmt.Fprintf(w, " (evidence: %s)", finding.Evidence)
		}
		if finding.Fix != "" {
			fmt.Fprintf(w, " -> fix: %s", finding.Fix)
		}
		fmt.Fprintln(w)
	}
}

func anyFixable(findings []app.Finding) bool {
	for _, finding := range findings {
		if finding.Fixable {
			return true
		}
	}
	return false
}

func toJSONFindings(findings []app.Finding) []jsonFinding {
	return mapAll(findings, func(finding app.Finding) jsonFinding {
		return jsonFinding{
			Kind: string(finding.Kind), Message: finding.Message, Evidence: finding.Evidence,
			Fixable: finding.Fixable, Fix: finding.Fix,
		}
	})
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
	if result.Reindexed {
		fmt.Fprintln(stdout, "reindexed: pulled records are searchable")
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
	fmt.Fprintf(stdout, "dumps: %d\n", result.Dumps)
	fmt.Fprintf(stdout, "items: %d\n", result.Items)
	fmt.Fprintf(stdout, "documents: %d\n", result.Documents)
	fmt.Fprintf(stdout, "pruned: %d\n", result.Pruned)
	fmt.Fprintf(stdout, "rebuilt: %t\n", result.Rebuild)
	return 0
}

func runSearch(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("search", stderr)
	project := fs.String("project", "", "project id filter")
	limit := fs.Int("limit", app.DefaultSearchLimit, "maximum results")
	kinds := fs.String("kind", "", "comma-separated kinds to keep: note, decision, attempt, handoff, dump, item (raw-zone external items; excluded by default)")
	dumps := fs.Bool("dumps", false, "include connector dumps (mail, GitHub/GitLab issues and PRs)")
	since := fs.String("since", "", "only documents updated since: duration (24h, 7d), date (2026-11-01) or RFC3339 time")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	flagArgs, queryParts := splitValueFlags(args, map[string]bool{
		"project": true,
		"limit":   true,
		"kind":    true,
		"since":   true,
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
	sinceTime, err := domain.ParseSince(*since, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "brain search: --%v\n", err)
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
	hits, err := runtime.App.SearchWith(context.Background(), app.SearchOptions{
		Text:         query,
		ProjectID:    *project,
		Limit:        *limit,
		Kinds:        splitList(*kinds),
		IncludeDumps: *dumps,
		Since:        sinceTime,
	})
	if err != nil {
		return reportError(stderr, "search", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "search", jsonSearch{Query: query, Hits: toJSONHits(hits)})
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
	providers := connectorbuild.SnapshotProviders()
	name := fs.String("name", "", strings.Join(providers, ", "))
	project := fs.String("project", "", "configured repository/project filter (ignored for imap)")
	brainProject := fs.String("brain-project", "", "map server raw items to this existing brain project")
	account := fs.String("account", "", "imap account id from connectors/mail.json")
	all := fs.Bool("all", false, "run configured connections; fall back to legacy targets when catalogue is empty (local/git)")
	days := fs.Int("days", 0, "only items updated in the last N days (alternative to --since)")
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
	switch {
	case *all && connectorName != "":
		fmt.Fprintln(stderr, "brain connector run: pass either --all or --name")
		return 2
	case *all:
	case containsString(providers, connectorName):
	default:
		if connectorName == "" {
			fmt.Fprintln(stderr, "brain connector run: --name is required")
		} else {
			fmt.Fprintf(stderr, "brain connector run: --name must be one of %s (got %q)\n", strings.Join(providers, ", "), *name)
		}
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "brain connector run: --limit must not be negative")
		return 2
	}
	if *days < 0 || (*days > 0 && strings.TrimSpace(*sinceRaw) != "") {
		fmt.Fprintln(stderr, "brain connector run: --days must be positive and cannot be combined with --since")
		return 2
	}
	var since *time.Time
	if *days > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -*days)
		since = &cutoff
	}
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
	opts := bootstrap.ConnectorOptions{
		Name:         connectorName,
		Project:      strings.TrimSpace(*project),
		BrainProject: strings.TrimSpace(*brainProject),
		Limit:        *limit,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Mode == config.ModeServer {
		if *all || *account != "" {
			fmt.Fprintln(stderr, "brain connector run: --all and --account need local or git mode (the server is configured by environment)")
			return 2
		}
		run, err := bootstrap.RunServerConnector(ctx, cfg, opts, since)
		printConnectorRun(stdout, run)
		if err != nil {
			return reportError(stderr, "connector run", err)
		}
		if run.Status != domain.ConnectorSuccess {
			return 1
		}
		return 0
	}

	if *all {
		catalogue, err := buildConnections(cfg, bootstrap.Options{})
		if err != nil {
			return reportError(stderr, "connector run", err)
		}
		connections, err := catalogue.Connections.List(ctx)
		if err != nil {
			return reportError(stderr, "connector run", err)
		}
		if len(connections) > 0 {
			code := 0
			for _, connection := range connections {
				if *project != "" && !containsString(connection.Projects, *project) {
					continue
				}
				fmt.Fprintf(stdout, "== %s\n", connection.ID)
				if runConfiguredConnection(ctx, cfg, connection.ID, since, *limit, false, stdout, stderr) != 0 {
					code = 1
				}
			}
			return code
		}
	}
	plans, err := planLocalConnectors(ctx, cfg, connectorName, strings.TrimSpace(*project), strings.TrimSpace(*account), *all, *limit)
	if err != nil {
		return reportError(stderr, "connector run", err)
	}
	if !runLocalConnectors(ctx, cfg, plans, since, stdout, stderr) {
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
	providers := connectorbuild.SnapshotProviders()
	fmt.Fprintf(w, `Usage:
  brain connector run --name %s [flags]
  brain connector run --all [--days N]

Flags:
  --project <value>   GitHub owner/repository or GitLab project filter; ignored for imap
  --account <id>      IMAP account from connectors/mail.json (password key imap/<id>/password)
  --all              %s targets + every mail.json account, one after another
  --days <n>          Only collect items updated in the last n days
  --since <RFC3339>   Only collect items updated at or after this time
  --limit <n>         Per-collection item limit (0 uses the connector default)
  --config <path>     Path to brain.yaml

Targets (local/git) default to the brain data: GitHub repositories from
registered projects' repo URLs, GitLab projects from repo URLs and the
"gitlab"/"origin" remotes of their checkouts, and IMAP accounts from
connectors/mail.json. Without a github/token secret the GitHub token is read
from "gh auth token" when the GitHub CLI is signed in. The environment below
overrides these defaults.

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
In local/git mode connector dumps are written below the brain data directory.
In server mode the command calls POST /v1/connectors/run; the server stores the
run record (PostgreSQL when configured) and discards bulk dumps. The run record
is exported by GET /v1/connectors/runs/{id}/markdown. Values are never printed.
`, strings.Join(providers, "|"), strings.Join(providers, " + "))
}

func runMigrate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("migrate", stderr)
	from := fs.String("from", "", "legacy .brain data directory")
	dataDir := fs.String("data-dir", "", "target data directory")
	dryRun := fs.Bool("dry-run", false, "report without writing")
	rollback := fs.String("rollback", "", "revert a migration: <manifest.json> or latest")
	cfgPath := initConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain migrate: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	if setFlags["rollback"] {
		if setFlags["from"] {
			fmt.Fprintln(stderr, "brain migrate: --rollback cannot be combined with --from")
			return 2
		}
		if strings.TrimSpace(*rollback) == "" {
			fmt.Fprintln(stderr, "brain migrate: --rollback requires a manifest path or latest")
			return 2
		}
		return runMigrateRollback(*rollback, *dataDir, *cfgPath, *dryRun, stdout, stderr)
	}
	if strings.TrimSpace(*from) == "" {
		fmt.Fprintln(stderr, "brain migrate: --from is required")
		return 2
	}

	target, configPath, err := resolveMigrateTarget(*dataDir, *cfgPath)
	if err != nil {
		return reportError(stderr, "migrate", err)
	}
	opts := migrate.Options{
		SourceDir: *from,
		TargetDir: target,
		DryRun:    *dryRun,
	}
	var backup *migrate.Backup
	if !*dryRun {
		backup, err = migrate.BeginBackup(opts, configPath, time.Now())
		if err != nil {
			return reportError(stderr, "migrate", err)
		}
	}

	result, runErr := migrate.Run(context.Background(), opts)
	printMigrateResult(stdout, result)
	if runErr == nil && !*dryRun {
		runErr = finishMigratedTargets(result, configPath)
	}
	if backup != nil {
		manifestPath, manifestErr := backup.Finish(result)
		if manifestErr != nil {
			manifestErr = fmt.Errorf("write rollback manifest: %w", manifestErr)
			if runErr == nil {
				runErr = manifestErr
			} else {
				fmt.Fprintf(stderr, "brain migrate: %v\n", manifestErr)
			}
		} else {
			fmt.Fprintf(stdout, "manifest  %s\n", manifestPath)
		}
	}
	if runErr != nil {
		return reportError(stderr, "migrate", runErr)
	}
	return 0
}

// resolveMigrateTarget returns the target data directory and the config path
// for migrate. Without --data-dir the data directory comes from brain.yaml.
func resolveMigrateTarget(dataDir, cfgPath string) (string, string, error) {
	target := strings.TrimSpace(dataDir)
	configPath := strings.TrimSpace(cfgPath)
	if target == "" {
		loadPath := configPath
		if loadPath == "" {
			loadPath = config.FileName
		}
		cfg, err := config.Load(loadPath)
		if err != nil {
			return "", "", err
		}
		target = strings.TrimSpace(cfg.DataDir)
		if configPath == "" {
			configPath = loadPath
		}
	}
	if target == "" {
		return "", "", fmt.Errorf("target data directory is required; pass --data-dir or set data_dir in brain.yaml")
	}
	if configPath == "" {
		configPath = filepath.Join(target, config.FileName)
	}
	return target, configPath, nil
}

// finishMigratedTargets updates the registry and the config after a successful import.
func finishMigratedTargets(result migrate.Result, configPath string) error {
	if err := registerMigratedProjects(result.TargetDir, result.SourceDir, result); err != nil {
		return err
	}
	return setMigratedSchema(configPath, result.TargetDir)
}

// runMigrateRollback reverts a migration. spec is a manifest path or "latest".
// Exit code 1 means some files were kept because they changed after migration.
func runMigrateRollback(spec, dataDir, cfgPath string, dryRun bool, stdout, stderr io.Writer) int {
	manifestPath := strings.TrimSpace(spec)
	flagsGiven := strings.TrimSpace(dataDir) != "" || strings.TrimSpace(cfgPath) != ""
	if manifestPath == "latest" {
		target, _, err := resolveMigrateTarget(dataDir, cfgPath)
		if err != nil {
			return reportError(stderr, "migrate", err)
		}
		manifestPath, err = migrate.FindLatestManifest(target)
		if err != nil {
			return reportError(stderr, "migrate", err)
		}
	} else if flagsGiven {
		manifest, err := migrate.LoadManifest(manifestPath)
		if err != nil {
			return reportError(stderr, "migrate", err)
		}
		target, _, err := resolveMigrateTarget(dataDir, cfgPath)
		if err != nil {
			return reportError(stderr, "migrate", err)
		}
		if !samePathForMigrate(target, manifest.TargetDir) {
			return reportError(stderr, "migrate", fmt.Errorf("manifest belongs to %s, not %s", manifest.TargetDir, target))
		}
	}
	result, err := migrate.Rollback(context.Background(), migrate.RollbackOptions{
		ManifestPath: manifestPath,
		DryRun:       dryRun,
	})
	if err != nil {
		return reportError(stderr, "migrate", err)
	}
	printRollbackResult(stdout, result)
	if result.Skipped > 0 {
		return 1
	}
	return 0
}

func samePathForMigrate(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(absA, absB)
	}
	return absA == absB
}

func printRollbackResult(w io.Writer, result migrate.RollbackResult) {
	fmt.Fprintf(w, "manifest  %s\n", result.ManifestPath)
	fmt.Fprintf(w, "target    %s\n", result.TargetDir)
	if result.DryRun {
		fmt.Fprintln(w, "dry run: nothing was changed")
	}
	for _, entry := range result.Entries {
		detail := entry.Path
		if entry.Detail != "" {
			detail += ": " + entry.Detail
		}
		fmt.Fprintf(w, "%s: %s\n", entry.Status, detail)
	}
	fmt.Fprintf(w, "skipped   %d\n", result.Skipped)
}

// legacyRegistryEntry is the subset of the legacy .brain registry.json used to
// enrich migrated projects. Unknown fields are ignored.
type legacyRegistryEntry struct {
	ID      string   `json:"id"`
	Path    string   `json:"path"`
	Kind    string   `json:"kind"`
	Repo    string   `json:"repo"`
	Branch  string   `json:"branch"`
	Aliases []string `json:"aliases"`
}

type legacyRegistry struct {
	Projects []legacyRegistryEntry `json:"projects"`
}

// readLegacyRegistry parses <source>/registry.json. A missing, malformed or
// BOM-prefixed file is not fatal: migration still registers project ids, just
// without repository metadata.
func readLegacyRegistry(sourceDir string) map[string]legacyRegistryEntry {
	raw, err := os.ReadFile(filepath.Join(sourceDir, "registry.json"))
	if err != nil {
		return nil
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	var registry legacyRegistry
	if err := json.Unmarshal(raw, &registry); err != nil {
		return nil
	}
	entries := make(map[string]legacyRegistryEntry, len(registry.Projects))
	for _, entry := range registry.Projects {
		if strings.TrimSpace(entry.ID) != "" {
			entries[entry.ID] = entry
		}
	}
	return entries
}

// migratedProjectKind maps the legacy kind vocabulary onto domain.ProjectKind.
func migratedProjectKind(kind string) domain.ProjectKind {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "app":
		return domain.ProjectKindApp
	case "service":
		return domain.ProjectKindService
	case "infra":
		return domain.ProjectKindInfra
	case "repo", "repository", "monorepo", "worktree":
		return domain.ProjectKindRepository
	default:
		return domain.ProjectKindOther
	}
}

// registerMigratedProjects creates or enriches registry entries for imported
// projects. Legacy registry metadata (path, repo, branch, kind) fills only
// empty fields, so a curated registry is never overwritten by stale legacy
// values. Existing entries are rewritten only when something actually changed.
func registerMigratedProjects(targetDir, sourceDir string, result migrate.Result) error {
	succeeded := make(map[string]struct{})
	for _, file := range result.Files {
		if file.Status == migrate.StatusCreated || file.Status == migrate.StatusUnchanged {
			succeeded[file.ProjectID] = struct{}{}
		}
	}
	legacy := readLegacyRegistry(sourceDir)
	store, err := fsstore.New(targetDir)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, projectID := range result.Projects {
		if _, ok := succeeded[projectID]; !ok {
			continue
		}
		meta, hasMeta := legacy[projectID]
		current, getErr := store.GetProject(ctx, projectID)
		exists := getErr == nil
		if getErr != nil && !errors.Is(getErr, domain.ErrNotFound) {
			return getErr
		}
		project := domain.Project{ID: projectID, Name: projectID, Kind: domain.ProjectKindOther}
		if exists {
			project = current
		}
		changed := !exists
		if project.Name == "" {
			project.Name = projectID
			changed = true
		}
		if hasMeta {
			if len(project.Paths) == 0 && strings.TrimSpace(meta.Path) != "" {
				project.Paths = []string{strings.TrimSpace(meta.Path)}
				changed = true
			}
			if project.RepoURL == "" && strings.TrimSpace(meta.Repo) != "" {
				project.RepoURL = strings.TrimSpace(meta.Repo)
				changed = true
			}
			if project.DefaultBranch == "" && strings.TrimSpace(meta.Branch) != "" {
				project.DefaultBranch = strings.TrimSpace(meta.Branch)
				changed = true
			}
			if len(project.Aliases) == 0 && len(meta.Aliases) > 0 {
				project.Aliases = append([]string(nil), meta.Aliases...)
				changed = true
			}
			if project.Kind == "" || project.Kind == domain.ProjectKindOther {
				if kind := migratedProjectKind(meta.Kind); kind != project.Kind {
					project.Kind = kind
					changed = true
				}
			}
		}
		if project.Kind == "" {
			project.Kind = domain.ProjectKindOther
			changed = true
		}
		if !changed {
			continue
		}
		if err := store.PutProject(ctx, project); err != nil {
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
	// Preserve a relative data_dir in the YAML: config.Load resolves it to an
	// absolute path for runtime use, and saving that absolute value would make
	// a portable git-backed instance machine-specific.
	rawDataDir := ""
	if raw, readErr := os.ReadFile(configPath); readErr == nil {
		var rawCfg struct {
			DataDir string `yaml:"data_dir"`
		}
		if yaml.Unmarshal(raw, &rawCfg) == nil {
			rawDataDir = strings.TrimSpace(rawCfg.DataDir)
		}
	}
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
	if rawDataDir != "" && !filepath.IsAbs(rawDataDir) {
		cfg.DataDir = rawDataDir
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
		names, confirmed := withoutYesFlag(rest)
		if len(names) != 1 {
			printSecretUsage(stderr)
			return 2
		}
		rest = names
		if !confirmed {
			// Removing a credential from the OS store cannot be undone.
			fmt.Fprintf(stderr, "brain secret remove: this permanently deletes %q from the OS secret store; pass --yes to confirm\n", strings.TrimSpace(rest[0]))
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

// withoutYesFlag drops --yes / -yes from args and reports whether it was set.
func withoutYesFlag(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	yes := false
	for _, arg := range args {
		if arg == "--yes" || arg == "-yes" {
			yes = true
			continue
		}
		out = append(out, arg)
	}
	return out, yes
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
  brain secret remove <name> --yes
                               Remove a secret from the OS store (irreversible)

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
	passwordStdin := fs.Bool("password-stdin", false, "read the password from one stdin line (or set BRAIN_PASSWORD)")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain login: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	var secret string
	if *passwordStdin {
		value, err := readSecretValue(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "brain login: %v\n", err)
			return 1
		}
		secret = value
	} else {
		secret = os.Getenv("BRAIN_PASSWORD")
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
	all := fs.Bool("all", false, "revoke every session of this account, not only this one")
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
	if *all {
		err = useCase.App.LogoutAll(context.Background())
	} else {
		err = useCase.App.Logout(context.Background())
	}
	if err != nil {
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
	if *all {
		fmt.Fprintln(stdout, "logged out: all sessions of this account were revoked (API tokens are unaffected)")
		return 0
	}
	fmt.Fprintln(stdout, "logged out")
	return 0
}

// runPassword changes the signed-in account's password. Both values are read
// from stdin, one line each, and are never printed.
func runPassword(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlagSet("password", stderr)
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain password: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	current, next, err := readPasswordPair(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "brain password: %v\n", err)
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "password", err)
	}
	if err := cfg.Validate(); err != nil {
		return reportError(stderr, "password", err)
	}
	if cfg.Mode != config.ModeServer {
		fmt.Fprintln(stderr, "brain password: server mode is required (set mode: server in brain.yaml)")
		return 1
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		fmt.Fprintln(stderr, "brain password: config.server_url is required")
		return 1
	}
	if staticServerTokenFromEnv() {
		fmt.Fprintln(stderr, "brain password: a static server token is set in the environment (BRAIN_TOKEN or BRAIN_SECRET_SERVER_TOKEN); a static API token cannot change a password, unset it and log in with a user account")
		return 1
	}
	useCase, err := buildRuntime(cfg)
	if err != nil {
		return reportError(stderr, "password", err)
	}
	defer func() { _ = useCase.Close() }()
	if err := useCase.App.ChangePassword(context.Background(), current, next); err != nil {
		if errors.Is(err, httpclient.ErrUnauthorized) {
			fmt.Fprintln(stderr, "brain password: your session has expired; run brain login again")
			return 1
		}
		return reportError(stderr, "password", err)
	}
	fmt.Fprintln(stdout, "password changed; other sessions of this account were signed out")
	return 0
}

// readPasswordPair reads the current and the new password from two stdin
// lines, using one reader so the second line is not lost to buffering.
func readPasswordPair(stdin io.Reader) (string, string, error) {
	if stdin == nil {
		return "", "", errors.New("stdin is not available; pipe the current and new password on two lines")
	}
	reader := bufio.NewReader(stdin)
	values := make([]string, 0, 2)
	for _, label := range []string{"current password", "new password"} {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", "", err
		}
		value := strings.TrimRight(line, "\r\n")
		if value == "" {
			return "", "", fmt.Errorf("the %s is required on stdin", label)
		}
		values = append(values, value)
	}
	return values[0], values[1], nil
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
	if source := installSource(); source != "" && !*check {
		// The package manager owns these files; replacing them would leave
		// it with a binary it did not install.
		fmt.Fprintf(stderr, "brain update: this brain was installed via %s; update it with: %s\n", source, managerUpdateCommand(source))
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
	case "serve", "init", "help", "-h", "--help", "version", "-v", "--version", "update", "event", "hooks", "config", "agents", "mcp", "session", "completion":
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
	hint := "brain update --yes"
	if source := installSource(); source != "" {
		hint = managerUpdateCommand(source)
	}
	fmt.Fprintf(stderr, "brain %s is available; run: %s\n", latest, hint)
}

// configPathFromArgs finds --config before flag parsing so the daily check uses
// the same configuration as the command that follows.
func configPathFromArgs(args []string) string {
	path := defaultConfigPath()
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
