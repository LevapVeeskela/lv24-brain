package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

// onboardReport is the stable machine-readable shape of `brain onboard`.
type onboardReport struct {
	Engine      string           `json:"engine"`
	Mode        string           `json:"mode"`
	DataDir     string           `json:"data_dir"`
	Config      string           `json:"config"`
	Checks      []string         `json:"checks"`
	Warnings    []string         `json:"warnings"`
	Provenance  *app.AdoptReport `json:"provenance,omitempty"`
	Sync        *app.SyncResult  `json:"sync,omitempty"`
	NextActions []string         `json:"next_actions"`
}

// runOnboard is the one-command setup check for a new machine. It registers
// the merge drivers, prints the health checks a machine needs before writing,
// and tells the operator how to start the periodic sync on this platform.
func runOnboard(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("onboard", stderr)
	asJSON := fs.Bool("json", false, "print a machine-readable report")
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "brain onboard: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	cfg, err := loadRequiredConfig(*cfgPath)
	if err != nil {
		return reportError(stderr, "onboard", err)
	}
	useCase, err := buildRuntime(cfg)
	if err != nil {
		return reportError(stderr, "onboard", err)
	}
	defer func() { _ = useCase.Close() }()

	ctx := context.Background()
	report := onboardReport{
		Engine:  version.Version,
		Mode:    string(cfg.Mode),
		DataDir: cfg.DataDir,
		Config:  *cfgPath,
	}
	doctor, err := useCase.App.Doctor(ctx, app.DoctorOptions{Fix: true, ConfigPath: *cfgPath})
	if err != nil {
		return reportError(stderr, "onboard", err)
	}
	for _, check := range doctor.Checks {
		line := check.Status + ": " + check.Name
		if strings.TrimSpace(check.Message) != "" {
			line += " (" + check.Message + ")"
		}
		report.Checks = append(report.Checks, line)
		if check.Status == "fail" {
			report.Warnings = append(report.Warnings, check.Name+": "+check.Message)
		}
	}

	code := 0
	adopt, adoptErr := useCase.App.AdoptLocalPaths(ctx, true)
	if adoptErr != nil {
		report.Warnings = append(report.Warnings, "provenance: "+adoptErr.Error())
	} else {
		report.Provenance = &adopt
		report.Checks = append(report.Checks, fmt.Sprintf(
			"ok: provenance (%d paths stamped for %s, %d already owned, %d not on this machine)",
			adopt.Stamped, adopt.Machine, adopt.Owned, adopt.Missing))
	}
	if cfg.Mode == config.ModeGit {
		result, syncErr := useCase.App.Sync(ctx, app.SyncOptions{StatusOnly: true})
		report.Sync = &result
		if syncErr != nil {
			return reportError(stderr, "onboard", syncErr)
		}
		if len(result.Conflicts) > 0 {
			code = 3
			report.Warnings = append(report.Warnings, "unresolved git conflicts: "+strings.Join(result.Conflicts, ", "))
		}
	}
	report.NextActions = onboardNextActions(cfg)

	if *asJSON {
		return writeJSONReport(stdout, stderr, "onboard", report)
	}
	fmt.Fprintf(stdout, "brain onboard: %s (%s mode)\n", report.Engine, report.Mode)
	fmt.Fprintf(stdout, "config: %s\n", report.Config)
	fmt.Fprintf(stdout, "data: %s\n", report.DataDir)
	for _, check := range report.Checks {
		fmt.Fprintln(stdout, check)
	}
	if report.Sync != nil {
		fmt.Fprintf(stdout, "sync: branch %s, ahead %d, behind %d, dirty %t\n",
			report.Sync.Branch, report.Sync.Ahead, report.Sync.Behind, report.Sync.Dirty)
		for _, conflict := range report.Sync.Conflicts {
			fmt.Fprintf(stdout, "conflict: %s\n", conflict)
		}
	}
	if len(report.Warnings) > 0 {
		fmt.Fprintln(stdout, "warnings:")
		for _, warning := range report.Warnings {
			fmt.Fprintf(stdout, "- %s\n", warning)
		}
	}
	fmt.Fprintln(stdout, "next:")
	for _, action := range report.NextActions {
		fmt.Fprintf(stdout, "- %s\n", action)
	}
	return code
}

// onboardNextActions names the exact background-sync command for the current
// platform. The command is deliberately printed, not installed: a service
// manager needs the owner's login context and should be reviewed first.
func onboardNextActions(cfg config.BrainConfig) []string {
	configPath := "~/brain-data/brain.yaml"
	if strings.TrimSpace(cfg.DataDir) != "" && cfg.DataDir != "." {
		configPath = cfg.DataDir + "/brain.yaml"
	}
	if strings.HasPrefix(cfg.DataDir, "/") {
		return []string{
			"brain sync --push --config " + configPath,
			"brain sync --watch --push --interval=5m --config " + configPath,
			"macOS: load a LaunchAgent that runs the watch command",
			"Linux: load a systemd user timer that runs the watch command",
		}
	}
	return []string{
		"brain sync --push --config " + configPath,
		"brain sync --watch --push --interval=5m --config " + configPath,
		"Windows: schedule `brain-task.exe <command>` (GUI-subsystem launcher) instead of a .bat directly, e.g. `brain-task.exe C:\\path\\to\\brain.exe sync --push`; a bare .bat or console program opens a cmd window in the interactive session",
	}
}
