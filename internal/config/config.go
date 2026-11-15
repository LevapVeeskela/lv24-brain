// Package config loads and validates the brain.yaml instance configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
	// Limits bounds one record write and protects curated paths from writes.
	// Omitted and zero values select the defaults; the effective values are
	// never below one, so a cap cannot be disabled by setting zero.
	Limits LimitsConfig `yaml:"limits,omitempty"`
	// Review sets the per-kind review cadence. An omitted block keeps the
	// documented defaults; nothing expires, a due record is only reported.
	Review ReviewConfig `yaml:"review,omitempty"`
	// RetiredPaths lists retired checkouts of this workspace. The engine ships
	// no defaults: selection redirects to ReplacedBy and registration is
	// refused, but nothing on disk is touched.
	RetiredPaths []RetiredPath `yaml:"retired_paths,omitempty"`
}

// Default write-time limits. They are generous enough that a normal note,
// decision or handoff never reaches them; they exist to stop an agent from
// writing an unbounded record or a runaway metadata list.
const (
	// DefaultMaxBodyBytes is the 1 MiB body budget of one record.
	DefaultMaxBodyBytes = 1 << 20
	// DefaultMaxTitleChars is the title budget in Unicode code points.
	DefaultMaxTitleChars = 200
	// DefaultMaxMetadataBytes is the 16 KiB budget of the tag, evidence,
	// alternatives and consequences lists.
	DefaultMaxMetadataBytes = 16 << 10
	// MinLimit is the smallest effective value of any size limit. Zero in
	// brain.yaml means "use the default", never "no limit".
	MinLimit = 1
)

// LimitsConfig is the `limits` block of brain.yaml. It is additive: a
// configuration without the block keeps the defaults.
type LimitsConfig struct {
	// MaxBodyBytes caps the free text of one record. Zero selects
	// DefaultMaxBodyBytes.
	MaxBodyBytes int `yaml:"max_body_bytes,omitempty"`
	// MaxTitleChars caps a record title in Unicode code points. Zero selects
	// DefaultMaxTitleChars. A handoff has no title field.
	MaxTitleChars int `yaml:"max_title_chars,omitempty"`
	// MaxMetadataBytes caps the combined size of the tag, evidence,
	// alternatives and consequences lists. Zero selects
	// DefaultMaxMetadataBytes.
	MaxMetadataBytes int `yaml:"max_metadata_bytes,omitempty"`
	// ProtectedProjects refuses record writes and imports into these project
	// ids. The default is empty; a workspace that owns a shared `global` or
	// `templates` project usually lists it here.
	ProtectedProjects []string `yaml:"protected_projects,omitempty"`
	// ProtectedGlobs refuses record writes whose data-directory path matches
	// one of these slash-separated globs. A pattern also protects every
	// directory below a path it matches, so "projects/global/**" covers the
	// whole project tree.
	ProtectedGlobs []string `yaml:"protected_globs,omitempty"`
}

// IsZero lets yaml omit a limits block that was never configured.
func (l LimitsConfig) IsZero() bool {
	return l.MaxBodyBytes == 0 && l.MaxTitleChars == 0 && l.MaxMetadataBytes == 0 &&
		len(l.ProtectedProjects) == 0 && len(l.ProtectedGlobs) == 0
}

// Effective resolves omitted and non-positive sizes to their defaults and
// normalizes the protected lists. The result is what the write path enforces.
func (l LimitsConfig) Effective() LimitsConfig {
	out := LimitsConfig{
		MaxBodyBytes:      l.MaxBodyBytes,
		MaxTitleChars:     l.MaxTitleChars,
		MaxMetadataBytes:  l.MaxMetadataBytes,
		ProtectedProjects: normalizePatterns(l.ProtectedProjects),
		ProtectedGlobs:    normalizePatterns(l.ProtectedGlobs),
	}
	if out.MaxBodyBytes < MinLimit {
		out.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if out.MaxTitleChars < MinLimit {
		out.MaxTitleChars = DefaultMaxTitleChars
	}
	if out.MaxMetadataBytes < MinLimit {
		out.MaxMetadataBytes = DefaultMaxMetadataBytes
	}
	return out
}

// EffectiveLimits returns the limits the write path enforces for this config.
func (c BrainConfig) EffectiveLimits() LimitsConfig {
	return c.Limits.Effective()
}

// Validate checks the limits block: negative sizes are configuration errors,
// protected entries must be non-empty, and every glob must be a valid
// slash-separated pattern.
func (l LimitsConfig) Validate() error {
	sizes := []struct {
		field string
		value int
	}{
		{"config.limits.max_body_bytes", l.MaxBodyBytes},
		{"config.limits.max_title_chars", l.MaxTitleChars},
		{"config.limits.max_metadata_bytes", l.MaxMetadataBytes},
	}
	for _, size := range sizes {
		if size.value < 0 {
			return domain.NewValidationError(size.field, "must be zero (use the default) or a positive number")
		}
	}
	for i, project := range l.ProtectedProjects {
		trimmed := strings.TrimSpace(project)
		if trimmed == "" {
			return domain.NewValidationError("config.limits.protected_projects", fmt.Sprintf("entry %d is empty", i+1))
		}
		if strings.ContainsAny(trimmed, `/\`) || trimmed == "." || trimmed == ".." {
			return domain.NewValidationError("config.limits.protected_projects",
				fmt.Sprintf("%q is not a project id (no path separators)", trimmed))
		}
	}
	for i, glob := range l.ProtectedGlobs {
		trimmed := strings.TrimSpace(filepath.ToSlash(glob))
		if trimmed == "" {
			return domain.NewValidationError("config.limits.protected_globs", fmt.Sprintf("entry %d is empty", i+1))
		}
		if _, err := path.Match(trimmed, "projects/example"); err != nil {
			return domain.NewValidationError("config.limits.protected_globs",
				fmt.Sprintf("%q is not a valid glob: %v", glob, err))
		}
	}
	return nil
}

// normalizePatterns trims entries and drops empty ones, so an in-memory
// configuration that bypassed Load still yields a usable matcher.
func normalizePatterns(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(filepath.ToSlash(value))
		trimmed = strings.TrimSuffix(trimmed, "/")
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ReviewConfig is the `review` block of brain.yaml: how long a record of each
// kind stays confidently current before review lists it as due. Dates are
// written to the record's frontmatter at write time; changing the block later
// affects new records and the default applied to legacy records without one.
type ReviewConfig struct {
	Defaults ReviewDefaults `yaml:"defaults,omitempty"`
}

// ReviewDefaults holds one review window per record kind. Values accept a
// whole number of days (90d), weeks (2w) or a Go duration (2160h). Empty
// selects the documented default.
type ReviewDefaults struct {
	Decision string `yaml:"decision,omitempty"`
	Attempt  string `yaml:"attempt,omitempty"`
	Note     string `yaml:"note,omitempty"`
	Handoff  string `yaml:"handoff,omitempty"`
	State    string `yaml:"state,omitempty"`
	Proposal string `yaml:"proposal,omitempty"`
}

// Default review windows, rendered as they appear in brain.yaml.
const (
	DefaultReviewDecision = "90d"
	DefaultReviewAttempt  = "60d"
	DefaultReviewNote     = "180d"
	DefaultReviewHandoff  = "90d"
	DefaultReviewState    = "14d"
	DefaultReviewProposal = "14d"
)

// ReviewWindows is the parsed review cadence: one duration per kind. A zero
// duration means that kind has no review date.
type ReviewWindows struct {
	Decision time.Duration
	Attempt  time.Duration
	Note     time.Duration
	Handoff  time.Duration
	State    time.Duration
	Proposal time.Duration
}

// IsZero lets yaml omit a review block that was never configured.
func (r ReviewConfig) IsZero() bool { return r.Defaults.IsZero() }

// IsZero lets yaml omit a defaults block whose keys were never set.
func (d ReviewDefaults) IsZero() bool {
	return d.Decision == "" && d.Attempt == "" && d.Note == "" && d.Handoff == "" && d.State == "" && d.Proposal == ""
}

// Effective resolves omitted windows to the documented defaults.
func (d ReviewDefaults) Effective() ReviewDefaults {
	out := ReviewDefaults{
		Decision: firstNonEmpty(d.Decision, DefaultReviewDecision),
		Attempt:  firstNonEmpty(d.Attempt, DefaultReviewAttempt),
		Note:     firstNonEmpty(d.Note, DefaultReviewNote),
		Handoff:  firstNonEmpty(d.Handoff, DefaultReviewHandoff),
		State:    firstNonEmpty(d.State, DefaultReviewState),
		Proposal: firstNonEmpty(d.Proposal, DefaultReviewProposal),
	}
	return out
}

// Windows parses the effective review windows. Validation already rejected
// malformed values; a store that skipped Validate still gets the defaults for
// a value that does not parse, never a hard failure at write time.
func (d ReviewDefaults) Windows() ReviewWindows {
	effective := d.Effective()
	parse := func(value string, fallback time.Duration) time.Duration {
		window, err := domain.ParseReviewWindow(value)
		if err != nil {
			return fallback
		}
		return window
	}
	return ReviewWindows{
		Decision: parse(effective.Decision, domain.DefaultReviewDecision),
		Attempt:  parse(effective.Attempt, domain.DefaultReviewAttempt),
		Note:     parse(effective.Note, domain.DefaultReviewNote),
		Handoff:  parse(effective.Handoff, domain.DefaultReviewHandoff),
		State:    parse(effective.State, domain.DefaultReviewState),
		Proposal: parse(effective.Proposal, domain.DefaultReviewProposal),
	}
}

// Validate rejects malformed or non-positive review windows and names the key.
func (d ReviewDefaults) Validate() error {
	entries := []struct {
		key   string
		value string
	}{
		{"decision", d.Decision},
		{"attempt", d.Attempt},
		{"note", d.Note},
		{"handoff", d.Handoff},
		{"state", d.State},
		{"proposal", d.Proposal},
	}
	for _, entry := range entries {
		if strings.TrimSpace(entry.value) == "" {
			continue
		}
		if _, err := domain.ParseReviewWindow(entry.value); err != nil {
			return domain.NewValidationError("config.review.defaults."+entry.key, err.Error())
		}
	}
	return nil
}

// EffectiveReviewWindows returns the review windows the engine applies.
func (c BrainConfig) EffectiveReviewWindows() ReviewWindows {
	return c.Review.Defaults.Windows()
}

func firstNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
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
	if err := c.Limits.Validate(); err != nil {
		return err
	}
	if err := c.Review.Defaults.Validate(); err != nil {
		return err
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
