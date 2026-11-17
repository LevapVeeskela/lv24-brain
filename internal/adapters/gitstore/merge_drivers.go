package gitstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// MergeDriver describes one managed git merge driver.
type MergeDriver struct {
	// Name is the suffix of the merge.<name> configuration key and the value
	// used by .gitattributes (merge=lv24-state and so on).
	Name string
	// Description is stored as merge.<name>.name.
	Description string
}

// MergeDrivers lists the drivers a brain data repository uses.
func MergeDrivers() []MergeDriver {
	return []MergeDriver{
		{Name: "lv24-state", Description: "lv24 brain state field merge"},
		{Name: "lv24-notes", Description: "lv24 brain note section merge"},
		{Name: "lv24-registry", Description: "lv24 brain registry keyed merge"},
		{Name: "lv24-items", Description: "lv24 brain raw-zone item union merge"},
	}
}

// MissingMergeDrivers reports managed drivers that have no
// merge.<name>.driver in the local repository configuration. A data directory
// that is not the root of a git work tree reports domain.ErrUnsupported: the
// drivers are registered per clone, so there is nowhere to store them yet.
func (p *Provider) MissingMergeDrivers(ctx context.Context) ([]string, error) {
	root, err := p.workTreeRoot(ctx)
	if err != nil {
		return nil, err
	}
	if root == "" {
		return nil, fmt.Errorf("gitstore: data directory is not a git work tree: %w", domain.ErrUnsupported)
	}
	var missing []string
	for _, driver := range MergeDrivers() {
		value, err := p.run(ctx, "config", "--local", "--get", "merge."+driver.Name+".driver")
		if err != nil {
			var commandErr *CommandError
			if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
				missing = append(missing, driver.Name)
				continue
			}
			return nil, err
		}
		if strings.TrimSpace(value) == "" {
			missing = append(missing, driver.Name)
		}
	}
	return missing, nil
}

// RegisterMergeDrivers writes merge.<name>.name and merge.<name>.driver for
// every managed driver, pointing at executable. It is idempotent: an already
// correct value is not rewritten, and another checkout's absolute path is
// replaced with this executable. A data directory that is not the root of a
// git work tree is left untouched and reports no change.
func (p *Provider) RegisterMergeDrivers(ctx context.Context, executable string) (bool, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return false, domain.NewValidationError("gitstore.executable", "is required")
	}
	if strings.ContainsAny(executable, "\"\n\r") {
		return false, domain.NewValidationError("gitstore.executable", "must not contain a quote or a line break")
	}
	root, err := p.workTreeRoot(ctx)
	if err != nil {
		return false, err
	}
	if root == "" {
		return false, nil
	}
	if !samePath(root, p.dir) {
		// The data directory is nested in a larger repository. Configuring that
		// repository would apply brain drivers to unrelated checkouts.
		return false, nil
	}
	command := quoteExecutable(executable) + " merge-driver"
	changed := false
	for _, driver := range MergeDrivers() {
		prefix := "merge." + driver.Name + "."
		for key, value := range map[string]string{
			prefix + "name":   driver.Description,
			prefix + "driver": command + " " + driverCommand(driver.Name),
		} {
			changedNow, err := p.setConfigIfChanged(ctx, key, value)
			if err != nil {
				return changed, err
			}
			changed = changed || changedNow
		}
	}
	return changed, nil
}

func driverCommand(name string) string {
	switch name {
	case "lv24-state":
		return "state %O %A %B %L %P"
	case "lv24-notes":
		return "notes %O %A %B %L %P"
	case "lv24-registry":
		return "registry %O %A %B %L %P"
	case "lv24-items":
		return "items %O %A %B %L %P"
	default:
		return ""
	}
}

func (p *Provider) setConfigIfChanged(ctx context.Context, key, value string) (bool, error) {
	current, err := p.run(ctx, "config", "--local", "--get", key)
	if err == nil && strings.TrimRight(current, "\n") == value {
		return false, nil
	}
	if err != nil {
		var commandErr *CommandError
		if !errors.As(err, &commandErr) || commandErr.ExitCode != 1 {
			return false, err
		}
	}
	if _, err := p.run(ctx, "config", "--local", key, value); err != nil {
		return false, err
	}
	return true, nil
}

// workTreeRoot returns the root of the repository that contains dir, or an
// empty string when dir is not inside a git work tree. A bare repository has no
// work tree and also yields an empty string.
func (p *Provider) workTreeRoot(ctx context.Context) (string, error) {
	out, err := p.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		var commandErr *CommandError
		if errors.As(err, &commandErr) {
			return "", nil
		}
		return "", err
	}
	root := strings.TrimSpace(out)
	if root == "" {
		return "", nil
	}
	return filepath.Clean(filepath.FromSlash(root)), nil
}

// quoteExecutable makes one executable path safe for the git driver command.
// Git for Windows executes a driver through a shell, so the path is quoted and
// uses forward slashes to keep backslashes out of shell quoting.
func quoteExecutable(executable string) string {
	path := filepath.ToSlash(executable)
	return `"` + path + `"`
}

func samePath(left, right string) bool {
	leftPath, leftErr := filepath.Abs(left)
	rightPath, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftPath = filepath.Clean(leftPath)
	rightPath = filepath.Clean(rightPath)
	if leftResolved, err := filepath.EvalSymlinks(leftPath); err == nil {
		leftPath = leftResolved
	}
	if rightResolved, err := filepath.EvalSymlinks(rightPath); err == nil {
		rightPath = rightResolved
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(leftPath, rightPath)
	}
	return leftPath == rightPath
}
