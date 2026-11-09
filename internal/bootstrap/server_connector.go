package bootstrap

import (
	"context"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpclient"
	"github.com/levapveeskela/lv24-brain/internal/adapters/secrets"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// RunServerConnector asks the configured brain server to run one read-only
// connector and returns the server-side run record. The CLI uses this path in
// server mode; local/git mode keeps the direct connector and Markdown sink.
func RunServerConnector(ctx context.Context, cfg config.BrainConfig, opts ConnectorOptions, since *time.Time) (domain.ConnectorRun, error) {
	if cfg.Mode != config.ModeServer {
		return domain.ConnectorRun{}, domain.NewValidationError("config.mode", "must be server")
	}
	serverURL := strings.TrimSpace(cfg.ServerURL)
	if serverURL == "" {
		return domain.ConnectorRun{}, domain.NewValidationError("config.server_url", "is required in server mode")
	}
	secretStore := secrets.Default()
	client, err := httpclient.New(httpclient.Config{
		BaseURL: serverURL,
		Token:   resolveServerToken(secretStore),
	})
	if err != nil {
		return domain.ConnectorRun{}, err
	}
	if !staticTokenFromEnv() {
		auth, authErr := httpclient.NewAuth(client, secretStore)
		if authErr != nil {
			return domain.ConnectorRun{}, authErr
		}
		client.SetTokenProvider(auth)
	}
	projects := make([]string, 0, 1)
	if project := strings.TrimSpace(opts.Project); project != "" {
		projects = append(projects, project)
	}
	return client.RunConnectorMapped(ctx, opts.Name, projects, since, opts.Limit, opts.BrainProject)
}
