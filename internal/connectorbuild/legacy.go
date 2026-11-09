package connectorbuild

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/levapveeskela/lv24-brain/internal/adapters/confluence"
	"github.com/levapveeskela/lv24-brain/internal/adapters/dbschema"
	"github.com/levapveeskela/lv24-brain/internal/adapters/feeds"
	"github.com/levapveeskela/lv24-brain/internal/adapters/folders"
	"github.com/levapveeskela/lv24-brain/internal/adapters/github"
	"github.com/levapveeskela/lv24-brain/internal/adapters/gitlab"
	"github.com/levapveeskela/lv24-brain/internal/adapters/imap"
	"github.com/levapveeskela/lv24-brain/internal/adapters/jira"
	"github.com/levapveeskela/lv24-brain/internal/adapters/linear"
	"github.com/levapveeskela/lv24-brain/internal/adapters/notion"
	"github.com/levapveeskela/lv24-brain/internal/adapters/sentry"
	"github.com/levapveeskela/lv24-brain/internal/adapters/slack"
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
		{Descriptor: GitHubDescriptor(), Factory: newNativeGitHub},
		{Descriptor: GitLabDescriptor(), Factory: newNativeGitLab},
		{Descriptor: IMAPDescriptor(), Factory: newNativeIMAP},
		{Descriptor: folders.Descriptor(), Factory: newNativeFolders},
		{Descriptor: jira.Descriptor(), Factory: newNativeJira},
		{Descriptor: confluence.Descriptor(), Factory: newNativeConfluence},
		{Descriptor: linear.Descriptor(), Factory: newNativeLinear},
		{Descriptor: slack.Descriptor(), Factory: newNativeSlack},
		{Descriptor: notion.Descriptor(), Factory: newNativeNotion},
		{Descriptor: sentry.Descriptor(), Factory: newNativeSentry},
		{Descriptor: dbschema.PostgresDescriptor(), Factory: newNativePostgresSchema},
		{Descriptor: dbschema.SQLiteDescriptor(), Factory: newNativeSQLiteSchema},
		{Descriptor: feeds.RSSDescriptor(), Factory: newNativeRSS},
		{Descriptor: feeds.WebDescriptor(), Factory: newNativeWeb},
	} {
		if err := registry.Register(entry.Descriptor, entry.Factory); err != nil {
			panic("connectorbuild: invalid built-in provider: " + err.Error())
		}
	}
	return registry
}

// GitHubDescriptor describes the native GitHub provider.
func GitHubDescriptor() domain.SourceDescriptor { return github.Descriptor() }

// GitLabDescriptor describes the native GitLab provider.
func GitLabDescriptor() domain.SourceDescriptor { return gitlab.Descriptor() }

// IMAPDescriptor describes the native IMAP provider (local-only by default).
func IMAPDescriptor() domain.SourceDescriptor { return imap.Descriptor() }

func newNativeGitHub(conn domain.Connection, deps Deps) (ports.Source, error) {
	return github.NewSource(conn, deps.Credentials, httpClient(deps), deps.Now)
}

func newNativeGitLab(conn domain.Connection, deps Deps) (ports.Source, error) {
	return gitlab.NewSource(conn, deps.Credentials, httpClient(deps), deps.Now)
}

func newNativeIMAP(conn domain.Connection, deps Deps) (ports.Source, error) {
	return imap.NewSource(conn, deps.Credentials, nil, deps.Now)
}

func newNativeFolders(conn domain.Connection, deps Deps) (ports.Source, error) {
	return folders.NewSource(conn, deps.Now)
}

func newNativeJira(conn domain.Connection, deps Deps) (ports.Source, error) {
	return jira.NewSource(conn, deps.Credentials, httpClient(deps), deps.Now)
}

func newNativeConfluence(conn domain.Connection, deps Deps) (ports.Source, error) {
	return confluence.NewSource(conn, deps.Credentials, httpClient(deps), deps.Now)
}

func newNativeLinear(conn domain.Connection, deps Deps) (ports.Source, error) {
	return linear.NewSource(conn, deps.Credentials, httpClient(deps), deps.Now)
}

func newNativeSlack(conn domain.Connection, deps Deps) (ports.Source, error) {
	return slack.NewSource(conn, deps.Credentials, httpClient(deps), deps.Now)
}

func newNativeNotion(conn domain.Connection, deps Deps) (ports.Source, error) {
	return notion.NewSource(conn, deps.Credentials, httpClient(deps), deps.Now)
}

func newNativeSentry(conn domain.Connection, deps Deps) (ports.Source, error) {
	return sentry.NewSource(conn, deps.Credentials, httpClient(deps), deps.Now)
}

func newNativePostgresSchema(conn domain.Connection, deps Deps) (ports.Source, error) {
	return dbschema.NewPostgresSource(conn, deps.Credentials, deps.Now)
}

func newNativeSQLiteSchema(conn domain.Connection, deps Deps) (ports.Source, error) {
	return dbschema.NewSQLiteSource(conn, deps.Now)
}

func newNativeRSS(conn domain.Connection, deps Deps) (ports.Source, error) {
	return feeds.NewRSS(conn, httpClient(deps))
}

func newNativeWeb(conn domain.Connection, deps Deps) (ports.Source, error) {
	return feeds.NewWeb(conn, httpClient(deps))
}

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
