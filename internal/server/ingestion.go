package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/connectors"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// rawStore is the durable persistence a server connector run needs: the
// raw-zone item and checkpoint stores plus a cross-process connection lock.
// The PostgreSQL serverstore implements it; the in-memory httpapi store does
// not, so memory mode fails a run as unsupported instead of reporting
// success while discarding output.
type rawStore interface {
	ItemZone() ports.ItemStore
	CheckpointZone() ports.CheckpointStore
	ports.ConnectionLocker
}

// serverConnectionStore is the ephemeral, immutable, one-entry connection
// catalogue for a server run. The connection is synthesized per request from
// the server environment and the request targets (see buildServerConnection);
// it is never persisted and cannot be created, changed or deleted. The
// ingestion host only reads it via Get.
type serverConnectionStore struct {
	conn domain.Connection
}

var _ ports.ConnectionStore = (*serverConnectionStore)(nil)

// List returns the single ephemeral connection.
func (s *serverConnectionStore) List(ctx context.Context) ([]domain.Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []domain.Connection{s.conn}, nil
}

// Get returns the ephemeral connection or domain.ErrNotFound.
func (s *serverConnectionStore) Get(ctx context.Context, id string) (domain.Connection, error) {
	if err := ctx.Err(); err != nil {
		return domain.Connection{}, err
	}
	if id != s.conn.ID {
		return domain.Connection{}, domain.ErrNotFound
	}
	return s.conn, nil
}

// Create refuses: the ephemeral connection is read-only.
func (s *serverConnectionStore) Create(context.Context, domain.Connection) error {
	return domain.ErrUnsupported
}

// Update refuses: the ephemeral connection is immutable.
func (s *serverConnectionStore) Update(context.Context, domain.Connection) error {
	return domain.ErrUnsupported
}

// Patch refuses: the ephemeral connection is immutable.
func (s *serverConnectionStore) Patch(context.Context, string, func(*domain.Connection) error) (domain.Connection, error) {
	return domain.Connection{}, domain.ErrUnsupported
}

// Delete refuses: the ephemeral connection is never persisted.
func (s *serverConnectionStore) Delete(context.Context, string) error {
	return domain.ErrUnsupported
}

// serverCredentials resolves native-source credential fields from the legacy
// secret store without copying values anywhere or logging them. It maps the
// connection's token/password calls to the existing legacy secret names:
// github/gitlab "token" to "github/token" / "gitlab/token", IMAP "password"
// to BRAIN_IMAP_PASSWORD_KEY or "imap/<slug(account)>/password".
type serverCredentials struct {
	secrets      ports.SecretStore
	provider     string
	connectionID string
	passwordKey  string
}

var _ ports.CredentialProvider = serverCredentials{}

// newServerCredentials binds the legacy secret names for one ephemeral
// connection. account selects the default IMAP password key.
func newServerCredentials(secrets ports.SecretStore, provider, connectionID, account string) serverCredentials {
	creds := serverCredentials{secrets: secrets, provider: provider, connectionID: connectionID}
	if provider == "imap" {
		if key := strings.TrimSpace(os.Getenv("BRAIN_IMAP_PASSWORD_KEY")); key != "" {
			creds.passwordKey = key
		} else {
			creds.passwordKey = "imap/" + connectors.Slug(account) + "/password"
		}
	}
	return creds
}

// Get returns the legacy secret for the connection's credential field. Any
// other connection ID or field is domain.ErrNotFound; values are passed
// through and never stored or logged here.
func (c serverCredentials) Get(ctx context.Context, connectionID, field string) (string, error) {
	if c.secrets == nil {
		return "", domain.NewValidationError("server.credentials", "is required")
	}
	if connectionID == "" || connectionID != c.connectionID {
		return "", domain.ErrNotFound
	}
	var key string
	switch c.provider {
	case "github":
		if field != "token" {
			return "", domain.ErrNotFound
		}
		key = "github/token"
	case "gitlab":
		if field != "token" {
			return "", domain.ErrNotFound
		}
		key = "gitlab/token"
	case "imap":
		if field != "password" {
			return "", domain.ErrNotFound
		}
		key = c.passwordKey
	default:
		return "", domain.ErrNotFound
	}
	return c.secrets.Get(ctx, key)
}

// buildServerConnection synthesizes the ephemeral connection for one run from
// the server environment and the request, preserving the legacy precedence:
// explicit environment lists win, then the request targets. GitHub reads
// BRAIN_GITHUB_REPOS then req.Projects; GitLab reads BRAIN_GITLAB_PROJECTS
// then req.Projects; IMAP reads BRAIN_IMAP_ACCOUNT/ADDRESS/USERNAME/FOLDERS.
//
// The connection ID is a stable hash of provider, host, account and the
// sorted target set, so the same inputs always resume the same checkpoints
// while different target sets never share a cursor. When req.ProjectID is
// set the caller must have validated the project; it becomes the
// connection's only project so raw items are mapped to it. Without it items
// stay unmapped and server search excludes them.
func buildServerConnection(req httpapi.ConnectorRunRequest) (domain.Connection, error) {
	provider := strings.ToLower(strings.TrimSpace(req.Name))
	projectID := strings.TrimSpace(req.ProjectID)
	switch provider {
	case "github", "gitlab", "imap":
	default:
		return domain.Connection{}, domain.NewValidationError("connector.name", "must be github, gitlab, or imap")
	}
	var conn domain.Connection
	switch provider {
	case "github":
		repos := serverEnvList("BRAIN_GITHUB_REPOS")
		if len(repos) == 0 {
			repos = serverCleanList(req.Projects)
		}
		if len(repos) == 0 {
			return domain.Connection{}, domain.NewValidationError("github.repos", "set BRAIN_GITHUB_REPOS or pass a project filter")
		}
		conn = domain.Connection{
			Provider: "github",
			BaseURL:  strings.TrimSpace(os.Getenv("BRAIN_GITHUB_BASE_URL")),
			Settings: map[string]string{"repos": strings.Join(repos, ",")},
		}
		conn.ID = serverConnectionID("github", conn.BaseURL, "", repos, projectID)
	case "gitlab":
		projects := serverEnvList("BRAIN_GITLAB_PROJECTS")
		if len(projects) == 0 {
			projects = serverCleanList(req.Projects)
		}
		if len(projects) == 0 {
			return domain.Connection{}, domain.NewValidationError("gitlab.projects", "set BRAIN_GITLAB_PROJECTS or pass a project filter")
		}
		conn = domain.Connection{
			Provider: "gitlab",
			BaseURL:  strings.TrimSpace(os.Getenv("BRAIN_GITLAB_BASE_URL")),
			Settings: map[string]string{"projects": strings.Join(projects, ",")},
		}
		conn.ID = serverConnectionID("gitlab", conn.BaseURL, "", projects, projectID)
	case "imap":
		account := strings.TrimSpace(os.Getenv("BRAIN_IMAP_ACCOUNT"))
		address := strings.TrimSpace(os.Getenv("BRAIN_IMAP_ADDRESS"))
		username := strings.TrimSpace(os.Getenv("BRAIN_IMAP_USERNAME"))
		folders := serverEnvList("BRAIN_IMAP_FOLDERS")
		if account == "" {
			return domain.Connection{}, domain.NewValidationError("imap.account", "set BRAIN_IMAP_ACCOUNT")
		}
		if address == "" {
			return domain.Connection{}, domain.NewValidationError("imap.address", "set BRAIN_IMAP_ADDRESS (host:port)")
		}
		if username == "" {
			return domain.Connection{}, domain.NewValidationError("imap.username", "set BRAIN_IMAP_USERNAME")
		}
		settings := map[string]string{"username": username}
		if len(folders) > 0 {
			settings["folders"] = strings.Join(folders, ",")
		}
		conn = domain.Connection{
			Provider: "imap",
			Account:  account,
			BaseURL:  serverIMAPBaseURL(address),
			Settings: settings,
		}
		conn.ID = serverConnectionID("imap", conn.BaseURL, account, folders, projectID)
	}
	if projectID != "" {
		conn.Projects = []string{projectID}
	}
	if err := conn.Validate(); err != nil {
		return domain.Connection{}, err
	}
	return conn, nil
}

// newIngestion builds the item-level host for one ephemeral server
// connection. Native registry sources read credentials through the legacy
// secret adapter; no legacy Secrets sink or Markdown sink is wired, so
// sources can only persist through the raw-zone stores and the host stores
// items before checkpoints. Runs are left unset: the runner persists the run
// record itself, which keeps upstream failures on the record while
// persistence failures surface as errors.
func (r *connectorRunner) newIngestion(raw rawStore, conn domain.Connection) *app.Ingestion {
	registry := r.registry
	if registry == nil {
		registry = connectorbuild.Builtin()
	}
	return &app.Ingestion{
		Connections: &serverConnectionStore{conn: conn},
		Registry:    registry,
		Deps:        connectorbuild.Deps{Credentials: newServerCredentials(r.secretStore, conn.Provider, conn.ID, conn.Account)},
		Items:       raw.ItemZone(),
		Checkpoints: raw.CheckpointZone(),
		Locker:      raw,
		Index:       nil,
		Runs:        nil,
	}
}

// serverConnectionID hashes provider, host, account, brain project mapping and
// the sorted target set into a stable connection ID. Different target sets or
// project mappings hash differently, so checkpoints (cursors) and item
// attribution are never shared across different selections.
func serverConnectionID(provider, baseURL, account string, targets []string, projectID string) string {
	sorted := append([]string(nil), targets...)
	sort.Strings(sorted)
	sum := sha256.New()
	sum.Write([]byte(provider))
	sum.Write([]byte{0})
	sum.Write([]byte(strings.TrimSpace(baseURL)))
	sum.Write([]byte{0})
	sum.Write([]byte(strings.ToLower(strings.TrimSpace(account))))
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		sum.Write([]byte{0})
		sum.Write([]byte(projectID))
	}
	for _, target := range sorted {
		sum.Write([]byte{0})
		sum.Write([]byte(target))
	}
	return provider + "-" + hex.EncodeToString(sum.Sum(nil))[:32]
}

// serverIMAPBaseURL maps a host:port address to an imaps:// URL for the
// connection endpoint. Values that already carry a scheme pass through.
func serverIMAPBaseURL(address string) string {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" || strings.Contains(trimmed, "://") {
		return trimmed
	}
	return "imaps://" + trimmed
}

// serverEnvList splits a comma-separated environment variable.
func serverEnvList(name string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	return serverCleanList(strings.Split(raw, ","))
}

// serverCleanList trims and drops empty entries.
func serverCleanList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
