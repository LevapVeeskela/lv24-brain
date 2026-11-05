package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/github"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitlab"
	"github.com/levapveeskela/lv24-brain/internal/adapters/imap"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ConnectorOptions selects one read-only connector and its configuration hints.
type ConnectorOptions struct {
	Name    string
	Project string
	Limit   int
}

// BuildConnector selects a read-only connector and points its Markdown sink at
// the brain data directory. Credentials are resolved by the connectors from the
// environment/OS secret store at run time.
func BuildConnector(cfg config.BrainConfig, opts ConnectorOptions, buildOpts Options) (ports.Connector, error) {
	name := strings.ToLower(strings.TrimSpace(opts.Name))
	switch name {
	case github.Name, gitlab.Name, imap.Name:
	default:
		return nil, domain.NewValidationError("connector.name", "must be github, gitlab, or imap")
	}
	if cfg.Mode == config.ModeServer {
		return nil, fmt.Errorf("%w: connectors require local or git mode with data_dir", domain.ErrUnsupported)
	}
	dataDir := strings.TrimSpace(cfg.DataDir)
	if dataDir == "" {
		return nil, domain.NewValidationError("config.data_dir", "is required for connectors")
	}
	project := strings.TrimSpace(opts.Project)

	switch name {
	case github.Name:
		repos := envList("BRAIN_GITHUB_REPOS")
		if len(repos) == 0 && project != "" {
			repos = []string{project}
		}
		if len(repos) == 0 {
			return nil, domain.NewValidationError("github.repos", "set BRAIN_GITHUB_REPOS or pass --project owner/repository")
		}
		sink, err := connectors.NewDirSink(dataDir)
		if err != nil {
			return nil, err
		}
		return github.New(github.Config{
			BaseURL: strings.TrimSpace(os.Getenv("BRAIN_GITHUB_BASE_URL")),
			Repos:   repos,
			Secrets: selectSecrets(buildOpts),
			Sink:    sink,
		})
	case gitlab.Name:
		projects := envList("BRAIN_GITLAB_PROJECTS")
		if len(projects) == 0 && project != "" {
			projects = []string{project}
		}
		if len(projects) == 0 {
			return nil, domain.NewValidationError("gitlab.projects", "set BRAIN_GITLAB_PROJECTS or pass --project group/project")
		}
		sink, err := connectors.NewDirSink(dataDir)
		if err != nil {
			return nil, err
		}
		return gitlab.New(gitlab.Config{
			BaseURL:  strings.TrimSpace(os.Getenv("BRAIN_GITLAB_BASE_URL")),
			Projects: projects,
			Secrets:  selectSecrets(buildOpts),
			Sink:     sink,
		})
	case imap.Name:
		return buildIMAPConnector(cfg, dataDir, opts.Limit, buildOpts)
	default:
		return nil, domain.NewValidationError("connector.name", "must be github, gitlab, or imap")
	}
}

func buildIMAPConnector(cfg config.BrainConfig, dataDir string, limit int, buildOpts Options) (ports.Connector, error) {
	account := strings.TrimSpace(os.Getenv("BRAIN_IMAP_ACCOUNT"))
	if account == "" {
		return nil, domain.NewValidationError("imap.account", "set BRAIN_IMAP_ACCOUNT")
	}
	address := strings.TrimSpace(os.Getenv("BRAIN_IMAP_ADDRESS"))
	if address == "" {
		return nil, domain.NewValidationError("imap.address", "set BRAIN_IMAP_ADDRESS (host:port)")
	}
	username := strings.TrimSpace(os.Getenv("BRAIN_IMAP_USERNAME"))
	if username == "" {
		return nil, domain.NewValidationError("imap.username", "set BRAIN_IMAP_USERNAME")
	}
	statePath := strings.TrimSpace(os.Getenv("BRAIN_IMAP_STATE"))
	if statePath == "" {
		statePath = filepath.Join(dataDir, "connectors", "mail-state.json")
	}
	sink, err := connectors.NewDirSink(dataDir)
	if err != nil {
		return nil, err
	}
	return imap.New(imap.Config{
		Account:     account,
		Address:     address,
		Username:    username,
		PasswordKey: strings.TrimSpace(os.Getenv("BRAIN_IMAP_PASSWORD_KEY")),
		Secrets:     selectSecrets(buildOpts),
		Folders:     envList("BRAIN_IMAP_FOLDERS"),
		Sink:        sink,
		StatePath:   statePath,
		Limit:       limit,
	})
}

func envList(name string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			values = append(values, value)
		}
	}
	return values
}
