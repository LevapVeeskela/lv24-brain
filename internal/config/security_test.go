package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadQuarantineAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	raw := "mode: local\ndata_dir: ./brain\nsecurity:\n  quarantine_agent: true\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Security.QuarantineAgent {
		t.Fatalf("security.quarantine_agent = false, want true")
	}

	// The default stays false and a save keeps the setting.
	cfg.Security.QuarantineAgent = true
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if !reloaded.Security.QuarantineAgent {
		t.Fatal("security.quarantine_agent was lost by Save")
	}
	if Default().Security.QuarantineAgent {
		t.Fatal("quarantine_agent is on by default")
	}
}
