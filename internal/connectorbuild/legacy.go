package connectorbuild

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/levapveeskela/lv24-brain/internal/adapters/github"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitlab"
	"github.com/levapveeskela/lv24-brain/internal/adapters/imap"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// LegacySource runs a snapshot-style ports.Connector behind the Source
// contract until the provider is ported to item-level reads (WP0.6).
//
// Read runs the connector once: it writes its Markdown snapshot to the sink
// as a side effect and returns an empty batch with no checkpoints and More
// false, so a host loop treats it as a single, complete page. A failed run
// is returned as an error; partial and successful runs are not. The run
// record of the latest Read is available from LastRun.
type LegacySource struct {
	descriptor domain.SourceDescriptor
	connector  ports.Connector

	mu   sync.Mutex
	last *domain.ConnectorRun
}

// NewLegacySource wraps connector with descriptor.
func NewLegacySource(descriptor domain.SourceDescriptor, connector ports.Connector) (*LegacySource, error) {
	if err := descriptor.Validate(); err != nil {
		return nil, err
	}
	if connector == nil {
		return nil, domain.NewValidationError("legacy.connector", "is required")
	}
	return &LegacySource{descriptor: cloneDescriptor(descriptor), connector: connector}, nil
}

// Descriptor returns the wrapped provider's descriptor.
func (s *LegacySource) Descriptor() domain.SourceDescriptor { return cloneDescriptor(s.descriptor) }

// Check is unsupported: legacy connectors have no side-effect-free probe.
func (s *LegacySource) Check(context.Context) error {
	return fmt.Errorf("%w: %s connector has no credential probe yet", domain.ErrUnsupported, s.descriptor.Provider)
}

// Read runs the legacy connector once with Since and Limit from req.
func (s *LegacySource) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	run, err := s.connector.Run(ctx, ports.ConnectorOptions{Since: req.Since, Limit: req.Limit})
	if run.ConnectionID == "" && req.Connection.ID != "" {
		run.ConnectionID = req.Connection.ID
	}
	s.mu.Lock()
	s.last = &run
	s.mu.Unlock()
	if err != nil {
		return ports.ReadBatch{}, err
	}
	if run.Status == domain.ConnectorFailed {
		return ports.ReadBatch{}, errors.New(firstNonEmpty(run.Error, s.descriptor.Provider+" connector failed"))
	}
	return ports.ReadBatch{}, nil
}

// LastRun returns the run record of the latest Read, if any.
func (s *LegacySource) LastRun() (domain.ConnectorRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return domain.ConnectorRun{}, false
	}
	return *s.last, true
}

// Connector returns the wrapped connector for callers that still drive the
// legacy Name/Run API directly.
func (s *LegacySource) Connector() ports.Connector { return s.connector }

// Builtin returns a new registry holding the built-in providers.
func Builtin() *Registry {
	registry := NewRegistry()
	for _, entry := range []Registration{
		{Descriptor: GitHubDescriptor(), Factory: newLegacyGitHub},
		{Descriptor: GitLabDescriptor(), Factory: newLegacyGitLab},
		{Descriptor: IMAPDescriptor(), Factory: newLegacyIMAP},
	} {
		if err := registry.Register(entry.Descriptor, entry.Factory); err != nil {
			panic("connectorbuild: invalid built-in provider: " + err.Error())
		}
	}
	return registry
}

// GitHubDescriptor describes the GitHub provider. Capabilities describe the
// item contract; the legacy snapshot connector has no checkpoints yet.
func GitHubDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:       github.Name,
		DisplayName:    "GitHub",
		Capabilities:   domain.Capabilities{Backfill: true, AuthModes: []domain.AuthMode{domain.AuthToken, domain.AuthOAuthDevice}},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "repos", Label: "Repositories", Required: true, Help: "Comma-separated owner/name list."},
			{Key: "token", Label: "Token", Required: true, Secret: true, Help: "Personal access token with read access."},
		},
	}
}

// GitLabDescriptor describes the GitLab provider.
func GitLabDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:       gitlab.Name,
		DisplayName:    "GitLab",
		Capabilities:   domain.Capabilities{Backfill: true, AuthModes: []domain.AuthMode{domain.AuthToken, domain.AuthOAuthPKCE}},
		DefaultPublish: domain.PublishShared,
		ConfigFields: []domain.ConfigField{
			{Key: "projects", Label: "Projects", Required: true, Help: "Comma-separated namespace/project list."},
			{Key: "token", Label: "Token", Required: true, Secret: true, Help: "Personal access token with read_api scope."},
		},
	}
}

// IMAPDescriptor describes the IMAP provider. Mail is local-only by default.
func IMAPDescriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:       imap.Name,
		DisplayName:    "IMAP mail",
		Capabilities:   domain.Capabilities{Backfill: true, AuthModes: []domain.AuthMode{domain.AuthBasic}},
		DefaultPublish: domain.PublishLocalOnly,
		ConfigFields: []domain.ConfigField{
			{Key: "username", Label: "Username", Required: true},
			{Key: "folders", Label: "Folders", Help: "Comma-separated folder list; default INBOX."},
			{Key: "password", Label: "Password", Required: true, Secret: true, Help: "App password; stored in the OS keychain."},
		},
	}
}

func newLegacyGitHub(conn domain.Connection, deps Deps) (ports.Source, error) {
	if err := requireLegacyDeps(deps); err != nil {
		return nil, err
	}
	connector, err := github.New(github.Config{
		BaseURL:    conn.BaseURL,
		Repos:      splitList(conn.Settings["repos"]),
		Secrets:    deps.Secrets,
		Sink:       deps.Sink,
		HTTPClient: httpClient(deps),
		Now:        deps.Now,
	})
	if err != nil {
		return nil, err
	}
	return NewLegacySource(GitHubDescriptor(), connector)
}

func newLegacyGitLab(conn domain.Connection, deps Deps) (ports.Source, error) {
	if err := requireLegacyDeps(deps); err != nil {
		return nil, err
	}
	connector, err := gitlab.New(gitlab.Config{
		BaseURL:    conn.BaseURL,
		Projects:   splitList(conn.Settings["projects"]),
		Secrets:    deps.Secrets,
		Sink:       deps.Sink,
		HTTPClient: httpClient(deps),
		Now:        deps.Now,
	})
	if err != nil {
		return nil, err
	}
	return NewLegacySource(GitLabDescriptor(), connector)
}

func newLegacyIMAP(conn domain.Connection, deps Deps) (ports.Source, error) {
	if err := requireLegacyDeps(deps); err != nil {
		return nil, err
	}
	if strings.TrimSpace(conn.Account) == "" {
		return nil, domain.NewValidationError("imap.account", "is required")
	}
	address, err := imapAddress(conn.BaseURL)
	if err != nil {
		return nil, err
	}
	connector, err := imap.New(imap.Config{
		Account:   conn.Account,
		Address:   address,
		Username:  conn.Settings["username"],
		Secrets:   deps.Secrets,
		Folders:   splitList(conn.Settings["folders"]),
		Sink:      deps.Sink,
		StatePath: deps.StatePath,
		Now:       deps.Now,
	})
	if err != nil {
		return nil, err
	}
	return NewLegacySource(IMAPDescriptor(), connector)
}

func requireLegacyDeps(deps Deps) error {
	if deps.Secrets == nil {
		return domain.NewValidationError("connector.secrets", "is required")
	}
	if deps.Sink == nil {
		return domain.NewValidationError("connector.sink", "is required")
	}
	return nil
}

// imapAddress maps imaps://host[:port] to host:port (default 993).
func imapAddress(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Scheme, "imaps") {
		return "", domain.NewValidationError("imap.baseUrl", "must be imaps://host[:port]")
	}
	port := parsed.Port()
	if port == "" {
		port = "993"
	}
	host := parsed.Hostname()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + port, nil
}

func httpClient(deps Deps) *http.Client {
	if deps.HTTPClient == nil {
		return nil
	}
	return deps.HTTPClient()
}

func splitList(raw string) []string {
	return cleanList(strings.Split(raw, ","))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
