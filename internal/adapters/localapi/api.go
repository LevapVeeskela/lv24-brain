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

// JobCanceler cancels a queued or running job. It returns the status observed
// before cancellation ("queued" or "running"). An unknown or expired job
// returns domain.ErrNotFound; a job that already finished returns
// domain.ErrConflict.
type JobCanceler interface {
	CancelJob(context.Context, string) (string, error)
}

// RunView deliberately excludes error text and any credential-bearing fields.
type RunView struct {
	ID           string `json:"id"`
	Source       string `json:"source"`
	ConnectionID string `json:"connectionId,omitempty"`
	// ConnectionIDSnakeCase mirrors connectionId for the snake_case memory
	// contract; both are the same value.
	ConnectionIDSnakeCase string                 `json:"connection_id,omitempty"`
	ProjectIDs            []string               `json:"project_ids,omitempty"`
	Status                domain.ConnectorStatus `json:"status"`
	StartedAt             time.Time              `json:"startedAt"`
	FinishedAt            *time.Time             `json:"finishedAt,omitempty"`
	Counts                map[string]int         `json:"counts,omitempty"`
	Stats                 *domain.RunStats       `json:"stats,omitempty"`
	// Actor is the trigger recorded on the run. Run records do not persist one
	// today, so the field is omitted rather than guessed; it is reserved for a
	// future run schema.
	Actor string `json:"actor,omitempty"`
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
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Paths          []string          `json:"paths,omitempty"`
	PathProvenance map[string]string `json:"path_provenance,omitempty"`
}

// ProjectLister exposes registered project names/ids only.
type ProjectLister interface {
	ListProjects(context.Context) ([]Project, error)
}

// MergeRecordView is one record a merge plan touches.
type MergeRecordView struct {
	Kind  string `json:"kind,omitempty"`
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	// State is "live" for a record the merge absorbs, or "tombstone" for one
	// that was already merged.
	State string `json:"state,omitempty"`
}

// MergeLinkView is one reference a merge rewrites from a duplicate to the
// survivor.
type MergeLinkView struct {
	SourceKind string `json:"source_kind,omitempty"`
	SourceID   string `json:"source_id,omitempty"`
	Field      string `json:"field,omitempty"`
	Ref        string `json:"ref,omitempty"`
	OldID      string `json:"old_id,omitempty"`
	NewID      string `json:"new_id,omitempty"`
}

// MergePlan is phase one of a merge: the records, the conflicts that would
// refuse an apply and the fingerprint the apply re-verifies. Files are only
// known after an apply, so the plan carries no file list.
type MergePlan struct {
	// PlanID is the opaque token to pass to /v1/merge/apply.
	PlanID        string            `json:"plan_id"`
	Project       string            `json:"project"`
	Kind          string            `json:"kind,omitempty"`
	Survivor      MergeRecordView   `json:"survivor"`
	Duplicates    []MergeRecordView `json:"duplicates"`
	AlreadyMerged []MergeRecordView `json:"already_merged,omitempty"`
	Links         []MergeLinkView   `json:"links,omitempty"`
	MergedFrom    []string          `json:"merged_from,omitempty"`
	Conflicts     []string          `json:"conflicts"`
	Fingerprint   string            `json:"fingerprint"`
	// Changed is false when there is nothing left to merge.
	Changed bool `json:"changed"`
}

// MergePlanRequest selects one two-phase merge. ProjectID is the canonical
// project id resolved by the handler.
type MergePlanRequest struct {
	ProjectID  string
	Survivor   string
	Duplicates []string
}

// MergeApplyRequest applies a plan token. AllowedProjects is the handler's
// allowlist; an empty list serves every project. The service refuses a token
// whose project is outside it.
type MergeApplyRequest struct {
	PlanID          string
	AllowedProjects []string
}

// MergeRollbackRequest restores one applied merge. AllowedProjects has the
// same meaning as in MergeApplyRequest.
type MergeRollbackRequest struct {
	PlanID          string
	AllowedProjects []string
}

// MergeFileView is one file an apply or rollback touched, relative to the data
// directory.
type MergeFileView struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

// MergeResult is the receipt of an apply. PlanID is the token to pass to
// /v1/merge/rollback once a manifest exists.
type MergeResult struct {
	PlanID   string          `json:"plan_id"`
	Project  string          `json:"project"`
	Applied  bool            `json:"applied"`
	Manifest string          `json:"manifest,omitempty"`
	Files    []MergeFileView `json:"files"`
}

// MergeRollbackResult is the restored state of one merge.
type MergeRollbackResult struct {
	PlanID            string          `json:"plan_id"`
	Manifest          string          `json:"manifest"`
	Files             []MergeFileView `json:"files"`
	Conflicts         []string        `json:"conflicts,omitempty"`
	AlreadyRolledBack bool            `json:"already_rolled_back"`
	Reindexed         bool            `json:"reindexed"`
}

// MergeService plans, applies and rolls back two-phase merges. A stale plan
// returns an error whose Code is "stale_plan"; the handler maps it to 409
// merge_stale.
type MergeService interface {
	PlanMerge(context.Context, MergePlanRequest) (MergePlan, error)
	ApplyMerge(context.Context, MergeApplyRequest) (MergeResult, error)
	RollbackMerge(context.Context, MergeRollbackRequest) (MergeRollbackResult, error)
}

// DigestRequest selects a digest. ProjectID is the canonical project id
// resolved by the handler; empty means every project this API serves. Write is
// false for a dry run.
type DigestRequest struct {
	ProjectID string
	Write     bool
}

// DigestDecisionView is one proposed decision of a digest.
type DigestDecisionView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// DigestProjectView is one project's part of a digest.
type DigestProjectView struct {
	Project           Project              `json:"project"`
	Changes           int                  `json:"changes"`
	Notes             int                  `json:"notes"`
	Decisions         int                  `json:"decisions"`
	Attempts          int                  `json:"attempts"`
	Handoffs          int                  `json:"handoffs"`
	Commits           int                  `json:"commits"`
	LatestHandoff     *DigestHandoffView   `json:"latest_handoff,omitempty"`
	ProposedDecisions []DigestDecisionView `json:"proposed_decisions"`
	OpenQuestions     []string             `json:"open_questions"`
	Blocked           []string             `json:"blocked"`
	Findings          map[string]int       `json:"findings,omitempty"`
}

// DigestHandoffView is the latest handoff of one digest project.
type DigestHandoffView struct {
	ID         string    `json:"id"`
	Summary    string    `json:"summary"`
	NextAction string    `json:"next_action,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// DigestView is the morning summary across projects.
type DigestView struct {
	GeneratedAt time.Time           `json:"generated_at"`
	Since       time.Time           `json:"since"`
	DryRun      bool                `json:"dry_run"`
	WrittenTo   string              `json:"written_to,omitempty"`
	Projects    []DigestProjectView `json:"projects"`
}

// DigestService builds the morning digest; Write persists its Markdown form.
type DigestService interface {
	Digest(context.Context, DigestRequest) (DigestView, error)
}

// PathOwnershipView classifies one registered checkout path for this machine.
type PathOwnershipView struct {
	Path    string `json:"path"`
	Machine string `json:"machine,omitempty"`
	State   string `json:"state"`
}

// ProjectOwnershipView groups the path classification of one project.
type ProjectOwnershipView struct {
	ProjectID string              `json:"project_id"`
	Paths     []PathOwnershipView `json:"paths"`
}

// AdoptReportView is the result of a path-provenance adoption run.
type AdoptReportView struct {
	Machine  string                 `json:"machine"`
	Applied  bool                   `json:"applied"`
	Stamped  int                    `json:"stamped"`
	Owned    int                    `json:"owned"`
	Missing  int                    `json:"missing"`
	Projects []ProjectOwnershipView `json:"projects"`
}

// AddProjectPathRequest registers one checkout path on a project. ProjectID is
// the canonical project id resolved by the handler.
type AddProjectPathRequest struct {
	ProjectID string
	Path      string
}

// ProjectAdminService manages machine path provenance and registered paths.
type ProjectAdminService interface {
	AdoptLocalPaths(context.Context, bool) (AdoptReportView, error)
	AddProjectPath(context.Context, AddProjectPathRequest) (Project, error)
}

// JournalQuery selects one journal page. ProjectID empty matches every
// project; the handler enforces the project allowlist.
type JournalQuery struct {
	ProjectID string
	Limit     int
}

// JournalPage is a bounded, newest-first journal page.
type JournalPage struct {
	Items   []JournalView `json:"items"`
	Limit   int           `json:"limit"`
	HasMore bool          `json:"has_more"`
}

// JournalService reads the per-machine activity journal.
type JournalService interface {
	Journal(context.Context, JournalQuery) (JournalPage, error)
}

// HookStatus is one agent host as reported by the hook status builder.
type HookStatus struct {
	Host       string  `json:"host"`
	Path       string  `json:"path"`
	Installed  bool    `json:"installed"`
	Detail     string  `json:"detail"`
	Trusted    *bool   `json:"trusted"`
	Trust      string  `json:"trust,omitempty"`
	TrustHash  string  `json:"trust_hash,omitempty"`
	TrustDrift bool    `json:"trust_drift,omitempty"`
	Approve    string  `json:"approve,omitempty"`
	LastRun    *string `json:"last_run"`
	LastEvent  string  `json:"last_event,omitempty"`
	LastError  string  `json:"last_error,omitempty"`
}

// HookStatusService reports the installed state of the brain agent hooks.
type HookStatusService interface {
	Hooks(context.Context) ([]HookStatus, error)
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
	Cancels     JobCanceler
	Runs        RunLister
	Schedules   ScheduleStore
	Projects    ProjectLister
	Settings    SettingsStore
	Updater     Updater
	Shutdown    Shutdown
	// Memory is the curated record, search, state, review, proposal and
	// catchup surface. It is optional so an engine without those use cases
	// keeps serving the connection routes and reports memory routes as
	// unavailable.
	Memory MemoryService
	// Ideas is the agent-first TODO surface. It is optional: a build without
	// the idea use cases reports the ideas routes as unavailable.
	Ideas IdeaService
	// DataDir is the brain data directory. Problem-row paths are rendered
	// relative to it when it is set; an absolute path is returned otherwise.
	DataDir string
	// Sync exposes the git status and one sync action. It is optional: local
	// mode without a sync provider reports the route as unavailable.
	Sync SyncService
	// Merge exposes the two-phase merge routes. It is optional: a build
	// without a record merger reports them as unavailable.
	Merge MergeService
	// Digest exposes the morning digest route.
	Digest DigestService
	// ProjectAdmin exposes path adoption and path registration.
	ProjectAdmin ProjectAdminService
	// Journal exposes the activity journal page.
	Journal JournalService
	// Hooks exposes the agent hook host status.
	Hooks HookStatusService
	// ProjectAllowlist limits every memory route to these resolved project
	// ids. Empty serves every registered project. A non-empty list refuses an
	// all-projects read unless it holds exactly one project: spanning several
	// projects needs an explicit scope.
	ProjectAllowlist []string
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
