//go:build windows

// Command braintask runs a scheduled command with no console window.
//
// Task Scheduler starts an interactive task with a console when the target is a
// console program or a batch file, so a daily or 30-minute job flashes a cmd
// window in the user's session. Built with -ldflags "-H windowsgui" and
// CREATE_NO_WINDOW, this launcher never creates a console; the child inherits
// no standard handles, so scheduled scripts keep writing to their own log
// files. It waits for the child and returns its exit code, so Task Scheduler
// still records success or failure.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const createNoWindow = 0x08000000

// runStdio runs a stdio server behind a GUI-subsystem launcher: the parent
// sees a windowless process, while the child keeps the inherited pipes. An MCP
// host that would otherwise open a console can start a console server
// invisibly through it.
func runStdio(name string, args []string) int {
	if lower := strings.ToLower(name); strings.HasSuffix(lower, ".bat") || strings.HasSuffix(lower, ".cmd") {
		args = append([]string{"/d", "/c", name}, args...)
		name = "cmd.exe"
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		return 1
	}
	return 0
}

func main() {
	if len(os.Args) >= 3 && os.Args[1] == "--stdio" {
		os.Exit(runStdio(os.Args[2], os.Args[3:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: brain-task <command> [args...]")
		os.Exit(2)
	}
	name := os.Args[1]
	args := os.Args[2:]
	if name == "" {
		fmt.Fprintln(os.Stderr, "brain-task: empty command")
		os.Exit(2)
	}
	if lower := strings.ToLower(name); strings.HasSuffix(lower, ".bat") || strings.HasSuffix(lower, ".cmd") {
		args = append([]string{"/d", "/c", name}, args...)
		name = "cmd.exe"
	}
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "brain-task:", err)
		os.Exit(1)
	}
}
