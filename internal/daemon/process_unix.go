//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// trimDeletedSuffix normalizes the Linux /proc/<pid>/exe target after a
// self-update replaced the running binary: the kernel appends " (deleted)".
func trimDeletedSuffix(path string) string {
	return strings.TrimSuffix(path, " (deleted)")
}

func validateBrainProcess(pid int) error {
	current, err := os.Executable()
	if err != nil {
		return err
	}
	current = trimDeletedSuffix(current)
	if resolved, err := filepath.EvalSymlinks(current); err == nil {
		current = resolved
	}
	// Linux exposes the exact executable path. The agent lock owner file is
	// the authoritative ownership check; this is an additional guard.
	if exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe")); err == nil {
		exe = trimDeletedSuffix(exe)
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if exe == "" || filepath.Clean(exe) != filepath.Clean(current) {
			return fmt.Errorf("process executable does not match this brain binary")
		}
		return nil
	}
	// macOS/BSD `ps -o comm=` may return either a full path or only the
	// executable name. Compare the full path when available; otherwise accept
	// the same basename (the lock owner check still has to match the token).
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return err
	}
	actual := strings.TrimSpace(string(out))
	if actual == "" {
		return fmt.Errorf("process executable does not match this brain binary")
	}
	if filepath.IsAbs(actual) || strings.ContainsRune(actual, os.PathSeparator) {
		actual, _ = filepath.EvalSymlinks(actual)
		if filepath.Clean(actual) != filepath.Clean(current) {
			return fmt.Errorf("process executable does not match this brain binary")
		}
		return nil
	}
	if filepath.Base(actual) != filepath.Base(current) {
		return fmt.Errorf("process executable does not match this brain binary")
	}
	return nil
}
