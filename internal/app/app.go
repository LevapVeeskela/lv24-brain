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
	// MergeDrivers registers the deterministic git merge drivers on the local
	// clone. It is optional and uses the same adapter as Sync.
	MergeDrivers ports.MergeDriverInstaller
	Auth         ports.AuthProvider
	Updater      ports.Updater
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
	// Fix applies the safe repairs doctor can make itself. It registers the
	// managed git merge drivers and never edits records, locks, or configuration.
	Fix bool
}

// DoctorCheck is one compatibility probe.
type DoctorCheck struct {
	Name    string
	Status  string
	Message string
	// Code identifies the finding a check reports, for example
	// "duplicate-id". It is empty when the check passed or has no specific
	// finding.
	Code string
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
	// TokenBudget switches the summary to the layered packer with this total
	// token budget. Zero keeps the legacy rune-only rendering.
	TokenBudget int
	// IncludeUnreviewed includes quarantined records (unreviewed agent,
	// connector or import writes, or an unknown origin) that are excluded from
	// trusted context by default.
	IncludeUnreviewed bool
	// AsOf builds the summary as it was valid at that instant instead of now.
	AsOf time.Time
	// IncludeSuperseded keeps records whose validity ended. An AsOf instant
	// selects the point in time and takes precedence.
	IncludeSuperseded bool
}

// BootstrapResult summarizes generated context.
type BootstrapResult struct {
	ProjectID string
	Summary   string
	// Quarantined lists the ids of records left out of the summary. It is
	// always populated, whether or not IncludeUnreviewed was set.
	Quarantined []string
	// StateQuarantined reports that the project state is quarantined and was
	// not injected as trusted text.
	StateQuarantined bool
	// Pack is the layered budget accounting when TokenBudget was set.
	Pack ContextPack
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
	// Provenance records who wrote the note and whether it was reviewed. The
	// zero value is the CLI default: human, reviewed, verified. MCP writes pass
	// agent; promotion passes connector/stated; an importer passes
	// import/unreviewed so the text stays quarantined until review.
	Provenance domain.Provenance
	// Supersedes is the id of the record this note replaces. The old record is
	// marked superseded after the note is written.
	Supersedes string
	// SupersededBy marks the new note as already replaced by that record; it
	// repairs history and writes valid_to now.
	SupersededBy string
	// Confidence is an optional 0..1 author confidence; zero is unset.
	Confidence float64
	// AllowDuplicate writes even though another record of the same kind
	// already holds the same normalized text. It is the explicit CLI override;
	// MCP writes never set it.
	AllowDuplicate bool
	// CreateProject registers ProjectID when no project, alias or path
	// matches; without it an unknown project is an error.
	CreateProject bool
	// AllowProtected overrides the limits.protected_projects and
	// limits.protected_globs refusal. It is the explicit CLI flag; MCP never
	// sets it.
	AllowProtected bool
	// Relations are explicit relations declared at write time. They are
	// accepted (the author claims them); candidate detection only ever adds
	// pending ones.
	Relations []domain.Relation
	// Subtype is the free-form engram observation type of the note.
	Subtype string
	// Scope is project, personal or global; empty means project.
	Scope domain.RecordScope
	// Topic makes the write a topic upsert when ID is empty: an existing note
	// with the same topic is updated in place instead of appending a copy.
	Topic string
}

// NoteResult identifies the appended note.
type NoteResult struct {
	Note     domain.Note
	Revision domain.SyncRevision
	// Already reports that a topic upsert found the same body and wrote
	// nothing; Note is the stored record.
	Already bool
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
	// Provenance is the write provenance; zero means human/reviewed/verified.
	Provenance domain.Provenance
	// Supersedes is the id of the decision this one replaces.
	Supersedes string
	// SupersededBy marks the new decision as already replaced by that record.
	SupersededBy string
	// Confidence is an optional 0..1 author confidence; zero is unset.
	Confidence float64
	// AllowDuplicate writes even though another decision already holds the
	// same normalized text.
	AllowDuplicate bool
	// CreateProject registers ProjectID when no project, alias or path
	// matches; without it an unknown project is an error.
	CreateProject bool
	// AllowProtected overrides the protected project and glob refusal.
	AllowProtected bool
	// Relations are explicit relations declared at write time.
	Relations []domain.Relation
	// Subtype is the free-form engram observation type of the decision.
	Subtype string
	// Scope is project, personal or global; empty means project.
	Scope domain.RecordScope
	// Topic makes the write a topic upsert when ID is empty.
	Topic string
}

// DecisionResult identifies the appended decision.
type DecisionResult struct {
	Decision domain.Decision
	Revision domain.SyncRevision
	// Already reports that a topic upsert found the same body and wrote
	// nothing; Decision is the stored record.
	Already bool
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
	// Provenance is the write provenance; zero means human/reviewed/verified.
	Provenance domain.Provenance
	// Supersedes is the id of the attempt this one replaces.
	Supersedes string
	// SupersededBy marks the new attempt as already replaced by that record.
	SupersededBy string
	// Confidence is an optional 0..1 author confidence; zero is unset.
	Confidence float64
	// AllowDuplicate writes even though another attempt already holds the
	// same normalized text.
	AllowDuplicate bool
	// CreateProject registers ProjectID when no project, alias or path
	// matches; without it an unknown project is an error.
	CreateProject bool
	// AllowProtected overrides the protected project and glob refusal.
	AllowProtected bool
	// Relations are explicit relations declared at write time.
	Relations []domain.Relation
	// Subtype is the free-form engram observation type of the attempt.
	Subtype string
	// Scope is project, personal or global; empty means project.
	Scope domain.RecordScope
	// Topic makes the write a topic upsert when ID is empty.
	Topic string
}

// AttemptResult identifies the appended attempt.
type AttemptResult struct {
	Attempt  domain.Attempt
	Revision domain.SyncRevision
	// Already reports that a topic upsert found the same body and wrote
	// nothing; Attempt is the stored record.
	Already bool
}

// HandoffInput is a new handoff record.
type HandoffInput struct {
	ProjectID  string
	Summary    string
	GitContext string
	NextAction string
	// Source records the external item an imported handoff came from.
	Source *domain.SourceRef
	// Provenance is the write provenance; zero means human/reviewed/verified.
	Provenance domain.Provenance
	// Supersedes is the id of the handoff this one replaces.
	Supersedes string
	// SupersededBy marks the new handoff as already replaced by that record.
	SupersededBy string
	// Confidence is an optional 0..1 author confidence; zero is unset.
	Confidence float64
	// AllowDuplicate writes even though another handoff already holds the
	// same summary and next action.
	AllowDuplicate bool
	// CreateProject registers ProjectID when no project, alias or path
	// matches; without it an unknown project is an error.
	CreateProject bool
	// AllowProtected overrides the protected project and glob refusal.
	AllowProtected bool
	// Relations are explicit relations declared at write time.
	Relations []domain.Relation
	// Subtype is the free-form engram observation type of the handoff.
	Subtype string
	// Scope is project, personal or global; empty means project.
	Scope domain.RecordScope
	// Topic makes the write a topic upsert when no ID exists for it.
	Topic string
}

// HandoffResult identifies the appended handoff.
type HandoffResult struct {
	Handoff  domain.Handoff
	Revision domain.SyncRevision
	// Already reports that a topic upsert found the same body and wrote
	// nothing; Handoff is the stored record.
	Already bool
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
	// SnoozedProposalCount is the number of pending or stale candidates hidden
	// by a snooze that has not come due. They are excluded from Proposals and
	// PendingProposalCount.
	SnoozedProposalCount int
}

// SyncOptions controls a sync run.
type SyncOptions struct {
	StatusOnly bool
	Push       bool
	// DisableAutostash keeps a dirty working tree in place; git then refuses
	// the rebase. The default stashes and restores it around the pull.
	DisableAutostash bool
}

// SyncResult is the sync outcome.
type SyncResult struct {
	Provider  string
	Branch    string
	Ahead     int
	Behind    int
	Changed   int
	Conflicts []string
	Dirty     bool
	// LastSynced is the revision recorded after the last completed sync on
	// this checkout, or zero when none has completed yet.
	LastSynced domain.SyncRevision
	// Recovered reports that an interrupted rebase or leftover autostash was
	// repaired before this sync proceeded.
	Recovered bool
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
