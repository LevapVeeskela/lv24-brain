//go:build windows

package update

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const creationNoWindow = 0x08000000

// scheduleReplace writes a detached cmd helper that retries the swap until the
// running image is released, then deletes itself.
func scheduleReplace(target, source, updateDir string, now func() time.Time) error {
	if err := os.MkdirAll(updateDir, 0o755); err != nil {
		return fmt.Errorf("update: create helper directory: %w", err)
	}
	script := filepath.Join(updateDir, fmt.Sprintf("apply-%d.cmd", now().UnixNano()))
	if err := os.WriteFile(script, []byte(swapScript), 0o600); err != nil {
		return fmt.Errorf("update: write replacement helper: %w", err)
	}
	command := exec.Command("cmd.exe", "/c", "start", "", "/b", script, target, source)
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: creationNoWindow}
	if err := command.Start(); err != nil {
		_ = os.Remove(script)
		return fmt.Errorf("update: start replacement helper: %w", err)
	}
	go func() { _ = command.Wait() }()
	return nil
}
