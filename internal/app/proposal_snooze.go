package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// SnoozeOptions hides one proposal from the review queue until a time. A zero
// Until clears the snooze. ExpectedUpdatedAt is optional: when set, the write is
// a compare-and-swap on the proposal revision the caller read.
type SnoozeOptions struct {
	ProjectID         string
	ProposalID        string
	Until             time.Time
	ExpectedUpdatedAt time.Time
}

// SnoozeResult is the proposal after the snooze write.
type SnoozeResult struct {
	Proposal domain.Proposal
	// Snoozed reports whether the stored snooze is still in the future.
	Snoozed bool
}

// SnoozeProposal sets or clears the review snooze of one pending or stale
// proposal. Snoozing never promotes, dismisses or rewrites the candidate text;
// the lifecycle status is unchanged. A terminal proposal is refused with a
// validation error, and an Until that is not in the future is refused rather
// than stored as an already-due snooze.
//
// When the proposal store implements ports.ProposalLocker the read-decide-write
// window runs under the same per-proposal lock as promotion, so a snooze and a
// promotion of one proposal cannot interleave.
func (a *App) SnoozeProposal(ctx context.Context, opts SnoozeOptions) (SnoozeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SnoozeResult{}, err
	}
	if a.deps.Proposals == nil {
		return SnoozeResult{}, notImplemented("snooze: proposal store")
	}
	if a.deps.Store == nil {
		return SnoozeResult{}, notImplemented("snooze: store")
	}
	proposalID := strings.TrimSpace(opts.ProposalID)
	if err := domain.ValidateProposalID("proposal.id", proposalID); err != nil {
		return SnoozeResult{}, err
	}
	opts.ProposalID = proposalID
	proposal, err := a.loadProposal(ctx, opts.ProjectID, proposalID)
	if err != nil {
		return SnoozeResult{}, err
	}
	locker, locked := a.deps.Proposals.(ports.ProposalLocker)
	if !locked {
		return a.snoozeLocked(ctx, opts, proposal)
	}
	var result SnoozeResult
	lockErr := locker.WithProposalLock(ctx, proposal.ProjectID, proposal.ID, func() error {
		current, err := a.deps.Proposals.Get(ctx, proposal.ProjectID, proposal.ID)
		if err != nil {
			return fmt.Errorf("load proposal %s: %w", proposal.ID, err)
		}
		result, err = a.snoozeLocked(ctx, opts, current)
		return err
	})
	if lockErr != nil {
		return result, lockErr
	}
	return result, nil
}

// snoozeLocked applies the snooze to one already loaded proposal.
func (a *App) snoozeLocked(ctx context.Context, opts SnoozeOptions, proposal domain.Proposal) (SnoozeResult, error) {
	now := a.now()
	until := opts.Until.UTC()
	if !opts.Until.IsZero() {
		if !until.After(now) {
			return SnoozeResult{}, domain.NewValidationError("snooze.until",
				"must be in the future; pass a later date or omit it to clear the snooze")
		}
	} else {
		until = time.Time{}
	}
	if proposal.StatusOrPending().Terminal() {
		return SnoozeResult{}, domain.NewValidationError("snooze.until",
			fmt.Sprintf("proposal %s is %s; a terminal proposal cannot be snoozed", proposal.ID, proposal.StatusOrPending()))
	}
	// The desired state already holds: report it without a write, even when a
	// retried request carries the revision from before the first attempt.
	if proposal.SnoozedUntil.Equal(until) {
		return SnoozeResult{Proposal: proposal, Snoozed: proposal.IsSnoozed(now)}, nil
	}
	if !opts.ExpectedUpdatedAt.IsZero() && !opts.ExpectedUpdatedAt.UTC().Equal(proposal.UpdatedAt.UTC()) {
		return SnoozeResult{}, domain.NewRevisionConflictError("proposal", proposal.ID, proposal.UpdatedAt)
	}
	snoozer, ok := a.deps.Proposals.(ports.ProposalSnoozer)
	if !ok {
		return SnoozeResult{}, fmt.Errorf("%w: snoozing proposals is not supported by this store", domain.ErrUnsupported)
	}
	updated, err := snoozer.SetSnooze(ctx, proposal.ProjectID, proposal.ID, until)
	if err != nil {
		return SnoozeResult{}, err
	}
	return SnoozeResult{Proposal: updated, Snoozed: updated.IsSnoozed(now)}, nil
}
