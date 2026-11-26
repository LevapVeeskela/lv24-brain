package ports

import (
	"context"
	"slices"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// RecordProblem is a record file a store skipped while listing (unreadable,
// malformed, zero bytes, or quarantined) so it can be reported and repaired.
type RecordProblem struct {
	// Path is the absolute path of the skipped file.
	Path string
	// Kind is the record kind: "note", "decision", "attempt" or "handoff".
	Kind string
	// Reason says why the file was skipped, in a form suitable for display.
	Reason string
}

// Dump is one connector dump file (mail, GitHub or GitLab) for indexing.
type Dump struct {
	// Path is relative to the data directory, with forward slashes.
	Path string
	// ProjectID is the frontmatter project_id ("github:owner/repo",
	// "mail:account"); empty when the file has none.
	ProjectID string
	Title     string
	Body      string
	UpdatedAt time.Time
}

// DumpLister is implemented by stores that keep connector dumps as files.
type DumpLister interface {
	ListDumps(ctx context.Context) ([]Dump, error)
}

// ProblemReporter is implemented by stores whose listing skips damaged files.
type ProblemReporter interface {
	Problems(ctx context.Context, projectID string) ([]RecordProblem, error)
}

// LockInspector is implemented by stores whose writes guard shared files with
// lock files. StaleLocks reports locks that no longer have a live owner: a
// dead process past its TTL, a lock pulled from another host, or one left by a
// previous boot. It never removes them.
type LockInspector interface {
	StaleLocks(ctx context.Context) ([]string, error)
}

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

// StateUpdater is an optional Store capability: it performs a read-modify-write
// of one project's state atomically with respect to other writers of the same
// store. change receives the stored state, or a zero state carrying projectID
// when none is stored, and edits it in place; a non-nil error aborts the write.
// Stores without this capability are updated with GetState and PutState.
type StateUpdater interface {
	UpdateState(ctx context.Context, projectID string, change func(*domain.State) error) (domain.State, error)
}

// Document is the indexed projection of one knowledge record.
type Document struct {
	ID        string
	ProjectID string
	Kind      string
	Title     string
	Body      string
	UpdatedAt time.Time
	// ContentUpdatedAt is the last title or body change, the instant a
	// --since window compares. Zero means UpdatedAt, matching records written
	// before the field existed.
	ContentUpdatedAt time.Time
	// ValidFrom is the instant the record became true. Zero means UpdatedAt,
	// matching records written before validity existed.
	ValidFrom time.Time
	// ValidTo is the instant the record stopped being true. Zero means
	// current.
	ValidTo time.Time
	// SupersededBy and Supersedes are the ids of the supersede chain links.
	SupersededBy string
	Supersedes   string
	// Status is the decision status (proposed, accepted, rejected,
	// superseded), the idea status (open, doing, done, dropped, someday) and
	// is empty for other kinds. Ranking treats an accepted decision as pinned
	// authority.
	Status string
	// Priority is the idea priority (low, normal, high); every idea has one
	// and other kinds carry the normal default.
	Priority string
	// DueAt is the timed due instant of an idea. Zero means no timed due.
	// The index stores the instant only; the offset lives in the Markdown
	// record and is not part of the derived projection.
	DueAt time.Time
	// DueDate is the local calendar date (YYYY-MM-DD) of an all-day idea.
	// Empty means no all-day due.
	DueDate string
	// Confidence is the optional 0..1 author confidence of the record.
	Confidence float64
	// Pinned is an explicit pin set by the owner; it feeds the pin ranking
	// signal and context preference.
	Pinned bool
	// Origin and Trust are the record provenance projected into the index:
	// human/agent/connector/import and verified/stated/inferred. An empty
	// value means the document has no curated provenance (a state, dump or
	// item, or a record written before provenance existed); the ranking reads
	// it as unknown trust, never as a weak claim.
	Origin string
	Trust  string
	// Subtype, Scope and TopicKey are the engram-compatible classification of
	// the record and are empty for records written before it existed.
	Subtype  string
	Scope    string
	TopicKey string
	// Occurrences counts the topic writes the record represents; zero is read
	// as one.
	Occurrences int
	// Relations is the typed relation set of the record. The index stores a
	// derived copy for listings and backlinks; frontmatter stays the source of
	// truth.
	Relations []RelationLink
}

// Match modes of a query. The zero value keeps the historical behaviour: the
// strict AND pass first, then the same terms joined with OR when it finds
// nothing.
const (
	MatchModeAll = "all"
	MatchModeAny = "any"
)

// Match reasons reported by a ranked search. A reason is listed only when its
// signal fired for that hit.
const (
	ReasonBM25       = "bm25"
	ReasonRecency    = "recency"
	ReasonImportance = "importance"
	ReasonPin        = "pin"
	ReasonLink       = "link"
)

// Explain is the weighted contribution of each ranking signal to one hit's
// composite score. The contributions sum to the hit's Score.
type Explain struct {
	BM25       float64
	Recency    float64
	Importance float64
	Pin        float64
	Link       float64
}

// Query narrows a full-text search.
type Query struct {
	Text      string
	ProjectID string
	Limit     int
	// Kinds keeps only these document kinds; empty keeps every kind.
	Kinds []string
	// ExcludeKinds drops these kinds (applied after Kinds).
	ExcludeKinds []string
	// Since keeps only documents whose content changed at or after this time;
	// the zero time keeps every document. Metadata-only writes (review,
	// snooze, validity, merge bookkeeping) do not move the content time, so a
	// window does not re-report the corpus after a review pass.
	Since time.Time
	// AsOf keeps documents valid at that instant: valid_from <= as_of and
	// (valid_to is empty or valid_to > as_of). Zero means "now".
	AsOf time.Time
	// IncludeSuperseded keeps documents whose validity ended. An AsOf instant
	// selects the point in time and takes precedence.
	IncludeSuperseded bool
	// IncludeStubs keeps low-signal stub records that search hides by
	// default. Stubs stay in listings and counts, so this flag only widens
	// retrieval.
	IncludeStubs bool
	// Now is the instant a default query evaluates "current" at. Zero means
	// the adapter clock. Ranking recency is measured from the same instant.
	Now time.Time
	// MatchMode is MatchModeAll or MatchModeAny. Empty keeps the historical
	// AND-then-OR fallback.
	MatchMode string
	// MinScore drops hits whose composite score is below it; zero keeps
	// every hit. The composite score is in [0, 1].
	MinScore float64
	// Explain also fills Hit.MatchReasons and Hit.Explain. MatchReasons is
	// always filled by a ranked index; Explain is the costlier part.
	Explain bool
	// IncludeBody also returns the full document body in Hit.Body.
	IncludeBody bool
	// Subtype, Scope and TopicKey keep only documents whose classification
	// matches. Empty keeps every value; the default search is unchanged.
	Subtype  string
	Scope    string
	TopicKey string
}

// AllowsSince reports whether a document whose content changed at updatedAt
// passes the Since filter. Adapters without SQL use it; SQL adapters apply the
// same rule.
func (q Query) AllowsSince(updatedAt time.Time) bool {
	return q.Since.IsZero() || !updatedAt.Before(q.Since)
}

// NowOrClock returns the query's ranking and validity instant, or the current
// clock when the caller left it zero.
func (q Query) NowOrClock() time.Time {
	if !q.Now.IsZero() {
		return q.Now
	}
	return time.Now()
}

// AllowsKind reports whether a hit of kind passes the Kinds and ExcludeKinds
// filters. Adapters without SQL use it; SQL adapters apply the same rule.
func (q Query) AllowsKind(kind string) bool {
	if len(q.Kinds) > 0 && !slices.Contains(q.Kinds, kind) {
		return false
	}
	return !slices.Contains(q.ExcludeKinds, kind)
}

// Hit is one search result.
type Hit struct {
	ID        string
	ProjectID string
	Kind      string
	Title     string
	Snippet   string
	Score     float64
	// Validity mirrors the indexed document so a caller can show why a record
	// is (or was) current.
	ValidFrom time.Time
	ValidTo   time.Time
	// SupersededBy is the id of the record that replaced this one.
	SupersededBy string
	// Status is the indexed record status: a decision status or an idea
	// status. Other kinds leave it empty.
	Status string
	// Priority is the idea priority (low, normal, high). Every idea carries
	// one; other kinds carry the normal default of the index.
	Priority string
	// DueAt is the timed due instant of an idea; zero when the idea has no
	// timed due (an all-day or undated idea).
	DueAt time.Time
	// DueDate is the all-day due date of an idea in YYYY-MM-DD; empty when
	// the idea has no all-day due.
	DueDate string
	// Subtype, Scope, TopicKey and Occurrences mirror the indexed
	// classification so a caller can show why a hit matched a filter.
	Subtype     string
	Scope       string
	TopicKey    string
	Occurrences int
	// Body is the full document text; it is set only when the query asked
	// for it (Query.IncludeBody).
	Body string
	// ContentHash is the indexed content hash of the record (normalized
	// kind, title and body). It is empty for a document projected without
	// one; search uses it to collapse the same record written on two
	// machines.
	ContentHash string
	// Tier is the retrieval support tier of the hit: supported when the hit
	// contains the rare query terms, related when it only matched the
	// relaxed pass. It is set by the search use case, empty when support was
	// not evaluated (a server-side search).
	Tier string
	// MatchReasons names the ranking signals that fired, in the order bm25,
	// recency, importance, pin, link.
	MatchReasons []string
	// Explain holds the weighted contribution of each signal and is set only
	// when the query asked for it (Query.Explain).
	Explain *Explain
}

// Index provides full-text search over stored knowledge.
type Index interface {
	Upsert(ctx context.Context, doc Document) error
	Delete(ctx context.Context, id string) error
	Search(ctx context.Context, query Query) ([]Hit, error)
}

// IndexLister is implemented by indexes that can enumerate their documents,
// so a reindex can drop documents whose source files are gone.
type IndexLister interface {
	IDs(ctx context.Context) ([]string, error)
}

// SyncStatus is the read-only divergence state of a sync provider.
type SyncStatus struct {
	Provider string
	Branch   string
	Ahead    int
	Behind   int
	Dirty    bool
	// Conflicts lists unmerged paths git still considers unresolved. Those
	// block the next sync.
	Conflicts []string
	// ConflictRecords lists driver-generated records (state-conflicts/,
	// registry-conflicts/, merge-conflicts/) found in the working tree. They
	// are informational: a dirty tree with such a record must still be
	// reportable, so callers surface them alongside Conflicts.
	ConflictRecords []string
	// LastSynced is the revision recorded after the last completed pull or
	// push. It is zero when no sync has completed on this checkout yet.
	LastSynced domain.SyncRevision
}

// SyncOptions controls a pull/commit/push cycle.
type SyncOptions struct {
	Push          bool
	CommitMessage string
	// Autostash stashes a dirty working tree before the rebase and restores it
	// afterwards. Git rejects a rebase with unstaged changes, and a concurrent
	// writer can dirty the tree between the commit and the pull.
	Autostash bool
}

// SyncResult reports what a sync step changed.
type SyncResult struct {
	Revision  domain.SyncRevision
	Changed   int
	Conflicts []string
	// ConflictRecords are driver-generated conflict records written or seen
	// during this sync. They are committed in the same sync and surfaced in
	// `sync --status --json`.
	ConflictRecords []string
	// Recovered reports that an interrupted rebase or a leftover autostash
	// from an earlier run was repaired before this sync proceeded.
	Recovered bool
}

// SyncProvider synchronizes the local brain with a git remote or server.
type SyncProvider interface {
	Status(ctx context.Context) (SyncStatus, error)
	Pull(ctx context.Context, opts SyncOptions) (SyncResult, error)
	Commit(ctx context.Context, message string) (domain.SyncRevision, error)
	Push(ctx context.Context) (SyncResult, error)
}

// SyncRecoverer is implemented by providers that can repair an interrupted
// earlier run (a killed rebase or a leftover autostash). App.Sync calls it
// before the conflict pre-check so a crashed sync is repaired instead of
// leaving the checkout detached.
type SyncRecoverer interface {
	// RecoverInterruptedSync repairs a killed earlier sync and reports whether
	// anything was repaired.
	RecoverInterruptedSync(ctx context.Context) (bool, error)
}

// SyncConflictCommitter is implemented by providers that can stage and commit
// driver-generated conflict records in the same sync that produced them.
type SyncConflictCommitter interface {
	// CommitConflictRecords commits every driver-generated conflict record in
	// the working tree. It returns the paths and whether a commit was created.
	CommitConflictRecords(ctx context.Context, message string) ([]string, bool, error)
}

// GitLayoutVerifier is implemented by git providers that can check that the
// data directory is the root of its work tree. Merge drivers and
// .gitattributes only apply to that repository's paths, so a nested data
// directory cannot be made safe and must be reported to the operator.
type GitLayoutVerifier interface {
	VerifySyncLayout(ctx context.Context) error
}

// SyncLocker serializes sync runs on one data directory across processes. A
// provider that writes a shared checkout implements it so that two `brain
// sync` processes cannot interleave commit, rebase, and push.
type SyncLocker interface {
	// LockSync takes the lock and returns the release function. The release
	// function must always be called.
	LockSync(ctx context.Context) (func(), error)
}

// MergeDriverInstaller registers the deterministic git merge drivers in the
// local clone of a data repository.
type MergeDriverInstaller interface {
	// MissingMergeDrivers names drivers configured in .gitattributes that the
	// local repository has not registered. A data directory that is not a git
	// repository reports domain.ErrUnsupported.
	MissingMergeDrivers(ctx context.Context) ([]string, error)
	// RegisterMergeDrivers writes the managed driver definitions for
	// executable and reports whether anything changed. It is idempotent and
	// does nothing when the data directory is not a git work tree.
	RegisterMergeDrivers(ctx context.Context, executable string) (bool, error)
}

// GitPathLister is implemented by sync providers that can report which files
// the shared tree tracks. Doctor uses it to find machine-local files that were
// published (raw items, checkpoints) without changing them.
type GitPathLister interface {
	TrackedPaths(ctx context.Context, prefix string) ([]string, error)
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

// Connector imports metadata from an external source as Markdown snapshots.
// It is the legacy contract; new integrations implement Source
// (ingestion.go). connectorbuild.LegacySource runs a Connector as a Source.
type Connector interface {
	Name() string
	Run(ctx context.Context, opts ConnectorOptions) (domain.ConnectorRun, error)
}

// ConnectorRunStore persists connector run records for review and audit.
// Local and Git modes write Markdown under connectors/runs. Server mode does
// not expose this interface to the CLI runtime; brain-server stores runs through
// httpapi.Store and the HTTP API.
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
