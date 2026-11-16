package graphalgo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// ValidateIndexPath checks the base index file of a graph diff: it must be an
// absolute path to an existing regular file, it must not be a symbolic link,
// and it must not be the index being queried. Both adapters call it before
// they open a second store, so a diff cannot read a directory, a link target
// outside the expected location, or compare a store with itself.
func ValidateIndexPath(headPath, raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", domain.NewValidationError("graph.base_index", "is required: name the index file of the base project")
	}
	if !filepath.IsAbs(path) {
		return "", domain.NewValidationError("graph.base_index", "must be an absolute path to a base index file")
	}
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil {
		return "", domain.NewValidationError("graph.base_index", fmt.Sprintf("cannot read %s: %v", path, err))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", domain.NewValidationError("graph.base_index", "must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return "", domain.NewValidationError("graph.base_index", fmt.Sprintf("%s is not a regular index file", path))
	}
	if filepath.Clean(headPath) == path {
		return "", domain.NewValidationError("graph.base_index", "must differ from the index being queried")
	}
	return path, nil
}
