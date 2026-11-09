package bootstrap

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ConnectorOptions selects one read-only connector and its configuration hints.
type ConnectorOptions struct {
	Name         string
	Project      string
	BrainProject string
	Limit        int
	// Data-derived targets; see connectorbuild.Options.
	Repos          []string
	GitLabProjects []string
	IMAP           *connectorbuild.IMAPAccount
}

// BuildConnector selects one legacy read-only Markdown-dump connector and
// points its sink at the brain data directory. It backs `connector run --name`
// and the empty-catalogue `connector run --all` fallback; the native
// item-level path is `connection run` through BuildIngestion. Credentials are
// resolved by the connectors from the environment/OS secret store at run time.
func BuildConnector(cfg config.BrainConfig, opts ConnectorOptions, buildOpts Options) (ports.Connector, error) {
	if cfg.Mode == config.ModeServer {
		return nil, fmt.Errorf("%w: connectors require local or git mode with data_dir", domain.ErrUnsupported)
	}
	dataDir := strings.TrimSpace(cfg.DataDir)
	if dataDir == "" {
		return nil, domain.NewValidationError("config.data_dir", "is required for connectors")
	}
	sink, err := connectors.NewDirSink(dataDir)
	if err != nil {
		return nil, err
	}
	projects := make([]string, 0, 1)
	if project := strings.TrimSpace(opts.Project); project != "" {
		projects = append(projects, project)
	}
	return connectorbuild.Build(connectorbuild.Options{
		Name:           opts.Name,
		Projects:       projects,
		Limit:          opts.Limit,
		Secrets:        withGHTokenFallback(selectSecrets(buildOpts)),
		Sink:           sink,
		StatePath:      filepath.Join(dataDir, "connectors", "mail-state.json"),
		Repos:          opts.Repos,
		GitLabProjects: opts.GitLabProjects,
		IMAP:           opts.IMAP,
	})
}
