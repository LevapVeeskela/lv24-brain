// Package connectorbuild selects one read-only connector and wires the
// environment/OS secret store, output sink and IMAP state path. It is shared by
// the CLI (data-directory Markdown sink) and brain-server (discard sink plus a
// PostgreSQL run record).
package connectorbuild

import (
	"fmt"
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

// snapshotBuilders constructs the legacy Markdown-dump connectors. The
// provider names are still owned by the registry: Build resolves the name
// through Registry.Lookup, and this map only adds the snapshot behaviour that
// Registry.Build (an item Source) deliberately does not have.
//
// `brain connector run --name` must keep writing the dumps that
// `brain search --dumps` and existing users rely on. The native item path is
// `brain connection run <id>`; it is not a drop-in replacement for this
// command. Keep this shim for v0.9.0 and remove it only with a dump-compatible
// native path.
var snapshotBuilders = map[string]func(Options) (ports.Connector, error){
	github.Name: buildGitHubSnapshot,
	gitlab.Name: buildGitLabSnapshot,
	imap.Name:   buildIMAPSnapshot,
}

// snapshotProviders lists the providers that still have a legacy Markdown
// snapshot connector. New native-only sources are not available through
// `brain connector run --name`; use `brain connection run <id>`.
func snapshotProviders() []string {
	return []string{github.Name, gitlab.Name, imap.Name}
}

// SnapshotProviders returns the provider names accepted by the legacy
// snapshot command (`brain connector run --name`). It is separate from
// Registry.Providers(), which lists every native source.
func SnapshotProviders() []string {
	return snapshotProviders()
}

// Build selects a read-only Markdown-dump connector. Credentials are resolved
// by the connector at run time through Secrets; env configuration is read here
// so both the CLI and the server use the same names. Provider names come from
// the same registry as the native ingestion path; the snapshot constructors
// below preserve the legacy flags, environment and data-derived targets.
func Build(opts Options) (ports.Connector, error) {
	registry := Builtin()
	name := strings.ToLower(strings.TrimSpace(opts.Name))
	entry, err := registry.Lookup(name)
	if err != nil {
		return nil, domain.NewValidationError("connector.name", "must be "+providerChoices(snapshotProviders()))
	}
	if opts.Secrets == nil {
		return nil, domain.NewValidationError("connector.secrets", "is required")
	}
	if opts.Sink == nil {
		return nil, domain.NewValidationError("connector.sink", "is required")
	}
	build, ok := snapshotBuilders[entry.Descriptor.Provider]
	if !ok {
		return nil, domain.NewValidationError("connector.name", fmt.Sprintf("%s has no snapshot connector; use brain connection run", entry.Descriptor.Provider))
	}
	return build(opts)
}

func buildGitHubSnapshot(opts Options) (ports.Connector, error) {
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
}

func buildGitLabSnapshot(opts Options) (ports.Connector, error) {
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
}

func buildIMAPSnapshot(opts Options) (ports.Connector, error) {
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
}

// providerChoices renders "github, gitlab, or imap" from the registry so the
// error cannot drift from the registered providers.
func providerChoices(providers []string) string {
	switch len(providers) {
	case 0:
		return "registered provider"
	case 1:
		return providers[0]
	default:
		return strings.Join(providers[:len(providers)-1], ", ") + ", or " + providers[len(providers)-1]
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
