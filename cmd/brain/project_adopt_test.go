package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func adoptTestConfig(t *testing.T, machine string) string {
	t.Helper()
	dataDir := t.TempDir()
	path := filepath.Join(t.TempDir(), "brain.yaml")
	cfg := config.Default()
	cfg.Mode = config.ModeLocal
	cfg.DataDir = dataDir
	cfg.MachineID = machine
	if err := config.Save(path, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	store, err := fsstore.New(dataDir)
	if err != nil {
		t.Fatalf("fsstore.New: %v", err)
	}
	ctx := context.Background()
	if err := store.PutProject(ctx, domain.Project{
		ID: "legacy", Name: "Legacy", Kind: domain.ProjectKindRepository,
		Paths: []string{t.TempDir(), filepath.Join(t.TempDir(), "absent")},
	}); err != nil {
		t.Fatalf("PutProject: %v", err)
	}
	return path
}

func TestProjectAdoptIsDryRunByDefault(t *testing.T) {
	cfgPath := adoptTestConfig(t, "cli-machine")

	code, stdout, stderr := runForTest(t, "project", "adopt", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project adopt: %d %s", code, stderr)
	}
	if !strings.Contains(stdout, "machine: cli-machine") || !strings.Contains(stdout, "1 stamped") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "pass --apply") {
		t.Fatalf("dry run hint missing: %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "project", "adopt", "--json", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project adopt --json: %d %s", code, stderr)
	}
	var envelope struct {
		Schema int `json:"schema"`
		Data   struct {
			Machine string `json:"machine"`
			Applied bool   `json:"applied"`
			Stamped int    `json:"stamped"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("json: %v (%q)", err, stdout)
	}
	if envelope.Schema != jsonSchemaVersion || envelope.Data.Machine != "cli-machine" ||
		envelope.Data.Applied || envelope.Data.Stamped != 1 {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestProjectAdoptAppliesOwners(t *testing.T) {
	cfgPath := adoptTestConfig(t, "cli-machine")

	code, stdout, stderr := runForTest(t, "project", "adopt", "--apply", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("project adopt --apply: %d %s", code, stderr)
	}
	if !strings.Contains(stdout, "stamp: legacy") || strings.Contains(stdout, "pass --apply") {
		t.Fatalf("stdout = %q", stdout)
	}

	code, stdout, stderr = runForTest(t, "project", "adopt", "--apply", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("second adopt: %d %s", code, stderr)
	}
	if !strings.Contains(stdout, "0 stamped, 1 already owned, 1 not on this machine") {
		t.Fatalf("second run = %q", stdout)
	}
}
