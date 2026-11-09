package bootstrap

import (
	"context"
	"fmt"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ConnectionRuntime manages the local catalogue without opening an index,
// initializing git sync, or contacting a provider.
type ConnectionRuntime struct {
	Connections ports.ConnectionStore
	Credentials ports.CredentialStore
	// Secrets is the raw local secret store, used for OAuth session bundles.
	Secrets  ports.SecretStore
	Registry *connectorbuild.Registry
	Migrator app.ConnectionMigrator
}

// BuildConnections wires local/git connection management. Discovery is lazy:
// only migration reads legacy project targets, local remotes and mail.json.
func BuildConnections(cfg config.BrainConfig, opts Options) (*ConnectionRuntime, error) {
	if cfg.Mode == config.ModeServer {
		return nil, fmt.Errorf("%w: connection management is not available in server mode yet", domain.ErrUnsupported)
	}
	if cfg.Mode != config.ModeLocal && cfg.Mode != config.ModeGit {
		return nil, domain.NewValidationError("config.mode", "must be local or git")
	}
	connections, err := fsstore.NewConnectionStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	secrets := selectSecrets(opts)
	credentials := connectorbuild.SecretCredentials{Store: secrets}
	return &ConnectionRuntime{
		Connections: connections,
		Credentials: credentials,
		Secrets:     secrets,
		Registry:    connectorbuild.Builtin(),
		Migrator: app.ConnectionMigrator{
			Connections:   connections,
			Credentials:   credentials,
			LegacySecrets: secrets,
			Discover: func(ctx context.Context) ([]app.LegacyConnection, error) {
				store, err := fsstore.New(cfg.DataDir)
				if err != nil {
					return nil, err
				}
				projects, err := store.ListProjects(ctx)
				if err != nil {
					return nil, err
				}
				return DiscoverLegacyConnectors(ctx, LegacyConnectorOptions{DataDir: cfg.DataDir, Projects: projects})
			},
		},
	}, nil
}
