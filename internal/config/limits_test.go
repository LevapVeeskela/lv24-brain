package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// writeLimitsConfig writes a minimal local-mode brain.yaml with the given
// limits block and loads it.
func writeLimitsConfig(t *testing.T, limits string) BrainConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brain.yaml")
	raw := "mode: local\ndata_dir: ./brain\n" + limits
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return cfg
}

func TestLimitsDefaultWhenOmitted(t *testing.T) {
	cfg := writeLimitsConfig(t, "")
	limits := cfg.EffectiveLimits()
	want := LimitsConfig{
		MaxBodyBytes:     DefaultMaxBodyBytes,
		MaxTitleChars:    DefaultMaxTitleChars,
		MaxMetadataBytes: DefaultMaxMetadataBytes,
	}
	if !reflect.DeepEqual(limits, want) {
		t.Fatalf("EffectiveLimits() = %+v, want %+v", limits, want)
	}
	if DefaultMaxBodyBytes != 1<<20 || DefaultMaxTitleChars != 200 || DefaultMaxMetadataBytes != 16<<10 {
		t.Fatalf("documented defaults changed: %+v", want)
	}
}

func TestLimitsZeroMeansDefaultNotDisabled(t *testing.T) {
	cfg := writeLimitsConfig(t, "limits:\n  max_body_bytes: 0\n  max_title_chars: 0\n  max_metadata_bytes: 0\n")
	limits := cfg.EffectiveLimits()
	if limits.MaxBodyBytes != DefaultMaxBodyBytes || limits.MaxTitleChars != DefaultMaxTitleChars ||
		limits.MaxMetadataBytes != DefaultMaxMetadataBytes {
		t.Fatalf("zero limits disabled a cap: %+v", limits)
	}
}

func TestLimitsExplicitTinyValuesSurviveSaveAndLoad(t *testing.T) {
	cfg := writeLimitsConfig(t, "limits:\n  max_body_bytes: 64\n  max_title_chars: 5\n  max_metadata_bytes: 32\n")
	want := LimitsConfig{MaxBodyBytes: 64, MaxTitleChars: 5, MaxMetadataBytes: 32}
	if got := cfg.EffectiveLimits(); !reflect.DeepEqual(got, want) {
		t.Fatalf("EffectiveLimits() = %+v, want %+v", got, want)
	}

	path := filepath.Join(t.TempDir(), "brain.yaml")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := reloaded.EffectiveLimits(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded limits = %+v, want %+v", got, want)
	}
}

func TestLimitsProtectedListsRoundTrip(t *testing.T) {
	cfg := writeLimitsConfig(t, "limits:\n  protected_projects:\n    - \" global \"\n    - templates\n  protected_globs:\n    - projects/global/**\n    - \"projects/templates/\"\n")
	limits := cfg.EffectiveLimits()
	if !reflect.DeepEqual(limits.ProtectedProjects, []string{"global", "templates"}) {
		t.Fatalf("ProtectedProjects = %+v", limits.ProtectedProjects)
	}
	if !reflect.DeepEqual(limits.ProtectedGlobs, []string{"projects/global/**", "projects/templates"}) {
		t.Fatalf("ProtectedGlobs = %+v", limits.ProtectedGlobs)
	}
}

func TestLimitsValidation(t *testing.T) {
	valid := func() BrainConfig {
		cfg := Default()
		cfg.DataDir = "brain"
		return cfg
	}
	tests := []struct {
		name   string
		mutate func(*BrainConfig)
		field  string
	}{
		{
			name:   "negative body",
			mutate: func(c *BrainConfig) { c.Limits.MaxBodyBytes = -1 },
			field:  "config.limits.max_body_bytes",
		},
		{
			name:   "negative title",
			mutate: func(c *BrainConfig) { c.Limits.MaxTitleChars = -1 },
			field:  "config.limits.max_title_chars",
		},
		{
			name:   "negative metadata",
			mutate: func(c *BrainConfig) { c.Limits.MaxMetadataBytes = -1 },
			field:  "config.limits.max_metadata_bytes",
		},
		{
			name:   "empty protected project",
			mutate: func(c *BrainConfig) { c.Limits.ProtectedProjects = []string{" "} },
			field:  "config.limits.protected_projects",
		},
		{
			name:   "protected project is a path",
			mutate: func(c *BrainConfig) { c.Limits.ProtectedProjects = []string{"projects/global"} },
			field:  "config.limits.protected_projects",
		},
		{
			name:   "bad glob",
			mutate: func(c *BrainConfig) { c.Limits.ProtectedGlobs = []string{"projects/[global"} },
			field:  "config.limits.protected_globs",
		},
		{
			name:   "empty glob",
			mutate: func(c *BrainConfig) { c.Limits.ProtectedGlobs = []string{""} },
			field:  "config.limits.protected_globs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Validate() error = %v, want ErrValidation", err)
			}
			var validationErr *domain.ValidationError
			if errors.As(err, &validationErr) && validationErr.Field != tt.field {
				t.Fatalf("validation field = %q, want %q", validationErr.Field, tt.field)
			}
		})
	}
}

func TestLimitsValidConfigPasses(t *testing.T) {
	cfg := Default()
	cfg.DataDir = "brain"
	cfg.Limits = LimitsConfig{
		MaxBodyBytes:      1024,
		MaxTitleChars:     50,
		MaxMetadataBytes:  256,
		ProtectedProjects: []string{"global"},
		ProtectedGlobs:    []string{"projects/global/**"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestSaveOmitsEmptyLimitsBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := Default()
	cfg.DataDir = "brain"
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "limits:") {
		t.Fatalf("unconfigured limits were written to brain.yaml:\n%s", raw)
	}
}
