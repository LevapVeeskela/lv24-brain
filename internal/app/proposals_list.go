package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ProposalQuery narrows a proposal listing. Limit caps how many proposals the
// caller needs; health is only inspected for the returned window, so a large
// proposal zone stays cheap.
type ProposalQuery struct {
	ProjectID string
	// Statuses keeps only these statuses; empty keeps every status.
	Statuses []domain.ProposalStatus
	// Kind keeps only proposals of this kind; empty keeps every kind.
	Kind domain.ProposalKind
	// Limit bounds Items; <= 0 uses DefaultProposalListLimit.
	Limit int
}

// DefaultProposalListLimit is the proposal window a caller gets without an
// explicit limit.
const DefaultProposalListLimit = 50

// ProposalListing is one window of a project's proposals. Total counts every
// proposal matching the query, even when Items is capped.
type ProposalListing struct {
	ProjectID string
	Items     []ProposalListItem
	Total     int
}

// ListProposals lists a project's candidate records with the source health the
// review UI shows. It never promotes or rewrites anything.
func (a *App) ListProposals(ctx context.Context, query ProposalQuery) (ProposalListing, error) {
	if err := a.requireStore(); err != nil {
		return ProposalListing{}, err
	}
	if a.deps.Proposals == nil {
		return ProposalListing{}, notImplemented("proposals: proposal store")
	}
	project, err := a.ensureProject(ctx, query.ProjectID, false)
	if err != nil {
		return ProposalListing{}, err
	}
	statuses, err := normalizeProposalStatuses(query.Statuses)
	if err != nil {
		return ProposalListing{}, err
	}
	if query.Kind != "" && !query.Kind.Valid() {
		return ProposalListing{}, domain.NewValidationError("proposal.kind",
			fmt.Sprintf("unknown kind %q (use decision, attempt or note)", query.Kind))
	}
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultProposalListLimit
	}
	stored, err := a.deps.Proposals.List(ctx, project.ID, ports.ProposalListOptions{
		Statuses: statuses,
		Kind:     query.Kind,
	})
	if err != nil {
		return ProposalListing{}, fmt.Errorf("list proposals: %w", err)
	}
	listing := ProposalListing{ProjectID: project.ID, Total: len(stored)}
	if len(stored) > limit {
		stored = stored[:limit]
	}
	listing.Items = make([]ProposalListItem, 0, len(stored))
	for _, proposal := range stored {
		item := proposalListItem(proposal)
		health, err := a.proposalSourceHealth(ctx, proposal)
		if err != nil {
			return ProposalListing{}, err
		}
		item.Stale, item.Orphan, item.LocalOnly, item.HealthReason = health.Stale, health.Orphan, health.LocalOnly, health.Reason
		listing.Items = append(listing.Items, item)
	}
	return listing, nil
}

// PendingProposals lists the pending and stale candidates a review inbox shows,
// newest first, together with the total count. The returned items carry source
// health, so an inbox can show the promotion blockers without another call.
func (a *App) PendingProposals(ctx context.Context, projectID string) ([]ProposalListItem, int, error) {
	if a.deps.Proposals == nil {
		return nil, 0, nil
	}
	listing, err := a.ListProposals(ctx, ProposalQuery{
		ProjectID: projectID,
		Statuses:  []domain.ProposalStatus{domain.ProposalPending, domain.ProposalStale},
		Limit:     proposalReviewLimit,
	})
	if err != nil {
		return nil, 0, err
	}
	return listing.Items, listing.Total, nil
}

// normalizeProposalStatuses validates a status filter; empty means "every
// status".
func normalizeProposalStatuses(statuses []domain.ProposalStatus) ([]domain.ProposalStatus, error) {
	out := make([]domain.ProposalStatus, 0, len(statuses))
	for _, raw := range statuses {
		status := domain.ProposalStatus(strings.ToLower(strings.TrimSpace(string(raw))))
		if status == "" {
			continue
		}
		if !status.Valid() {
			return nil, domain.NewValidationError("proposal.status",
				fmt.Sprintf("unknown status %q (pending, stale, promoted, dismissed)", raw))
		}
		out = append(out, status)
	}
	return out, nil
}
