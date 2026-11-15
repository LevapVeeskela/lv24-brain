package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestContextBudgetDefaultsAndValidation(t *testing.T) {
	// Arrange
	cfg := config.Default()

	// Act / Assert: an omitted block selects the documented default.
	if got := cfg.EffectiveContextBudget(); got != domain.DefaultContextBudgetTokens {
		t.Fatalf("effective budget = %d, want %d", got, domain.DefaultContextBudgetTokens)
	}
	for name, budget := range map[string]int{
		"negative":  -1,
		"too large": domain.MaxContextBudgetTokens + 1,
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cfg
			invalid.Context.Budget = budget
			if err := invalid.Validate(); err == nil {
				t.Fatalf("Validate accepted budget %d", budget)
			}
		})
	}
	cfg.Context.Budget = domain.MaxContextBudgetTokens
	cfg.DataDir = "data"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(max) = %v", err)
	}
	if got := cfg.EffectiveContextBudget(); got != domain.MaxContextBudgetTokens {
		t.Fatalf("effective budget = %d, want %d", got, domain.MaxContextBudgetTokens)
	}
}

func TestContextBudgetRoundTripsThroughBrainYAML(t *testing.T) {
	// Arrange
	dir := t.TempDir()
	path := filepath.Join(dir, "brain.yaml")
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.Context.Budget = 9000

	// Act
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Context.Budget != 9000 || loaded.EffectiveContextBudget() != 9000 {
		t.Fatalf("loaded budget = %d effective = %d\n%s", loaded.Context.Budget, loaded.EffectiveContextBudget(), raw)
	}
}
