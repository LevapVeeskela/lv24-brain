// Package localapi implements the authenticated loopback control API used by
// the desktop process. It depends on narrow application-facing interfaces and
// deliberately does not import another adapter.
package localapi

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	DefaultMaxBodyBytes = 1 << 20
	MaxLogLineBytes     = 4096
	MaxLogLines         = 1000
)

// TokenProvider supplies the runtime bearer token. Implementations should
// return the token from the agent runtime record, never log it.
type TokenProvider interface{ Token() string }

// TokenFunc adapts a function to TokenProvider.
type TokenFunc func() string

func (f TokenFunc) Token() string { return f() }

// ConnectionService manages the secret-free connection catalogue.
type ConnectionService interface {
	List(context.Context) ([]domain.Connection, error)
	Get(context.Context, string) (domain.Connection, error)
	Create(context.Context, domain.Connection) error
	Update(context.Context, domain.Connection) error
	// Patch atomically applies mutate to the stored connection. The callback
	// runs under the store's write lock; returning an error aborts the change.
	Patch(context.Context, string, func(*domain.Connection) error) (domain.Connection, error)
	Delete(context.Context, string) error
}

// SourceRegistry provides descriptors for catalogue validation and creates a
// source checker for a configured connection.
type SourceRegistry interface {
	Descriptor(string) (domain.SourceDescriptor, error)
	Checker(context.Context, domain.Connection) (SourceChecker, error)
}

// SourceChecker performs a provider's non-mutating credential/config check.
type SourceChecker interface{ Check(context.Context) error }

// CredentialSetter is the only API path allowed to write a secret.
type CredentialSetter interface {
	Set(context.Context, string, string, string) error
}

// RunOptions are bounded options for a queued ingestion run.
type RunOptions struct {
	Since *time.Time `json:"since,omitempty"`
	Limit int        `json:"limit,omitempty"`
	Days  int        `json:"days,omitempty"`
}

// JobQueue queues a run and returns its job identifier. Busy errors should
// wrap domain.ErrConflict.
type JobQueue interface {
	Queue(context.Context, string, RunOptions) (string, error)
}

// RunView deliberately excludes error text and any credential-bearing fields.
type RunView struct {
	ID           string                 `json:"id"`
	Source       string                 `json:"source"`
	ConnectionID string                 `json:"connectionId,omitempty"`
	Status       domain.ConnectorStatus `json:"status"`
	StartedAt    time.Time              `json:"startedAt"`
	FinishedAt   *time.Time             `json:"finishedAt,omitempty"`
	Counts       map[string]int         `json:"counts,omitempty"`
	Stats        *domain.RunStats       `json:"stats,omitempty"`
}

// LogLine is one already-redacted line emitted for a run.
type LogLine struct{ Text string }

// RunLog contains a bounded producer. Close must release producer resources.
type RunLog interface {
	Lines() <-chan LogLine
	Close() error
}

// RunLister returns persisted run summaries and opens a cancellable log tail.
type RunLister interface {
	ListRuns(context.Context, int) ([]domain.ConnectorRun, error)
	GetRun(context.Context, string) (domain.ConnectorRun, error)
	OpenRunLog(context.Context, string) (RunLog, error)
}

type Schedule struct {
	ConnectionID string `json:"connectionId"`
	Expression   string `json:"schedule"`
}

// ScheduleStore persists scheduler configuration.
type ScheduleStore interface {
	ListSchedules(context.Context) ([]Schedule, error)
	SetSchedule(context.Context, string, string) error
}

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProjectLister exposes registered project names/ids only.
type ProjectLister interface {
	ListProjects(context.Context) ([]Project, error)
}

// SettingsStore holds only non-secret agent settings.
type SettingsStore interface {
	GetSettings(context.Context) (map[string]string, error)
	PutSettings(context.Context, map[string]string) error
}

type UpdateResult struct {
	Available bool   `json:"available"`
	Applied   bool   `json:"applied"`
	Version   string `json:"version,omitempty"`
}

// Updater checks and optionally applies an engine update after explicit user
// confirmation. Its result must not contain URLs, credentials, or errors.
type Updater interface {
	Update(context.Context) (UpdateResult, error)
}

// Shutdown requests graceful process shutdown.
type Shutdown interface{ RequestShutdown(context.Context) error }

// ShutdownFunc adapts a callback to Shutdown.
type ShutdownFunc func(context.Context) error

func (f ShutdownFunc) RequestShutdown(ctx context.Context) error { return f(ctx) }

type Options struct {
	Token       TokenProvider
	Port        int
	Version     string
	PID         int
	StartedAt   time.Time
	Now         func() time.Time
	MaxBody     int64
	Heartbeat   time.Duration
	Status      string
	Redact      func(string) string
	Connections ConnectionService
	Sources     SourceRegistry
	Credentials CredentialSetter
	Jobs        JobQueue
	Runs        RunLister
	Schedules   ScheduleStore
	Projects    ProjectLister
	Settings    SettingsStore
	Updater     Updater
	Shutdown    Shutdown
}

// Handler is the protected HTTP handler. Instantiate through NewHandler or
// NewServer so Host validation always has the exact listener port.
type Handler struct {
	opts Options
	mux  *http.ServeMux
}

// Server owns a loopback-only TCP listener and its HTTP server.
type Server struct {
	Listener net.Listener
	HTTP     *http.Server
	Handler  *Handler
}
