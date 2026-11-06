// Package connectorbuild selects one read-only connector and wires the
// environment/OS secret store, output sink and IMAP state path. It is shared by
// the CLI (data-directory Markdown sink) and brain-server (discard sink plus a
// PostgreSQL run record).
package connectorbuild

import (
	"os"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/github"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitlab"
	"github.com/levapveeskela/lv24-brain/internal/adapters/imap"
	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Options selects one connector and its environment-independent inputs.
type Options struct {
	Name     string
	Projects []string
	Limit    int
	Secrets  ports.SecretStore
	Sink     connectors.MarkdownSink
	// StatePath is the IMAP cursor file. BRAIN_IMAP_STATE wins when set; an
	// empty value keeps the IMAP connector stateless.
	StatePath string
	// Repos and GitLabProjects are data-derived defaults (registry, git
	// remotes) used when neither the BRAIN_* environment nor a project filter
	// selects targets.
	Repos          []string
	GitLabProjects []string
	// IMAP is a data-derived account (connectors/mail.json) used when
	// BRAIN_IMAP_ACCOUNT is unset.
	IMAP *IMAPAccount
}

// IMAPAccount is one mailbox to read.
type IMAPAccount struct {
	Account  string
	Address  string
	Username string
	Folders  []string
}

// Build selects a read-only connector. Credentials are resolved by the
// connector at run time through Secrets; env configuration is read here so
// both the CLI and the server use the same names.
func Build(opts Options) (ports.Connector, error) {
	name := strings.ToLower(strings.TrimSpace(opts.Name))
	switch name {
	case github.Name, gitlab.Name, imap.Name:
	default:
		return nil, domain.NewValidationError("connector.name", "must be github, gitlab, or imap")
	}
	if opts.Secrets == nil {
		return nil, domain.NewValidationError("connector.secrets", "is required")
	}
	if opts.Sink == nil {
		return nil, domain.NewValidationError("connector.sink", "is required")
	}

	switch name {
	case github.Name:
		repos := envList("BRAIN_GITHUB_REPOS")
		if len(repos) == 0 {
			repos = cleanList(opts.Projects)
		}
		if len(repos) == 0 {
			repos = cleanList(opts.Repos)
		}
		if len(repos) == 0 {
			return nil, domain.NewValidationError("github.repos", "set BRAIN_GITHUB_REPOS, pass a project filter, or register projects with GitHub repo URLs")
		}
		return github.New(github.Config{
			BaseURL: strings.TrimSpace(os.Getenv("BRAIN_GITHUB_BASE_URL")),
			Repos:   repos,
			Secrets: opts.Secrets,
			Sink:    opts.Sink,
		})
	case gitlab.Name:
		projects := envList("BRAIN_GITLAB_PROJECTS")
		if len(projects) == 0 {
			projects = cleanList(opts.Projects)
		}
		if len(projects) == 0 {
			projects = cleanList(opts.GitLabProjects)
		}
		if len(projects) == 0 {
			return nil, domain.NewValidationError("gitlab.projects", "set BRAIN_GITLAB_PROJECTS, pass a project filter, or add gitlab.com remotes to registered checkouts")
		}
		return gitlab.New(gitlab.Config{
			BaseURL:  strings.TrimSpace(os.Getenv("BRAIN_GITLAB_BASE_URL")),
			Projects: projects,
			Secrets:  opts.Secrets,
			Sink:     opts.Sink,
		})
	case imap.Name:
		account := strings.TrimSpace(os.Getenv("BRAIN_IMAP_ACCOUNT"))
		address := strings.TrimSpace(os.Getenv("BRAIN_IMAP_ADDRESS"))
		username := strings.TrimSpace(os.Getenv("BRAIN_IMAP_USERNAME"))
		folders := envList("BRAIN_IMAP_FOLDERS")
		if account == "" && opts.IMAP != nil {
			account = strings.TrimSpace(opts.IMAP.Account)
			address = strings.TrimSpace(opts.IMAP.Address)
			username = strings.TrimSpace(opts.IMAP.Username)
			folders = cleanList(opts.IMAP.Folders)
		}
		if account == "" {
			return nil, domain.NewValidationError("imap.account", "set BRAIN_IMAP_ACCOUNT or pass --account from connectors/mail.json")
		}
		if address == "" {
			return nil, domain.NewValidationError("imap.address", "set BRAIN_IMAP_ADDRESS (host:port)")
		}
		if username == "" {
			return nil, domain.NewValidationError("imap.username", "set BRAIN_IMAP_USERNAME")
		}
		statePath := strings.TrimSpace(os.Getenv("BRAIN_IMAP_STATE"))
		if statePath == "" {
			statePath = strings.TrimSpace(opts.StatePath)
		}
		return imap.New(imap.Config{
			Account:     account,
			Address:     address,
			Username:    username,
			PasswordKey: strings.TrimSpace(os.Getenv("BRAIN_IMAP_PASSWORD_KEY")),
			Secrets:     opts.Secrets,
			Folders:     folders,
			Sink:        opts.Sink,
			StatePath:   statePath,
			Limit:       opts.Limit,
		})
	default:
		return nil, domain.NewValidationError("connector.name", "must be github, gitlab, or imap")
	}
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

func cleanList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
