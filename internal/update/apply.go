package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Apply replaces the running engine binary with artifactPath. It preserves the
// current binary as <executable>.previous for Rollback. On Windows the swap is
// delegated to a detached helper when the target is the running executable,
// because a running image cannot be replaced in place.
func (u *Updater) Apply(ctx context.Context, artifactPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	artifactPath = strings.TrimSpace(artifactPath)
	if artifactPath == "" {
		return errors.New("update: artifact path is required")
	}
	info, err := os.Stat(artifactPath)
	if err != nil {
		return fmt.Errorf("update: inspect artifact: %w", err)
	}
	if info.IsDir() || info.Size() == 0 {
		return errors.New("update: artifact must be a non-empty file")
	}
	target, err := u.executablePath()
	if err != nil {
		return err
	}
	if samePath(artifactPath, target) {
		return errors.New("update: artifact is the running binary")
	}

	staged, err := stageBinary(artifactPath, filepath.Dir(target), filepath.Base(target))
	if err != nil {
		return err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			_ = os.Remove(staged)
		}
	}()

	if err := backupCurrent(target); err != nil {
		return err
	}
	if u.deferToHelper(target) {
		if err := scheduleReplace(target, staged, u.updateDir, u.now); err != nil {
			return err
		}
		handedOff = true
		return nil
	}
	if err := replaceFile(staged, target); err != nil {
		if u.goos == "windows" && runtime.GOOS == "windows" {
			if helperErr := scheduleReplace(target, staged, u.updateDir, u.now); helperErr == nil {
				handedOff = true
				return nil
			}
		}
		return fmt.Errorf("update: replace binary: %w", err)
	}
	handedOff = true
	return nil
}

// Rollback restores <executable>.previous over the current binary. The
// previous copy is retained so a failed rollback can be retried.
func (u *Updater) Rollback(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target, err := u.executablePath()
	if err != nil {
		return err
	}
	previous := target + ".previous"
	info, err := os.Stat(previous)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoPrevious
	}
	if err != nil {
		return fmt.Errorf("update: inspect previous binary: %w", err)
	}
	if info.IsDir() || info.Size() == 0 {
		return fmt.Errorf("%w: previous binary is not a file", ErrNoPrevious)
	}
	staged, err := copyFileToTemp(previous, filepath.Dir(target), "."+filepath.Base(target)+"-rollback-*")
	if err != nil {
		return err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			_ = os.Remove(staged)
		}
	}()
	if u.deferToHelper(target) {
		if err := scheduleReplace(target, staged, u.updateDir, u.now); err != nil {
			return err
		}
		handedOff = true
		return nil
	}
	if err := replaceFile(staged, target); err != nil {
		if u.goos == "windows" && runtime.GOOS == "windows" {
			if helperErr := scheduleReplace(target, staged, u.updateDir, u.now); helperErr == nil {
				handedOff = true
				return nil
			}
		}
		return fmt.Errorf("update: restore previous binary: %w", err)
	}
	handedOff = true
	return nil
}

func (u *Updater) executablePath() (string, error) {
	path := u.execPath
	if path == "" {
		resolved, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("update: locate running executable: %w", err)
		}
		path = resolved
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("update: resolve executable path: %w", err)
	}
	return absolute, nil
}

func (u *Updater) deferToHelper(target string) bool {
	if runtime.GOOS != "windows" || u.goos != "windows" {
		return false
	}
	return isCurrentExecutable(target)
}

func isCurrentExecutable(target string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		return false
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return false
	}
	return os.SameFile(targetInfo, selfInfo)
}

func backupCurrent(target string) error {
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("update: inspect current binary: %w", err)
	}
	if info.IsDir() {
		return errors.New("update: current binary path is a directory")
	}
	tmp, err := copyFileToTemp(target, filepath.Dir(target), "."+filepath.Base(target)+"-previous-*")
	if err != nil {
		return err
	}
	previous := target + ".previous"
	if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmp)
		return fmt.Errorf("update: replace previous binary: %w", err)
	}
	if err := os.Rename(tmp, previous); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("update: keep previous binary: %w", err)
	}
	return nil
}

func copyFileToTemp(sourcePath, dir, pattern string) (string, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", fmt.Errorf("update: open %s: %w", sourcePath, err)
	}
	defer func() { _ = source.Close() }()
	return copyToTemp(source, dir, pattern, 0o755)
}

func replaceFile(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return err
	}
	return nil
}

func samePath(a, b string) bool {
	aAbs, errA := filepath.Abs(a)
	bAbs, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return filepath.Clean(aAbs) == filepath.Clean(bAbs)
}
