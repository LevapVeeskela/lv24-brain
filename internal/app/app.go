// Package app holds the brain use cases. Feature implementations land in later
// work packages; the skeletons below define the stable call surface.
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

// Dependencies are adapter implementations selected at bootstrap.
type Dependencies struct {
	Store   ports.Store
	Index   ports.Index
	Sync    ports.SyncProvider
	Secrets ports.SecretStore
	Auth    ports.AuthProvider
	Updater ports.Updater
	Now     func() time.Time
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
type DoctorOptions struct{}

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
}

// UpdateOptions controls the updater.
type UpdateOptions struct {
	Check   bool
	Yes     bool
	Channel string
}

// UpdateResult is the updater outcome.
type UpdateResult struct {
	Current VersionInfo
	Latest  VersionInfo
	Applied bool
}

// VersionInfo is a release identity.
type VersionInfo struct {
	Version string
	Channel string
}

func notImplemented(feature string) error {
	return &NotImplementedError{Feature: feature}
}

// Init creates a new brain instance.
func (a *App) Init(ctx context.Context, opts InitOptions) (InitResult, error) {
	return InitResult{}, notImplemented("init")
}

// Doctor checks engine and data compatibility.
func (a *App) Doctor(ctx context.Context, opts DoctorOptions) (DoctorReport, error) {
	return DoctorReport{}, notImplemented("doctor")
}

// Open loads the active project context.
func (a *App) Open(ctx context.Context, opts OpenOptions) (OpenResult, error) {
	return OpenResult{}, notImplemented("open")
}

// Bootstrap builds the agent context for a project.
func (a *App) Bootstrap(ctx context.Context, opts BootstrapOptions) (BootstrapResult, error) {
	return BootstrapResult{}, notImplemented("bootstrap")
}

// Note appends a note.
func (a *App) Note(ctx context.Context, input NoteInput) (NoteResult, error) {
	return NoteResult{}, notImplemented("note")
}

// Decision appends a decision.
func (a *App) Decision(ctx context.Context, input DecisionInput) (DecisionResult, error) {
	return DecisionResult{}, notImplemented("decision")
}

// Attempt appends an attempt.
func (a *App) Attempt(ctx context.Context, input AttemptInput) (AttemptResult, error) {
	return AttemptResult{}, notImplemented("attempt")
}

// Handoff appends a handoff.
func (a *App) Handoff(ctx context.Context, input HandoffInput) (HandoffResult, error) {
	return HandoffResult{}, notImplemented("handoff")
}

// Status reports the configured instance without touching adapters.
func (a *App) Status(ctx context.Context) (StatusResult, error) {
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

// Review builds a review report.
func (a *App) Review(ctx context.Context, opts ReviewOptions) (ReviewResult, error) {
	return ReviewResult{}, notImplemented("review")
}

// Sync pulls, commits and pushes brain data.
func (a *App) Sync(ctx context.Context, opts SyncOptions) (SyncResult, error) {
	return SyncResult{}, notImplemented("sync")
}

// Update checks for and applies engine updates.
func (a *App) Update(ctx context.Context, opts UpdateOptions) (UpdateResult, error) {
	return UpdateResult{}, notImplemented("update")
}
