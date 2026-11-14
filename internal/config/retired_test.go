package config

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestEffectiveRetiredPathsHasNoBuiltInDefaults(t *testing.T) {
	if got := Default().EffectiveRetiredPaths(); len(got) != 0 {
		t.Fatalf("Default().EffectiveRetiredPaths() = %+v, want none (the engine is data-free)", got)
	}

	root := t.TempDir()
	configured := []RetiredPath{
		{Path: filepath.Join(root, "old-checkout"), ReplacedBy: "harvest"},
		{Path: filepath.Join(root, "second"), ReplacedBy: "demo"},
	}
	cfg := Default()
	cfg.RetiredPaths = configured
	got := cfg.EffectiveRetiredPaths()
	if len(got) != len(configured) {
		t.Fatalf("EffectiveRetiredPaths() = %+v, want the configured entries only", got)
	}
	for i := range configured {
		if got[i] != configured[i] {
			t.Fatalf("EffectiveRetiredPaths()[%d] = %+v, want %+v", i, got[i], configured[i])
		}
	}
}

func TestValidateRetiredPaths(t *testing.T) {
	valid := func() BrainConfig {
		cfg := Default()
		cfg.DataDir = "brain"
		return cfg
	}
	root := t.TempDir()
	for name, entry := range map[string]RetiredPath{
		"missing path":        {ReplacedBy: "demo"},
		"missing replacement": {Path: filepath.Join(root, "old-checkout")},
	} {
		cfg := valid()
		cfg.RetiredPaths = []RetiredPath{entry}
		err := cfg.Validate()
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: Validate() = %v, want ErrValidation", name, err)
		}
		var validationErr *domain.ValidationError
		if errors.As(err, &validationErr) && validationErr.Field != "config.retired_paths" {
			t.Fatalf("%s: field = %q, want config.retired_paths", name, validationErr.Field)
		}
	}
	cfg := valid()
	cfg.RetiredPaths = []RetiredPath{{Path: filepath.Join(root, "old-checkout"), ReplacedBy: "demo"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with a complete entry = %v", err)
	}
}
