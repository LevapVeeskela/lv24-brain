package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Apply replaces the running engine binary with artifactPath. It preserves the
// current binary as <executable>.previous for Rollback. On Windows a running
// image cannot be overwritten, so swap renames it aside first.
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

	removeLeftovers(target, u.now())
	staged, err := stageBinary(artifactPath, filepath.Dir(target), filepath.Base(target))
	if err != nil {
		return err
	}
	if err := backupCurrent(target); err != nil {
		_ = os.Remove(staged)
		return err
	}
	if err := u.swap(staged, target); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("update: replace binary: %w", err)
	}
	u.pruneStaged(artifactPath)
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
	removeLeftovers(target, u.now())
	staged, err := copyFileToTemp(previous, filepath.Dir(target), "."+filepath.Base(target)+"-rollback-*")
	if err != nil {
		return err
	}
	if err := u.swap(staged, target); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("update: restore previous binary: %w", err)
	}
	return nil
}

// leftoverAge protects files a concurrent apply may still be using.
const leftoverAge = time.Hour

// swap moves staged over target. A running Windows image cannot be
// overwritten but can be renamed, so the old file is moved aside to
// .<name>-old-<n> (deleted by a later run once no process uses it). If even
// the rename fails, a detached helper retries the swap after the process exits.
func (u *Updater) swap(staged, target string) error {
	err := replaceFile(staged, target)
	if err == nil || u.goos != "windows" || runtime.GOOS != "windows" {
		return err
	}
	aside := filepath.Join(filepath.Dir(target), fmt.Sprintf(".%s-old-%d", filepath.Base(target), u.now().UnixNano()))
	if renameErr := os.Rename(target, aside); renameErr != nil {
		if helperErr := scheduleReplace(target, staged, u.updateDir, u.now); helperErr != nil {
			return errors.Join(err, renameErr, helperErr)
		}
		return nil
	}
	if err := replaceFile(staged, target); err != nil {
		_ = os.Rename(aside, target)
		return err
	}
	return nil
}

// removeLeftovers deletes binaries moved aside by earlier swaps and staged
// copies of interrupted applies. Files still in use stay until a later run.
func removeLeftovers(target string, now time.Time) {
	dir, base := filepath.Dir(target), filepath.Base(target)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "."+base+"-old-") && !strings.HasPrefix(name, "."+base+"-new-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || (strings.HasPrefix(name, "."+base+"-new-") && now.Sub(info.ModTime()) < leftoverAge) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// pruneStaged removes downloaded release artifacts other than the one just
// applied, so the update directory does not grow with every release.
func (u *Updater) pruneStaged(keep string) {
	if u.updateDir == "" {
		return
	}
	entries, err := os.ReadDir(u.updateDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(u.updateDir, entry.Name())
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "staged-") || samePath(path, keep) {
			continue
		}
		_ = os.Remove(path)
	}
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
