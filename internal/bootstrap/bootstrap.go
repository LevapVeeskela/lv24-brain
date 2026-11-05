// Package bootstrap is the CLI composition root. It selects filesystem, git,
// index and update adapters and injects them into the use-case layer.
package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/fsstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitstore"
	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/adapters/secrets"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sqliteindex"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/update"
)

const (
	indexRelativePath = "index/brain.db"
	serverTokenKey    = "server-token"
	serverTokenEnv    = "BRAIN_TOKEN"
	// serverSecretTokenEnv is the environment form of the static server token
	// read by the secret store (BRAIN_SECRET_ + canonical SERVER_TOKEN).
	serverSecretTokenEnv = secrets.DefaultEnvPrefix + "SERVER_TOKEN"
)

// Options overrides adapters for tests and embedding.
type Options struct {
	// Secrets replaces the default environment/OS secret store.
	Secrets ports.SecretStore
}

// Runtime owns the use cases and the resources created for them.
type Runtime struct {
	App     *app.App
	Secrets ports.SecretStore
	// Updater is the concrete updater behind App. Update; the CLI uses it for
	// the notify-only daily check.
	Updater *update.Updater
	// RunStore persists connector run records. It is nil in server mode: the CLI
	// calls the server API instead of writing a local Markdown run store.
	RunStore ports.ConnectorRunStore
	index    *sqliteindex.Index
}

// Build selects adapters for cfg with production defaults.
func Build(cfg config.BrainConfig) (*Runtime, error) {
	return BuildWithOptions(cfg, Options{})
}

// BuildWithOptions selects adapters for cfg. A missing or uninitialized data
// directory (one without projects/) is not fatal: the runtime keeps diagnostics
// usable and write use cases report that the store is unavailable. Server mode
// selects the HTTP client with a token from BRAIN_TOKEN, then the OS secret
// store key "server-token".
func BuildWithOptions(cfg config.BrainConfig, opts Options) (*Runtime, error) {
	secretStore := selectSecrets(opts)
	deps := app.Dependencies{Now: time.Now, Secrets: secretStore}
	var index *sqliteindex.Index
	var runStore ports.ConnectorRunStore

	switch cfg.Mode {
	case config.ModeLocal, config.ModeGit:
		store, openedIndex, err := openFilesystem(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		if store != nil {
			deps.Store = store
			runStore = store
			index = openedIndex
			deps.Index = index
		}
		if cfg.Mode == config.ModeGit && store != nil {
			sync, syncErr := gitstore.Open(cfg.DataDir, gitstore.Options{})
			if syncErr != nil {
				if index != nil {
					_ = index.Close()
				}
				return nil, syncErr
			}
			deps.Sync = sync
		}
	case config.ModeServer:
		if strings.TrimSpace(cfg.ServerURL) != "" {
			client, clientErr := httpclient.New(httpclient.Config{
				BaseURL: cfg.ServerURL,
				Token:   resolveServerToken(secretStore),
			})
			if clientErr == nil {
				deps.Store = client
				// BRAIN_TOKEN and an environment-provided
				// BRAIN_SECRET_SERVER_TOKEN pin the client to a static token
				// and win over stored credentials. Login would be confusing
				// (and could silently ignore the env token), so only stored
				// credentials wire the auth provider, refresh, and logout.
				if !staticTokenFromEnv() {
					auth, authErr := httpclient.NewAuth(client, secretStore)
					if authErr == nil {
						client.SetTokenProvider(auth)
						deps.Auth = auth
					}
				}
			}
			// Invalid server URLs are reported by doctor's config check rather
			// than blocking diagnostics with a bootstrap error.
		}
	default:
		return nil, domain.NewValidationError("config.mode", "must be local, git, or server")
	}

	updater, err := update.New(update.Options{IgnoreDisabled: true})
	if err != nil {
		if index != nil {
			_ = index.Close()
		}
		return nil, err
	}
	deps.Updater = updater

	return &Runtime{
		App:      app.New(cfg, deps),
		Secrets:  secretStore,
		Updater:  updater,
		RunStore: runStore,
		index:    index,
	}, nil
}

func selectSecrets(opts Options) ports.SecretStore {
	if opts.Secrets != nil {
		return opts.Secrets
	}
	return secrets.Default()
}

// OpenRunStore opens only the connector run store. Connector runs must not
// depend on git sync or the SQLite index: a broken repository should not block
// recording its own run history. A missing or uninitialized data directory
// yields a nil store, matching Runtime.RunStore.
func OpenRunStore(cfg config.BrainConfig) (ports.ConnectorRunStore, error) {
	if cfg.Mode != config.ModeLocal && cfg.Mode != config.ModeGit {
		return nil, nil
	}
	dataDir := strings.TrimSpace(cfg.DataDir)
	if dataDir == "" {
		return nil, nil
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}
	projects, err := os.Stat(filepath.Join(dataDir, "projects"))
	if err != nil || !projects.IsDir() {
		return nil, nil
	}
	return fsstore.New(dataDir)
}

func resolveServerToken(store ports.SecretStore) string {
	if token := strings.TrimSpace(os.Getenv(serverTokenEnv)); token != "" {
		return token
	}
	if store == nil {
		return ""
	}
	token, err := store.Get(context.Background(), serverTokenKey)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}

// staticTokenFromEnv reports whether a static server token is supplied by the
// environment. Both names shadow stored credentials and must disable login and
// logout for the process.
func staticTokenFromEnv() bool {
	return strings.TrimSpace(os.Getenv(serverTokenEnv)) != "" ||
		strings.TrimSpace(os.Getenv(serverSecretTokenEnv)) != ""
}

// Close releases the SQLite index. It is safe to call on a nil Runtime or a
// runtime without an index.
func (r *Runtime) Close() error {
	if r == nil || r.index == nil {
		return nil
	}
	index := r.index
	r.index = nil
	return index.Close()
}

func openFilesystem(dataDir string) (*fsstore.Store, *sqliteindex.Index, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return nil, nil, nil
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, nil
	}
	if projectsInfo, projectsErr := os.Stat(filepath.Join(dataDir, "projects")); projectsErr != nil || !projectsInfo.IsDir() {
		return nil, nil, nil
	}
	store, err := fsstore.New(dataDir)
	if err != nil {
		return nil, nil, err
	}
	index, err := sqliteindex.Open(filepath.Join(dataDir, filepath.FromSlash(indexRelativePath)))
	if err != nil {
		// The index is derived data; Markdown remains usable without it.
		return store, nil, nil
	}
	return store, index, nil
}
