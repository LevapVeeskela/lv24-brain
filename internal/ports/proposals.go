package ports

import (
	"context"

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
type ProposalStore interface {
	// List returns proposals of one project, newest updated first. A missing
	// project returns domain.ErrNotFound. Files that cannot be parsed are
	// skipped and reported through ProposalProblemReporter when available.
	List(ctx context.Context, projectID string, opts ProposalListOptions) ([]domain.Proposal, error)
	// Get returns one proposal or domain.ErrNotFound.
	Get(ctx context.Context, projectID, id string) (domain.Proposal, error)
	// Put creates or refreshes a pending proposal. The id, schema version,
	// status and payload hash are filled when empty. A rewrite preserves
	// created_at; an unchanged payload hash and a terminal (promoted or
	// dismissed) proposal are returned as stored without a write. It returns
	// the effective stored proposal.
	Put(ctx context.Context, proposal domain.Proposal) (domain.Proposal, error)
	// SetStatus moves a proposal to status. promoted and dismissed are
	// terminal: they cannot be replaced by another status. Repeating the same
	// terminal status is idempotent and repairs promoted_record_id. recordID
	// is the promoted curated record id and is ignored for other statuses.
	SetStatus(ctx context.Context, projectID, id string, status domain.ProposalStatus, recordID string) (domain.Proposal, error)
}

// ProposalProblemReporter is implemented by stores whose proposal listing
// skips damaged files. Kind is "proposal" for every returned problem.
type ProposalProblemReporter interface {
	ProposalProblems(ctx context.Context, projectID string) ([]RecordProblem, error)
}
