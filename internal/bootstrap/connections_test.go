package bootstrap_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestBuildConnectionsLocalAndGit(t *testing.T) {
	for _, mode := range []string{config.ModeLocal, config.ModeGit} {
		t.Run(mode, func(t *testing.T) {
			// Arrange: deliberately omit a git repository and search index.
			cfg := config.Default()
			cfg.Mode, cfg.DataDir = mode, filepath.Join(t.TempDir(), "data")
			secrets := &fakeSecretStore{values: map[string]string{}}

			// Act: the narrow composition root needs neither git nor SQLite.
			runtime, err := bootstrap.BuildConnections(cfg, bootstrap.Options{Secrets: secrets})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			connection := domain.Connection{ID: "work", Provider: "github", Settings: map[string]string{"repos": "a/b"}}
			if err := runtime.Connections.Create(ctx, connection); err != nil {
				t.Fatal(err)
			}
			if err := runtime.Credentials.Set(ctx, "work", "token", "private-value"); err != nil {
				t.Fatal(err)
			}
			reopened, err := bootstrap.BuildConnections(cfg, bootstrap.Options{Secrets: secrets})
			if err != nil {
				t.Fatal(err)
			}
			got, err := reopened.Connections.Get(ctx, "work")

			// Assert: catalogue and credentials use the intended stores/namespaces.
			if err != nil || !reflect.DeepEqual(got, connection) {
				t.Fatalf("connection = %+v, %v", got, err)
			}
			if value, err := reopened.Credentials.Get(ctx, "work", "token"); err != nil || value != "private-value" {
				t.Fatalf("credential = %q, %v", value, err)
			}
			if value := secrets.values["conn/work/token"]; value != "private-value" {
				t.Fatal("wrong credential namespace")
			}
			if !reflect.DeepEqual(runtime.Registry.Providers(), connectorbuild.Builtin().Providers()) {
				t.Fatal("registry differs from built-ins")
			}
			if runtime.Migrator.Discover == nil || runtime.Migrator.Connections == nil || runtime.Migrator.Credentials == nil || runtime.Migrator.LegacySecrets == nil {
				t.Fatal("incomplete migration wiring")
			}
			for _, path := range []string{".git", "index", "projects"} {
				if _, err := os.Stat(filepath.Join(cfg.DataDir, path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexpected %s: %v", path, err)
				}
			}
		})
	}
}

func TestBuildConnectionsRejectsUnsupportedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		mode, dataDir string
		want          error
	}{
		{config.ModeServer, t.TempDir(), domain.ErrUnsupported},
		{"unknown", t.TempDir(), domain.ErrValidation},
		{config.ModeLocal, "", domain.ErrValidation},
	} {
		cfg := config.Default()
		cfg.Mode, cfg.DataDir = tc.mode, tc.dataDir
		runtime, err := bootstrap.BuildConnections(cfg, bootstrap.Options{Secrets: &fakeSecretStore{}})
		if runtime != nil || !errors.Is(err, tc.want) {
			t.Errorf("mode=%s: runtime=%v err=%v", tc.mode, runtime, err)
		}
	}
}
