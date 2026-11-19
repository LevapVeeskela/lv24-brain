//go:build windows

// Package winproc configures child processes that must not show a console
// window. A console program started by a GUI process allocates a visible
// console unless CREATE_NO_WINDOW is set; the desktop app and the scheduled
// tasks already set it, and command-line helpers use this package.
package winproc

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// Hide marks cmd to start without a console window. Call it before Start/Run.
// Existing attributes are preserved.
func Hide(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	attr := cmd.SysProcAttr
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.HideWindow = true
	attr.CreationFlags |= createNoWindow
	cmd.SysProcAttr = attr
}
