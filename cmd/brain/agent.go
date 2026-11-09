package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/daemon"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

func printAgentUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  brain agent [--config <path>] [--parent-pid <pid>]
  brain agent status [--config <path>] [--json]
  brain agent stop [--config <path>]

Without a subcommand, agent runs in the foreground. A parent PID makes the
agent stop when its supervisor exits. The local control API is not enabled yet.
`)
}

// runAgent is the standalone CLI surface; main.go dispatch and completion
// registration are intentionally left to the orchestrator.
func runAgent(args []string, stdout, stderr io.Writer) int {
	command := "run"
	if len(args) > 0 && (args[0] == "status" || args[0] == "stop" || args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		command, args = args[0], args[1:]
	}
	if command == "help" || command == "--help" || command == "-h" {
		printAgentUsage(stdout)
		return 0
	}
	fs := newFlagSet("agent "+command, stderr)
	cfgPath := configFlag(fs)
	var asJSON bool
	var parentPID int
	if command == "status" {
		fs.BoolVar(&asJSON, "json", false, "print JSON")
	}
	if command == "run" {
		fs.IntVar(&parentPID, "parent-pid", 0, "exit when this parent process exits")
	}
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 0 {
		fmt.Fprintf(stderr, "brain agent %s: unexpected argument %q\n", command, positional[0])
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "agent "+command, err)
	}
	if cfg.Mode == config.ModeServer {
		fmt.Fprintln(stderr, "brain agent: requires local or git mode")
		return 2
	}
	ctx := context.Background()
	switch command {
	case "status":
		status, err := daemon.ReadStatus(ctx, cfg.DataDir)
		if err != nil {
			return reportError(stderr, "agent status", err)
		}
		if asJSON {
			return writeJSONReport(stdout, stderr, "agent status", status)
		}
		if !status.Running {
			fmt.Fprintln(stdout, "agent not running")
			return 0
		}
		fmt.Fprintf(stdout, "agent running pid=%d port=%d version=%s started_at=%s\n", status.PID, status.Port, status.Version, status.StartedAt.UTC().Format(time.RFC3339))
		return 0
	case "stop":
		stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := daemon.StopProcess(stopCtx, cfg.DataDir); err != nil {
			return reportError(stderr, "agent stop", err)
		}
		fmt.Fprintln(stdout, "agent stop requested")
		return 0
	case "run":
		return runAgentForeground(cfg, parentPID, stdout, stderr)
	default:
		printAgentUsage(stderr)
		return 2
	}
}

func runAgentForeground(cfg config.BrainConfig, parentPID int, stdout, stderr io.Writer) int {
	ingestion, err := buildIngestion(cfg, bootstrap.Options{})
	if err != nil {
		return reportError(stderr, "agent", err)
	}
	defer ingestion.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	engine, err := daemon.New(daemon.Options{
		DataDir: cfg.DataDir, Version: version.Version, ParentPID: parentPID,
		Connections: ingestion.Ingestion.Connections,
		Runner:      ingestionRunner{host: ingestion.Ingestion},
	})
	if err != nil {
		return reportError(stderr, "agent", err)
	}
	err = engine.Run(ctx)
	if err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			fmt.Fprintln(stderr, "brain agent: another agent is already running")
			return 1
		}
		return reportError(stderr, "agent", err)
	}
	return 0
}

type ingestionRunner struct{ host *app.Ingestion }

func (r ingestionRunner) RunConnection(ctx context.Context, opts app.RunConnectionOptions) (app.RunConnectionResult, error) {
	return r.host.RunConnection(ctx, opts)
}
