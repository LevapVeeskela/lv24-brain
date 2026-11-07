package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const gitignoreContent = `# LV24 Brain local data exclusions
secrets/
index/
backups/
*.db
*.sqlite
*.sqlite3
.env
`

// Init creates the local data layout and writes brain.yaml. Server mode only
// records the server URL; its store adapter is selected by a later work package.
func (a *App) Init(ctx context.Context, opts InitOptions) (InitResult, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return InitResult{}, err
		}
	}
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = config.ModeLocal
	}
	switch mode {
	case config.ModeLocal, config.ModeGit, config.ModeServer:
	default:
		return InitResult{}, domain.NewValidationError("init.mode", fmt.Sprintf("must be %q, %q, or %q", config.ModeLocal, config.ModeGit, config.ModeServer))
	}

	requestedDataDir := strings.TrimSpace(opts.DataDir)
	configPath := strings.TrimSpace(opts.ConfigPath)
	if configPath == "" {
		seedDataDir := requestedDataDir
		if seedDataDir == "" {
			seedDataDir = defaultDataDir(mode)
		}
		if seedDataDir != "" {
			configPath = filepath.Join(seedDataDir, config.FileName)
		} else {
			configPath = config.FileName
		}
	}
	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return InitResult{}, fmt.Errorf("init: resolve config path %q: %w", configPath, err)
	}

	cfg := config.Default()
	if existing, loadErr := config.Load(absoluteConfigPath); loadErr == nil {
		cfg = existing
	} else if !config.IsNotExist(loadErr) {
		return InitResult{}, loadErr
	}

	dataDir := requestedDataDir
	if dataDir == "" {
		dataDir = strings.TrimSpace(cfg.DataDir)
	}
	if dataDir == "" {
		dataDir = defaultDataDir(mode)
	}
	if mode == config.ModeServer {
		dataDir = ""
	} else if dataDir == "" {
		return InitResult{}, domain.NewValidationError("config.data_dir", "is required; pass --data-dir")
	}
	if dataDir != "" {
		absoluteDataDir, absErr := filepath.Abs(dataDir)
		if absErr != nil {
			return InitResult{}, fmt.Errorf("init: resolve data directory %q: %w", dataDir, absErr)
		}
		dataDir = absoluteDataDir
	}

	cfg.Mode = mode
	cfg.DataDir = dataDir
	if serverURL := strings.TrimSpace(opts.ServerURL); serverURL != "" {
		cfg.ServerURL = serverURL
	}
	if mode != config.ModeServer {
		cfg.ServerURL = ""
	}
	if cfg.SchemaVersion < 1 {
		cfg.SchemaVersion = config.CurrentSchemaVersion
	}
	if cfg.UpdateChannel == "" {
		cfg.UpdateChannel = config.ChannelStable
	}
	if err := cfg.Validate(); err != nil {
		return InitResult{}, err
	}

	if mode == config.ModeLocal || mode == config.ModeGit {
		if err := os.MkdirAll(filepath.Join(dataDir, "projects"), 0o755); err != nil {
			return InitResult{}, fmt.Errorf("init: create data directory %s: %w", dataDir, err)
		}
		if mode == config.ModeGit {
			if err := ensureGitattributes(dataDir); err != nil {
				return InitResult{}, err
			}
			if err := ensureGitignore(dataDir); err != nil {
				return InitResult{}, err
			}
		}
	}
	if err := config.Save(absoluteConfigPath, cfg); err != nil {
		return InitResult{}, err
	}
	a.cfg = cfg
	return InitResult{ConfigPath: absoluteConfigPath, DataDir: dataDir, Mode: mode}, nil
}

func defaultDataDir(mode string) string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	if mode == config.ModeGit {
		return filepath.Join(home, "brain-data")
	}
	return filepath.Join(home, "brain")
}

func ensureGitignore(dataDir string) error {
	path := filepath.Join(dataDir, ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("init: inspect %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(gitignoreContent), 0o644); err != nil {
		return fmt.Errorf("init: write %s: %w", path, err)
	}
	return nil
}

// gitattributesContent makes git keep every line of append-only journals that
// several machines of one user write concurrently (journal/<user>.jsonl), so a
// sync never stops on a journal conflict.
const gitattributesContent = `# Journals are append-only JSON Lines written by several machines; union
# merge keeps every line from both sides instead of stopping on a conflict.
journal/*.jsonl merge=union
`

func ensureGitattributes(dataDir string) error {
	path := filepath.Join(dataDir, ".gitattributes")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("init: inspect %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(gitattributesContent), 0o644); err != nil {
		return fmt.Errorf("init: write %s: %w", path, err)
	}
	return nil
}
