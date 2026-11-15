// Package config loads and validates the brain.yaml instance configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"gopkg.in/yaml.v3"
)

const (
	// FileName is the default instance configuration file name.
	FileName = "brain.yaml"

	ModeLocal  = "local"
	ModeGit    = "git"
	ModeServer = "server"

	ChannelStable = "stable"
	ChannelBeta   = "beta"

	// CurrentSchemaVersion is the newest data schema this engine understands.
	CurrentSchemaVersion = 1
)

var engineVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.\-+]+)?$`)

// BrainConfig is the contents of brain.yaml.
type BrainConfig struct {
	Mode             string         `yaml:"mode"`
	DataDir          string         `yaml:"data_dir,omitempty"`
	ServerURL        string         `yaml:"server_url,omitempty"`
	User             string         `yaml:"user,omitempty"`
	SchemaVersion    int            `yaml:"schema_version"`
	MinEngineVersion string         `yaml:"min_engine_version,omitempty"`
	UpdateChannel    string         `yaml:"update_channel"`
	Security         SecurityConfig `yaml:"security,omitempty"`
	// RetiredPaths lists retired checkouts of this workspace. The engine ships
	// no defaults: selection redirects to ReplacedBy and registration is
	// refused, but nothing on disk is touched.
	RetiredPaths []RetiredPath `yaml:"retired_paths,omitempty"`
}

// SecurityConfig controls how untrusted writes are treated.
type SecurityConfig struct {
	// QuarantineAgent makes agent writes through MCP unreviewed, so their text
	// stays out of context until `brain record review` or brain_review_record
	// clears it. The default is false: an agent write is reviewed immediately.
	QuarantineAgent bool `yaml:"quarantine_agent,omitempty"`
}

// RetiredPath is a checkout path that was retired in favor of another project.
type RetiredPath struct {
	// Path is the retired directory; selectors under it are redirected.
	Path string `yaml:"path"`
	// ReplacedBy is the project id or alias that now owns the work.
	ReplacedBy string `yaml:"replaced_by"`
}

// EffectiveRetiredPaths returns the retired checkouts configured in
// brain.yaml. There are no built-in paths: a machine-specific checkout belongs
// to the workspace configuration, not to the public engine.
func (c BrainConfig) EffectiveRetiredPaths() []RetiredPath {
	return c.RetiredPaths
}

// Default returns a local-mode configuration for a new brain.
func Default() BrainConfig {
	return BrainConfig{
		Mode:          ModeLocal,
		SchemaVersion: CurrentSchemaVersion,
		UpdateChannel: ChannelStable,
	}
}

// Load reads brain.yaml from path and applies defaults for omitted fields.
func Load(path string) (BrainConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return BrainConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg BrainConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return BrainConfig{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.resolveDataDir(path); err != nil {
		return BrainConfig{}, err
	}
	return cfg, nil
}

// resolveDataDir makes a relative data_dir relative to the config file, not
// the caller's current working directory. The YAML remains portable.
func (c *BrainConfig) resolveDataDir(configPath string) error {
	if c.DataDir == "" || filepath.IsAbs(c.DataDir) {
		return nil
	}
	abs, err := filepath.Abs(filepath.Join(filepath.Dir(configPath), c.DataDir))
	if err != nil {
		return fmt.Errorf("resolve data_dir %q: %w", c.DataDir, err)
	}
	c.DataDir = abs
	return nil
}

// Save validates cfg and writes it to path atomically.
func Save(path string, cfg BrainConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create config directory %s: %w", dir, err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".brain.yaml-*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString("# lv24-brain instance configuration\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config %s: %w", path, err)
	}
	return nil
}

// Validate checks the configuration against the current engine contract.
func (c BrainConfig) Validate() error {
	switch c.Mode {
	case ModeLocal, ModeGit, ModeServer:
	default:
		return domain.NewValidationError("config.mode", fmt.Sprintf("must be one of %q, %q, %q", ModeLocal, ModeGit, ModeServer))
	}
	switch c.UpdateChannel {
	case ChannelStable, ChannelBeta:
	default:
		return domain.NewValidationError("config.update_channel", fmt.Sprintf("must be %q or %q", ChannelStable, ChannelBeta))
	}
	if c.SchemaVersion < 1 {
		return domain.NewValidationError("config.schema_version", "must be at least 1")
	}
	if c.SchemaVersion > CurrentSchemaVersion {
		return domain.NewValidationError(
			"config.schema_version",
			fmt.Sprintf("data schema %d requires a newer engine (this engine supports %d)", c.SchemaVersion, CurrentSchemaVersion),
		)
	}
	if c.MinEngineVersion != "" && !engineVersionPattern.MatchString(c.MinEngineVersion) {
		return domain.NewValidationError("config.min_engine_version", "must look like 1.2.3")
	}
	for _, entry := range c.RetiredPaths {
		if strings.TrimSpace(entry.Path) == "" {
			return domain.NewValidationError("config.retired_paths", "each entry needs a path")
		}
		if strings.TrimSpace(entry.ReplacedBy) == "" {
			return domain.NewValidationError("config.retired_paths", "each entry needs replaced_by (a project id or alias)")
		}
	}
	if c.Mode == ModeLocal || c.Mode == ModeGit {
		if c.DataDir == "" {
			return domain.NewValidationError("config.data_dir", fmt.Sprintf("is required in %s mode", c.Mode))
		}
	}
	if c.Mode == ModeServer {
		if c.ServerURL == "" {
			return domain.NewValidationError("config.server_url", "is required in server mode")
		}
		if err := validateServerURL(c.ServerURL); err != nil {
			return err
		}
	}
	return nil
}

// IsNotExist reports whether err means the configuration file is absent.
func IsNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

func (c *BrainConfig) applyDefaults() {
	if c.SchemaVersion == 0 {
		c.SchemaVersion = CurrentSchemaVersion
	}
	if c.UpdateChannel == "" {
		c.UpdateChannel = ChannelStable
	}
}
