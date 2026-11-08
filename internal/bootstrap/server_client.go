package bootstrap

import (
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/adapters/secrets"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// NewServerClient builds an authenticated HTTP client for server-only admin
// commands (project members, API tokens). It resolves credentials like the
// server runtime: a static token from the environment, else the stored login.
func NewServerClient(cfg config.BrainConfig) (*httpclient.Client, error) {
	if cfg.Mode != config.ModeServer {
		return nil, domain.NewValidationError("config.mode", "must be server")
	}
	serverURL := strings.TrimSpace(cfg.ServerURL)
	if serverURL == "" {
		return nil, domain.NewValidationError("config.server_url", "is required in server mode")
	}
	secretStore := secrets.Default()
	client, err := httpclient.New(httpclient.Config{BaseURL: serverURL, Token: resolveServerToken(secretStore)})
	if err != nil {
		return nil, err
	}
	if !staticTokenFromEnv() {
		auth, err := httpclient.NewAuth(client, secretStore)
		if err != nil {
			return nil, err
		}
		client.SetTokenProvider(auth)
	}
	return client, nil
}
