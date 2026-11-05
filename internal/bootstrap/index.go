package bootstrap

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// ResetIndex deletes the local SQLite index files and returns a runtime with a
// fresh index. Other data files are untouched.
func ResetIndex(cfg config.BrainConfig) (*Runtime, error) {
	if cfg.Mode == config.ModeServer {
		return nil, fmt.Errorf("%w: index is local-only in server mode", domain.ErrUnsupported)
	}
	dataDir := strings.TrimSpace(cfg.DataDir)
	if dataDir == "" {
		return nil, domain.NewValidationError("config.data_dir", "is required to reset the local index")
	}
	if info, err := os.Stat(filepath.Join(dataDir, "projects")); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: data directory is not initialized; run brain init", domain.ErrValidation)
	}
	path := filepath.Join(dataDir, filepath.FromSlash(indexRelativePath))
	for _, candidate := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("reset index: remove %s: %w", candidate, err)
		}
	}
	return Build(cfg)
}
