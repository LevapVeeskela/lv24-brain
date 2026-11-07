//go:build windows

package update

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const runningImageEnv = "BRAIN_UPDATE_TEST_RUNNING_IMAGE"

// TestRunningImageHelper is the process image held open by
// TestApplyReplacesRunningWindowsImage; it does nothing in a normal run.
func TestRunningImageHelper(t *testing.T) {
	if os.Getenv(runningImageEnv) == "" {
		t.Skip("helper process only")
	}
	time.Sleep(30 * time.Second)
}

func TestApplyReplacesRunningWindowsImage(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "brain.exe")
	copyTestBinary(t, target)
	process := exec.Command(target, "-test.run=^TestRunningImageHelper$")
	process.Env = append(os.Environ(), runningImageEnv+"=1")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	time.Sleep(300 * time.Millisecond)

	artifact := filepath.Join(dir, "artifact")
	if err := os.WriteFile(artifact, []byte("new-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	updateDir := t.TempDir()
	for _, name := range []string{"staged-0.1.0-brain.exe", "staged-0.1.1-brain.exe"} {
		if err := os.WriteFile(filepath.Join(updateDir, name), []byte("old download"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(updateDir, "staged-0.1.2-brain.exe")
	if err := os.WriteFile(keep, []byte("new-binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	updater := testUpdater(t, "http://127.0.0.1:1", func(opts *Options) {
		opts.ExecutablePath = target
		opts.UpdateDir = updateDir
	})

	if err := updater.Apply(context.Background(), keep); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	assertFileContent(t, target, "new-binary")
	aside, _ := filepath.Glob(filepath.Join(dir, ".brain.exe-old-*"))
	if len(aside) != 1 {
		t.Fatalf("running image not moved aside: %v", aside)
	}
	if staged, _ := filepath.Glob(filepath.Join(updateDir, "staged-*")); len(staged) != 1 || !strings.HasSuffix(staged[0], "0.1.2-brain.exe") {
		t.Fatalf("staged downloads after apply = %v", staged)
	}

	// Once the old image exits, the next run removes the aside copy.
	_ = process.Process.Kill()
	_ = process.Wait()
	time.Sleep(200 * time.Millisecond)
	removeLeftovers(target, time.Now())
	if left, _ := filepath.Glob(filepath.Join(dir, ".brain.exe-old-*")); len(left) != 0 {
		t.Fatalf("aside copy not removed: %v", left)
	}
}

func copyTestBinary(t *testing.T, dst string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
