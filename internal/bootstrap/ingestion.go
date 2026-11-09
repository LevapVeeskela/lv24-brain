package bootstrap

import (
	"github.com/levapveeskela/lv24-brain/internal/adapters/connectionlock"
	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitstore"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/oauthlogin"
)

// IngestionRuntime owns the item ingestion host and its local search index.
type IngestionRuntime struct {
	Ingestion *app.Ingestion
	runtime   *Runtime
}

// Close releases the derived search index.
func (r *IngestionRuntime) Close() error { return r.runtime.Close() }

// Services exposes the runtime services the agent control API shares
// (secrets, projects, updater). It is nil only for a zero IngestionRuntime.
func (r *IngestionRuntime) Services() *Runtime {
	if r == nil {
		return nil
	}
	return r.runtime
}

// BuildIngestion composes a connection catalogue, raw-zone stores, process
// lock, credential resolver, run history and index for local/git ingestion.
func BuildIngestion(cfg config.BrainConfig, opts Options) (*IngestionRuntime, error) {
	catalogue, err := BuildConnections(cfg, opts)
	if err != nil {
		return nil, err
	}
	store, err := fsstore.New(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	locker, err := connectionlock.New(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	runtime, err := BuildWithOptions(cfg, opts)
	if err != nil {
		return nil, err
	}
	sink, err := connectors.NewDirSink(cfg.DataDir)
	if err != nil {
		_ = runtime.Close()
		return nil, err
	}
	host := &app.Ingestion{
		Connections: catalogue.Connections, Registry: catalogue.Registry,
		Deps: connectorbuild.Deps{
			Credentials: &oauthlogin.Credentials{
				Base:     catalogue.Credentials,
				Sessions: &oauthlogin.SessionStore{Secrets: runtime.Secrets},
			},
			Secrets: withGHTokenFallback(runtime.Secrets),
			Sink:    sink,
		},
		Items: store.Items(), Checkpoints: store.Checkpoints(), Locker: locker, Index: runtime.App, Runs: store,
	}
	if cfg.Mode == config.ModeGit {
		provider, err := gitstore.Open(cfg.DataDir, gitstore.Options{})
		if err != nil {
			_ = runtime.Close()
			return nil, err
		}
		host.PreparePublish = provider.PrepareConnectionPublish
	}
	return &IngestionRuntime{Ingestion: host, runtime: runtime}, nil
}
