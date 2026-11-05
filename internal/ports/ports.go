package ports

import (
	"context"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Store persists brain records. Implementations decide whether the backing
// storage is Markdown files, git, or a server API.
type Store interface {
	GetProject(ctx context.Context, id string) (domain.Project, error)
	PutProject(ctx context.Context, project domain.Project) error
	ListProjects(ctx context.Context) ([]domain.Project, error)

	GetState(ctx context.Context, projectID string) (domain.State, error)
	PutState(ctx context.Context, state domain.State) (domain.SyncRevision, error)

	AppendNote(ctx context.Context, note domain.Note) (domain.SyncRevision, error)
	AppendDecision(ctx context.Context, decision domain.Decision) (domain.SyncRevision, error)
	AppendAttempt(ctx context.Context, attempt domain.Attempt) (domain.SyncRevision, error)
	AppendHandoff(ctx context.Context, handoff domain.Handoff) (domain.SyncRevision, error)

	CurrentRevision(ctx context.Context) (domain.SyncRevision, error)
}

// Document is the indexed projection of one knowledge record.
type Document struct {
	ID        string
	ProjectID string
	Kind      string
	Title     string
	Body      string
	UpdatedAt time.Time
}

// Query narrows a full-text search.
type Query struct {
	Text      string
	ProjectID string
	Limit     int
}

// Hit is one search result.
type Hit struct {
	ID        string
	ProjectID string
	Kind      string
	Title     string
	Snippet   string
	Score     float64
}

// Index provides full-text search over stored knowledge.
type Index interface {
	Upsert(ctx context.Context, doc Document) error
	Delete(ctx context.Context, id string) error
	Search(ctx context.Context, query Query) ([]Hit, error)
}

// SyncStatus is the read-only divergence state of a sync provider.
type SyncStatus struct {
	Provider  string
	Branch    string
	Ahead     int
	Behind    int
	Dirty     bool
	Conflicts []string
}

// SyncOptions controls a pull/commit/push cycle.
type SyncOptions struct {
	Push          bool
	CommitMessage string
}

// SyncResult reports what a sync step changed.
type SyncResult struct {
	Revision  domain.SyncRevision
	Changed   int
	Conflicts []string
}

// SyncProvider synchronizes the local brain with a git remote or server.
type SyncProvider interface {
	Status(ctx context.Context) (SyncStatus, error)
	Pull(ctx context.Context, opts SyncOptions) (SyncResult, error)
	Commit(ctx context.Context, message string) (domain.SyncRevision, error)
	Push(ctx context.Context) (SyncResult, error)
}

// SecretStore keeps connector credentials outside the data repository.
type SecretStore interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
	Names(ctx context.Context) ([]string, error)
}

// ConnectorOptions limits one connector run.
type ConnectorOptions struct {
	Projects []string
	Since    *time.Time
	Limit    int
}

// Connector imports metadata from an external source.
type Connector interface {
	Name() string
	Run(ctx context.Context, opts ConnectorOptions) (domain.ConnectorRun, error)
}

// ConnectorRunStore persists connector run records for review and audit.
// Local and Git modes write Markdown under connectors/runs. There is no
// server-side run store yet: server mode returns a nil ConnectorRunStore.
type ConnectorRunStore interface {
	AppendConnectorRun(ctx context.Context, run domain.ConnectorRun) error
	// ListConnectorRuns returns runs newest first. An empty source means every
	// source; a limit of zero selects the adapter default.
	ListConnectorRuns(ctx context.Context, source string, limit int) ([]domain.ConnectorRun, error)
}

// Credentials are local or OIDC login inputs.
type Credentials struct {
	Login    string
	Password string
}

// Session is an authenticated principal with rotating tokens.
type Session struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	User         domain.User
}

// AuthProvider authenticates users in server mode.
type AuthProvider interface {
	Login(ctx context.Context, credentials Credentials) (Session, error)
	Refresh(ctx context.Context, refreshToken string) (Session, error)
	Logout(ctx context.Context, sessionID string) error
	Principal(ctx context.Context, accessToken string) (domain.User, error)
}

// ReleaseInfo describes one downloadable engine release.
type ReleaseInfo struct {
	Version     string
	Channel     string
	URL         string
	SHA256      string
	PublishedAt time.Time
}

// Updater checks, verifies, stages and replaces the engine binary.
type Updater interface {
	Check(ctx context.Context, channel string) (ReleaseInfo, error)
	Download(ctx context.Context, release ReleaseInfo) (string, error)
	Apply(ctx context.Context, artifactPath string) error
	Rollback(ctx context.Context) error
}
