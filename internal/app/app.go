// Package app holds the brain use cases. Adapters are supplied at bootstrap;
// this package depends only on the domain, ports, and configuration contracts.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/version"
)

// ErrNotImplemented marks use cases whose feature has not landed yet.
var ErrNotImplemented = errors.New("not implemented")

// NotImplementedError names the missing feature.
type NotImplementedError struct {
	Feature string
}

func (e *NotImplementedError) Error() string {
	return fmt.Sprintf("%s: %v", e.Feature, ErrNotImplemented)
}

func (e *NotImplementedError) Unwrap() error { return ErrNotImplemented }

// DefaultBootstrapBudget bounds the generated context in runes.
const DefaultBootstrapBudget = 4000

// Dependencies are adapter implementations selected at bootstrap.
type Dependencies struct {
	Store ports.Store
	Index ports.Index
	// Searcher runs search against the server in server mode, where no local
	// index is built. Index stays nil there.
	Searcher Searcher
	// Journal records per-user activity in local and git modes; nil in
	// server mode.
	Journal ports.Journal
	Sync    ports.SyncProvider
	Secrets ports.SecretStore
	Auth    ports.AuthProvider
	Updater ports.Updater
	// Connections, Items and Proposals back the local raw zone and derived
	// proposal zone. All three are optional: adapters without them keep their
	// current behavior, and proposal use cases report that the feature is not
	// available instead of panicking.
	Connections   ports.ConnectionStore
	Items         ports.ItemStore
	Proposals     ports.ProposalStore
	ContextGrants ports.ContextGrantStore
	Now           func() time.Time
}

// App executes brain use cases.
type App struct {
	cfg  config.BrainConfig
	deps Dependencies
}

// New builds an App. Adapters may be nil until their feature is wired.
func New(cfg config.BrainConfig, deps Dependencies) *App {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &App{cfg: cfg, deps: deps}
}

// InitOptions configures a new brain instance.
type InitOptions struct {
	ConfigPath string
	Mode       string
	DataDir    string
	ServerURL  string
}

// InitResult reports what init created.
type InitResult struct {
	ConfigPath string
	DataDir    string
	Mode       string
}

// DoctorOptions configures compatibility checks.
type DoctorOptions struct {
	// ConfigPath is the brain.yaml path used for the run. Empty skips the file
	// existence probe and checks the in-memory configuration only.
	ConfigPath string
}

// DoctorCheck is one compatibility probe.
type DoctorCheck struct {
	Name    string
	Status  string
	Message string
}

// DoctorReport is the aggregated doctor result.
type DoctorReport struct {
	Checks []DoctorCheck
}

// OpenOptions selects the active project.
type OpenOptions struct {
	ProjectID string
}

// OpenResult is the opened project context.
type OpenResult struct {
	Project domain.Project
	State   domain.State
}

// BootstrapOptions selects the project to bootstrap.
type BootstrapOptions struct {
	ProjectID string
	// Budget bounds the summary in runes; zero selects DefaultBootstrapBudget.
	Budget int
}

// BootstrapResult summarizes generated context.
type BootstrapResult struct {
	ProjectID string
	Summary   string
}

// NoteInput is a new note.
type NoteInput struct {
	ProjectID string
	Title     string
	Body      string
	Tags      []string
	// ID optionally pins the record id; empty selects the dated default.
	// Promotion uses a deterministic id derived from the proposal.
	ID string
	// Source records the external item a promoted note came from.
	Source *domain.SourceRef
	// CreateProject registers ProjectID when no project, alias or path
	// matches; without it an unknown project is an error.
	CreateProject bool
}

// NoteResult identifies the appended note.
type NoteResult struct {
	Note     domain.Note
	Revision domain.SyncRevision
}

// DecisionInput is a new decision record.
type DecisionInput struct {
	ProjectID    string
	Title        string
	Status       domain.DecisionStatus
	Context      string
	Alternatives []string
	Consequences []string
	Evidence     []string
	// Paths are repo-relative files or directories the decision governs.
	Paths []string
	// ID optionally pins the record id; empty selects the dated default.
	ID string
	// Source records the external item a promoted decision came from.
	Source *domain.SourceRef
	// CreateProject registers ProjectID when no project, alias or path
	// matches; without it an unknown project is an error.
	CreateProject bool
}

// DecisionResult identifies the appended decision.
type DecisionResult struct {
	Decision domain.Decision
	Revision domain.SyncRevision
}

// AttemptInput is a new attempt record.
type AttemptInput struct {
	ProjectID  string
	Title      string
	Hypothesis string
	Result     string
	Reason     string
	// ID optionally pins the record id; empty selects the dated default.
	ID string
	// Source records the external item a promoted attempt came from.
	Source *domain.SourceRef
	// CreateProject registers ProjectID when no project, alias or path
	// matches; without it an unknown project is an error.
	CreateProject bool
}

// AttemptResult identifies the appended attempt.
type AttemptResult struct {
	Attempt  domain.Attempt
	Revision domain.SyncRevision
}

// HandoffInput is a new handoff record.
type HandoffInput struct {
	ProjectID  string
	Summary    string
	GitContext string
	NextAction string
	// CreateProject registers ProjectID when no project, alias or path
	// matches; without it an unknown project is an error.
	CreateProject bool
}

// HandoffResult identifies the appended handoff.
type HandoffResult struct {
	Handoff  domain.Handoff
	Revision domain.SyncRevision
}

// StatusResult is the read-only instance status.
type StatusResult struct {
	Mode          string
	DataDir       string
	ServerURL     string
	User          string
	SchemaVersion int
	EngineVersion string
	UpdateChannel string
}

// ReviewOptions narrows a review report.
type ReviewOptions struct {
	ProjectID string
	Since     string
}

// ReviewResult is the review output.
type ReviewResult struct {
	Summary string
	Items   []string
	// Proposals lists the newest pending (or stale) proposal candidates,
	// capped at 20. It is empty when the store has no proposal zone.
	Proposals []ProposalListItem
	// PendingProposalCount is the total number of pending proposals, even
	// when Proposals is capped.
	PendingProposalCount int
}

// SyncOptions controls a sync run.
type SyncOptions struct {
	StatusOnly bool
	Push       bool
}

// SyncResult is the sync outcome.
type SyncResult struct {
	Provider  string
	Ahead     int
	Behind    int
	Changed   int
	Conflicts []string
	// Reindexed reports that pulled records were added to the search index.
	Reindexed bool
}

// UpdateOptions controls the updater.
type UpdateOptions struct {
	Check   bool
	Yes     bool
	Channel string
	// Rollback restores the previous engine binary. It cannot be combined with
	// Check or Yes because it neither queries nor downloads a release.
	Rollback bool
}

// UpdateResult is the updater outcome.
type UpdateResult struct {
	Current    VersionInfo
	Latest     VersionInfo
	Applied    bool
	RolledBack bool
}

// VersionInfo is a release identity.
type VersionInfo struct {
	Version string
	Channel string
}

func notImplemented(feature string) error {
	return &NotImplementedError{Feature: feature}
}

// Status reports the configured instance without touching adapters.
func (a *App) Status(ctx context.Context) (StatusResult, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return StatusResult{}, err
		}
	}
	return StatusResult{
		Mode:          a.cfg.Mode,
		DataDir:       a.cfg.DataDir,
		ServerURL:     a.cfg.ServerURL,
		User:          a.cfg.User,
		SchemaVersion: a.cfg.SchemaVersion,
		EngineVersion: version.Version,
		UpdateChannel: a.cfg.UpdateChannel,
	}, nil
}
