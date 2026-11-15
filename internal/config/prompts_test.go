package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPromptsConfigDefaults(t *testing.T) {
	var cfg PromptsConfig
	if cfg.Enabled {
		t.Fatal("prompt zone is enabled by default")
	}
	if got := cfg.EffectiveRetentionDays(); got != DefaultPromptRetentionDays {
		t.Fatalf("retention = %d, want %d", got, DefaultPromptRetentionDays)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("zero config rejected: %v", err)
	}
}

func TestPromptsConfigYAML(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		enabled   bool
		retention int
	}{
		{name: "omitted", raw: "mode: local\n", retention: 30},
		{name: "enabled", raw: "mode: local\nprompts:\n  enabled: true\n", enabled: true, retention: 30},
		{name: "explicit retention", raw: "mode: local\nprompts:\n  enabled: true\n  retention_days: 7\n", enabled: true, retention: 7},
		{name: "keep forever", raw: "mode: local\nprompts:\n  enabled: true\n  retention_days: 0\n", enabled: true, retention: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var cfg BrainConfig
			if err := yaml.Unmarshal([]byte(test.raw), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if cfg.Prompts.Enabled != test.enabled {
				t.Fatalf("enabled = %v, want %v", cfg.Prompts.Enabled, test.enabled)
			}
			if got := cfg.Prompts.EffectiveRetentionDays(); got != test.retention {
				t.Fatalf("retention = %d, want %d", got, test.retention)
			}
		})
	}
}

func TestPromptsConfigRejectsNegativeRetention(t *testing.T) {
	negative := -1
	err := PromptsConfig{RetentionDays: &negative}.Validate()
	if err == nil || !strings.Contains(err.Error(), "retention_days") {
		t.Fatalf("error = %v", err)
	}
}

func TestPromptBlockIsOmittedWhenUnset(t *testing.T) {
	raw, err := yaml.Marshal(BrainConfig{Mode: ModeLocal, SchemaVersion: 1, UpdateChannel: ChannelStable})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "prompts:") {
		t.Fatalf("unset prompts block was written:\n%s", raw)
	}
}
