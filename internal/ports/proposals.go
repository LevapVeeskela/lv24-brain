package ports

import (
	"context"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Proposal lists and stores (contracts-v1, Phase 4.1). Proposals are a
// machine-local derived zone: they are excluded from the curated revision and
// are never synced. ProposalStore is optional and separate from Store so
// server and read-only stores keep satisfying Store.

// ProposalListOptions narrows a proposal listing. Zero values mean "no
// filter" and Limit 0 means "every matching proposal".
type ProposalListOptions struct {
	// Statuses keeps only these statuses; empty keeps every status.
	Statuses []domain.ProposalStatus
	// Kind keeps only proposals of this kind; empty keeps every kind.
	Kind domain.ProposalKind
	// ConnectionID keeps only proposals from this source connection.
	ConnectionID string
	// Rule keeps only proposals produced by this digest rule.
	Rule string
	// Limit caps the result after sorting newest first; 0 means no cap.
	Limit int
}

// ProposalStore persists pending candidate records derived from source items.
// It is optional so adapters without a proposals zone (server mode) remain
// valid Store implementations.
//
// A stored proposal file must carry a canonical status, payload hash and both
// timestamps. A file that cannot be read as a proposal is never overwritten in
// place: adapters preserve the original and report the write as an error.
type ProposalStore interface {
	// List returns proposals of one project, newest updated first. A missing
	// project returns domain.ErrNotFound. Files that cannot be parsed are
	// skipped and reported through ProposalProblemReporter when available.
	List(ctx context.Context, projectID string, opts ProposalListOptions) ([]domain.Proposal, error)
	// Get returns one proposal or domain.ErrNotFound. A damaged file returns
	// a content-free error; its details are available through
	// ProposalProblemReporter, never as raw file paths or field values.
	Get(ctx context.Context, projectID, id string) (domain.Proposal, error)
	// Put creates or refreshes a pending proposal. The schema version,
	// status and payload hash are filled when empty; an empty id is derived
	// from project, connection and external id and the payload hash is
	// recomputed afterwards. A rewrite preserves created_at; an unchanged
	// payload hash and a terminal (promoted or dismissed) proposal are
	// returned as stored without a write. A file that exists but does not
	// read as a proposal is preserved aside and Put returns an error, so a
	// terminal proposal is never silently reset to pending. It returns the
	// effective stored proposal.
	Put(ctx context.Context, proposal domain.Proposal) (domain.Proposal, error)
	// SetStatus moves a proposal to status. promoted and dismissed are
	// terminal: they cannot be replaced by another status. Repeating the same
	// terminal status is idempotent and repairs promoted_record_id. recordID
	// is the promoted curated record id; it is ignored for every other status
	// (a non-promoted proposal never stores one).
	SetStatus(ctx context.Context, projectID, id string, status domain.ProposalStatus, recordID string) (domain.Proposal, error)
}

// ProposalSnoozer hides a pending or stale proposal from review until a time.
// It is optional: a store without it keeps listing every proposal and the app
// reports the snooze use case as unsupported.
type ProposalSnoozer interface {
	// SetSnooze stores until as the return instant; the zero time clears the
	// snooze. Repeating the stored value is idempotent and writes nothing. A
	// terminal (promoted or dismissed) proposal is a validation error.
	SetSnooze(ctx context.Context, projectID, proposalID string, until time.Time) (domain.Proposal, error)
}

// ProposalLocker serializes the read-decide-write window of one proposal
// between processes and goroutines. It is optional: a store without it keeps
// the plain use-case behavior, which is safe for a single writer but lets two
// concurrent promotions of the same pending proposal each write a record
// because both pass the idempotency scan before either marks the proposal.
//
// The lock is scoped to one (project, proposal) pair, so promotions of
// different proposals still run in parallel. fn runs while the lock is held and
// its error is returned unchanged; the lock is released on every path,
// including a panic in fn. A callback that re-enters with the same pair must
// not be used: the lock is not reentrant, because the store's own SetStatus
// takes a separate write lock for the same file.
type ProposalLocker interface {
	WithProposalLock(ctx context.Context, projectID, proposalID string, fn func() error) error
}

// ProposalProblemReporter is implemented by stores whose proposal listing
// skips damaged files. Kind is "proposal" for every returned problem. Path is
// relative to the data directory (forward slashes) so diagnostics never expose
// a machine-local absolute path; Reason is a stable code, not raw file
// content.
type ProposalProblemReporter interface {
	ProposalProblems(ctx context.Context, projectID string) ([]RecordProblem, error)
}
