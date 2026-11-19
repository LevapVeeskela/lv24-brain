//go:build !windows

// Package winproc is a no-op outside Windows.
package winproc

import "os/exec"

// Hide does nothing: only Windows allocates a console window for a child.
func Hide(cmd *exec.Cmd) {}
