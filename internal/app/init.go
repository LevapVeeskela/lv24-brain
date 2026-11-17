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

// gitignoreContent excludes machine-local state and, crucially, lock and
// temporary files. A committed "<file>.lock" would make every other machine
// wait for it after a clone, and a committed ".tmp-*" is a partial write.
const gitignoreContent = `# LV24 Brain local data exclusions
secrets/
index/
backups/
/mail/
/github/
/gitlab/
/merges/
connectors/.locks/
proposals/
agent/
*.db
*.sqlite
*.sqlite3
.tmp-*
*.lock
**/.lock
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
	return ensureIgnorePatterns(filepath.Join(dataDir, ".gitignore"), gitignoreContent)
}

// ensureIgnorePatterns creates the ignore file when missing and appends any
// managed pattern it is missing. That upgrades an existing data repository,
// whose file was written by an older engine, without touching the user's own
// lines: lock and temp files must never be published on any machine.
func ensureIgnorePatterns(path, content string) error {
	return ensureManagedLines(path, content, "# Added by brain init: locks and temporary files are never published")
}

// ensureManagedLines writes content when path is missing and otherwise appends
// only the managed non-comment lines the file lacks. It never rewrites or
// deletes a user line, so it can upgrade a file written by an older engine.
func ensureManagedLines(path, content, header string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("init: read %s: %w", path, err)
	}
	if os.IsNotExist(err) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("init: write %s: %w", path, err)
		}
		return nil
	}
	existing := string(raw)
	missing := missingManagedLines(content, existing)
	if len(missing) == 0 {
		return nil
	}
	if !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	existing += "\n" + header + "\n" + strings.Join(missing, "\n") + "\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		return fmt.Errorf("init: write %s: %w", path, err)
	}
	return nil
}

func missingManagedLines(content, existing string) []string {
	var missing []string
	for _, line := range strings.Split(content, "\n") {
		pattern := strings.TrimSpace(line)
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}
		if !hasIgnoreLine(existing, pattern) {
			missing = append(missing, pattern)
		}
	}
	return missing
}

func hasIgnoreLine(content, pattern string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == pattern {
			return true
		}
	}
	return false
}

// missingGitattributesLines reports the managed driver lines the data
// repository does not carry yet. A missing .gitattributes reports every line.
func missingGitattributesLines(dataDir string) ([]string, error) {
	path := filepath.Join(dataDir, ".gitattributes")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return missingManagedLines(gitattributesContent, ""), nil
		}
		return nil, fmt.Errorf("init: read %s: %w", path, err)
	}
	return missingManagedLines(gitattributesContent, string(raw)), nil
}

// gitattributesContent makes git keep every line of append-only journals that
// several machines of one user write concurrently (journal/<user>.jsonl), and
// routes the single mutable files through the deterministic merge drivers.
// A clone without the drivers registered falls back to a normal conflict,
// which is safe; brain init and brain doctor --fix register them.
const gitattributesContent = `# Normalize text to LF in the repository on every platform. Without this, a
# Windows clone with core.autocrlf=true writes CRLF into the work tree and
# hashers/diffs of work-tree bytes disagree across machines.
* text=auto eol=lf

# Binary artifacts never get text normalization or line-based merges.
*.db binary
*.sqlite binary
*.zip binary
*.png binary
*.jpg binary
*.jpeg binary
*.gif binary
*.pdf binary

# Journals are append-only JSON Lines written by several machines; union
# merge keeps every line from both sides instead of stopping on a conflict.
journal/*.jsonl merge=union

# Single mutable files are merged field by field, not line by line.
projects/*/state.md merge=lv24-state
registry.json merge=lv24-registry
projects/*/notes/*.md merge=lv24-notes

# Shared raw-zone items are content-addressed and immutable: normalize the
# host observation time and keep both sides if they genuinely diverge.
connectors/*/items/*.md merge=lv24-items

# Checkpoints hold host-specific cursors; they are gitignored and must never be
# merged or end up in a conflict even if an older checkout tracked them.
connectors/*/checkpoints/*.json -merge -text
connectors/*/*.lock -merge -text
`

func ensureGitattributes(dataDir string) error {
	return ensureManagedLines(
		filepath.Join(dataDir, ".gitattributes"),
		gitattributesContent,
		"# Added by brain: deterministic merge drivers for concurrent edits",
	)
}
