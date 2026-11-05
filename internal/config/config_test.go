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

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "brain.yaml")
	want := BrainConfig{
		Mode:             ModeGit,
		DataDir:          filepath.Join(dir, "data"),
		User:             "alex",
		SchemaVersion:    CurrentSchemaVersion,
		MinEngineVersion: "0.1.0",
		UpdateChannel:    ChannelBeta,
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	raw := "mode: local\ndata_dir: ./brain\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", got.SchemaVersion, CurrentSchemaVersion)
	}
	if got.UpdateChannel != ChannelStable {
		t.Fatalf("UpdateChannel = %q, want %q", got.UpdateChannel, ChannelStable)
	}
}

func TestSaveRejectsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brain.yaml")
	err := Save(path, BrainConfig{Mode: ModeLocal})
	if err == nil {
		t.Fatal("Save() error = nil, want validation error")
	}
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("Save() error = %v, want ErrValidation", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("config file should not exist after failed Save, stat error = %v", statErr)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("Load() error = nil, want error")
	}
	if !IsNotExist(err) {
		t.Fatalf("IsNotExist(%v) = false, want true", err)
	}
}

func TestValidate(t *testing.T) {
	valid := func() BrainConfig {
		return BrainConfig{
			Mode:          ModeLocal,
			DataDir:       "C:/brain",
			SchemaVersion: CurrentSchemaVersion,
			UpdateChannel: ChannelStable,
		}
	}
	tests := []struct {
		name    string
		mutate  func(*BrainConfig)
		wantErr bool
		field   string
	}{
		{name: "valid local"},
		{
			name: "valid git",
			mutate: func(c *BrainConfig) {
				c.Mode = ModeGit
				c.User = "alex"
			},
		},
		{
			name: "valid server",
			mutate: func(c *BrainConfig) {
				c.Mode = ModeServer
				c.DataDir = ""
				c.ServerURL = "https://brain.example.com"
			},
		},
		{
			name:    "unknown mode",
			mutate:  func(c *BrainConfig) { c.Mode = "cloud" },
			wantErr: true,
			field:   "config.mode",
		},
		{
			name:    "local without data dir",
			mutate:  func(c *BrainConfig) { c.DataDir = "" },
			wantErr: true,
			field:   "config.data_dir",
		},
		{
			name: "server without url",
			mutate: func(c *BrainConfig) {
				c.Mode = ModeServer
				c.DataDir = ""
			},
			wantErr: true,
			field:   "config.server_url",
		},
		{
			name: "server with bad scheme",
			mutate: func(c *BrainConfig) {
				c.Mode = ModeServer
				c.DataDir = ""
				c.ServerURL = "ftp://brain.example.com"
			},
			wantErr: true,
			field:   "config.server_url",
		},
		{
			name:    "unknown channel",
			mutate:  func(c *BrainConfig) { c.UpdateChannel = "nightly" },
			wantErr: true,
			field:   "config.update_channel",
		},
		{
			name:    "zero schema",
			mutate:  func(c *BrainConfig) { c.SchemaVersion = 0 },
			wantErr: true,
			field:   "config.schema_version",
		},
		{
			name:    "future schema",
			mutate:  func(c *BrainConfig) { c.SchemaVersion = CurrentSchemaVersion + 1 },
			wantErr: true,
			field:   "config.schema_version",
		},
		{
			name:    "bad min engine version",
			mutate:  func(c *BrainConfig) { c.MinEngineVersion = "latest" },
			wantErr: true,
			field:   "config.min_engine_version",
		},
		{
			name:   "pre-release min engine version",
			mutate: func(c *BrainConfig) { c.MinEngineVersion = "1.2.3-beta.1" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Validate() error = nil, want error")
				}
				if !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("Validate() error = %v, want ErrValidation", err)
				}
				var validationErr *domain.ValidationError
				if errors.As(err, &validationErr) && validationErr.Field != tt.field {
					t.Fatalf("validation field = %q, want %q", validationErr.Field, tt.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestSaveWritesNoSecretsHeaderOnly(t *testing.T) {
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
	if !strings.HasPrefix(string(raw), "# lv24-brain instance configuration\n") {
		t.Fatalf("config header missing: %q", raw)
	}
}
