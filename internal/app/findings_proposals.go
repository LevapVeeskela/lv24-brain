package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/connectorbuild"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Proposal review heuristics (Phase 4.1). They are report-only: `review --fix`
// never promotes, dismisses or rewrites a proposal; a person or agent decides.
const (
	FindingProposalStale        FindingKind = "proposal-stale"
	FindingProposalOrphan       FindingKind = "proposal-orphan"
	FindingProposalContradicted FindingKind = "proposal-contradicted"
)

const (
	// proposalStaleAfter is the age at which an unrefreshed candidate looks
	// stale even when its source item is unchanged.
	proposalStaleAfter = 14 * 24 * time.Hour
	// proposalReviewLimit caps the proposal list rendered by review.
	proposalReviewLimit = 20
)

// ProposalListItem is one pending candidate as review lists it. It carries no
// source body: proposals stay out of context and search.
type ProposalListItem struct {
	ID           string
	Kind         domain.ProposalKind
	Title        string
	Rule         string
	Confidence   float64
	Status       domain.ProposalStatus
	ConnectionID string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// proposalSourceHealth is the freshness verdict shared by review and
// promotion. Findings prefer Orphan over Stale; --force still allows either
// for a deliberate promotion.
type proposalSourceHealth struct {
	Stale     bool
	Orphan    bool
	LocalOnly bool
	Reason    string
}

// proposalSourceHealth inspects the source connection and item of one
// proposal. Adapters without the raw zone skip the checks they cannot do.
func (a *App) proposalSourceHealth(ctx context.Context, proposal domain.Proposal) (proposalSourceHealth, error) {
	health := proposalSourceHealth{}
	updated := proposal.UpdatedAt
	if updated.IsZero() {
		updated = proposal.CreatedAt
	}
	if !updated.IsZero() {
		age := a.now().Sub(updated)
		if age > proposalStaleAfter {
			health.Stale = true
			health.Reason = fmt.Sprintf("not refreshed for %d days", int(age.Hours()/24))
		}
	}
	if proposal.StatusOrPending() == domain.ProposalStale {
		health.Stale = true
		if health.Reason == "" {
			health.Reason = "marked stale"
		}
	}

	connection := domain.Connection{ID: proposal.ConnectionID}
	if a.deps.Connections == nil {
		// No descriptor and no connection record: be conservative and treat
		// the source as local-only.
		health.LocalOnly = true
	} else {
		loaded, err := a.deps.Connections.Get(ctx, proposal.ConnectionID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			health.Orphan = true
			health.LocalOnly = true
			health.Reason = fmt.Sprintf("source connection %q is gone", proposal.ConnectionID)
			return health, nil
		case err != nil:
			return health, err
		default:
			connection = loaded
			health.LocalOnly = connectionLocalOnly(loaded)
			if len(loaded.Projects) > 0 && !slices.Contains(loaded.Projects, proposal.ProjectID) {
				health.Orphan = true
				health.Reason = fmt.Sprintf("source connection %q no longer feeds project %q", loaded.ID, proposal.ProjectID)
				return health, nil
			}
		}
	}

	if a.deps.Items == nil {
		return health, nil
	}
	item, err := a.deps.Items.Get(ctx, domain.ItemRef{ConnectionID: proposal.ConnectionID, ExternalID: proposal.ExternalID})
	switch {
	case errors.Is(err, domain.ErrNotFound):
		health.Orphan = true
		health.Reason = fmt.Sprintf("source item %q is gone", proposal.ExternalID)
		return health, nil
	case err != nil:
		return health, err
	case item.Deleted:
		health.Orphan = true
		health.Reason = fmt.Sprintf("source item %q is tombstoned", proposal.ExternalID)
		return health, nil
	}
	if item.ProjectID != "" && item.ProjectID != proposal.ProjectID {
		health.Orphan = true
		health.Reason = fmt.Sprintf("source item moved to project %q", item.ProjectID)
		return health, nil
	}

	match, ok := matchProposalRule(connection, item)
	if !ok {
		health.Stale = true
		health.Reason = "source item no longer matches a digest rule"
		return health, nil
	}
	rebuilt, err := buildProposal(proposal.ProjectID, connection, item, match)
	if err != nil {
		health.Stale = true
		health.Reason = "source item cannot be rebuilt safely"
		return health, nil
	}
	payload := proposal.PayloadHash
	if payload == "" {
		payload = proposal.WithPayloadHash().PayloadHash
	}
	if rebuilt.PayloadHash != payload {
		health.Stale = true
		health.Reason = "source item changed since the digest"
	}
	return health, nil
}

// connectionLocalOnly applies the documented precedence: an explicit
// connection override, then the provider descriptor default. An unknown
// provider is treated as local-only so promotion errs on the safe side.
func connectionLocalOnly(connection domain.Connection) bool {
	if connection.Publish != "" {
		return connection.Publish == domain.PublishLocalOnly
	}
	entry, err := connectorbuild.Builtin().Lookup(connection.Provider)
	if err != nil {
		return true
	}
	return entry.Descriptor.DefaultPublish == domain.PublishLocalOnly
}

// pendingProposalCount returns how many candidates wait for a decision.
func (a *App) pendingProposalCount(ctx context.Context, projectID string) (int, error) {
	proposals, err := a.listPendingProposals(ctx, projectID)
	if err != nil {
		return 0, fmt.Errorf("list proposals: %w", err)
	}
	return len(proposals), nil
}

// listPendingProposals returns pending and stale candidates newest first.
func (a *App) listPendingProposals(ctx context.Context, projectID string) ([]domain.Proposal, error) {
	if a.deps.Proposals == nil {
		return nil, nil
	}
	return a.deps.Proposals.List(ctx, projectID, ports.ProposalListOptions{
		Statuses: []domain.ProposalStatus{domain.ProposalPending, domain.ProposalStale},
	})
}

// proposalListItems converts proposals for review, capping the list while
// keeping the total count.
func proposalListItems(proposals []domain.Proposal, limit int) ([]ProposalListItem, int) {
	total := len(proposals)
	if limit > 0 && len(proposals) > limit {
		proposals = proposals[:limit]
	}
	items := make([]ProposalListItem, 0, len(proposals))
	for _, proposal := range proposals {
		items = append(items, ProposalListItem{
			ID:           proposal.ID,
			Kind:         proposal.Kind,
			Title:        proposal.Title,
			Rule:         proposal.Rule,
			Confidence:   proposal.Confidence,
			Status:       proposal.StatusOrPending(),
			ConnectionID: proposal.ConnectionID,
			CreatedAt:    proposal.CreatedAt,
			UpdatedAt:    proposal.UpdatedAt,
		})
	}
	return items, total
}

// ProposalFindings computes proposal-stale, proposal-orphan and
// proposal-contradicted findings. Review calls it explicitly; Findings and
// Context stay curated-only so they do not pay raw item reads.
func (a *App) ProposalFindings(ctx context.Context, projectID string) ([]Finding, error) {
	if a.deps.Proposals == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	proposals, err := a.listPendingProposals(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list proposals: %w", err)
	}
	if len(proposals) == 0 {
		return nil, nil
	}
	decisions, err := a.listDecisions(ctx, projectID)
	if err != nil {
		return nil, err
	}
	attempts, err := a.listAttempts(ctx, projectID)
	if err != nil {
		return nil, err
	}

	var findings []Finding
	for _, proposal := range proposals {
		health, err := a.proposalSourceHealth(ctx, proposal)
		if err != nil {
			return nil, err
		}
		switch {
		case health.Orphan:
			findings = append(findings, Finding{
				Kind: FindingProposalOrphan,
				Message: fmt.Sprintf("proposal %q has no usable source: %s; dismiss it with `brain promote %s --dismiss`",
					SummarizeLine(proposal.Title, 80), health.Reason, proposal.ID),
				Evidence: proposal.ID,
			})
			continue
		case health.Stale:
			findings = append(findings, Finding{
				Kind: FindingProposalStale,
				Message: fmt.Sprintf("proposal %q is stale (%s); confirm it with `brain promote %s --force` or dismiss it",
					SummarizeLine(proposal.Title, 80), health.Reason, proposal.ID),
				Evidence: proposal.ID,
			})
			continue
		}
		if evidence := proposalContradiction(proposal, decisions, attempts); evidence != "" {
			findings = append(findings, Finding{
				Kind: FindingProposalContradicted,
				Message: fmt.Sprintf("proposal %q matches a later decision or failed attempt; confirm it or dismiss it",
					SummarizeLine(proposal.Title, 80)),
				Evidence: evidence,
			})
		}
	}
	return findings, nil
}

// proposalContradiction looks for an accepted decision or an attempt written
// after the proposal with a similar title. Attempts record failed approaches,
// so any later similar attempt may contradict the candidate.
func proposalContradiction(proposal domain.Proposal, decisions []domain.Decision, attempts []domain.Attempt) string {
	for _, decision := range decisions {
		if decision.Status != domain.DecisionAccepted || !decision.CreatedAt.After(proposal.CreatedAt) {
			continue
		}
		if similar(proposal.Title, decision.Title) {
			return decision.ID
		}
	}
	for _, attempt := range attempts {
		if !attempt.CreatedAt.After(proposal.CreatedAt) {
			continue
		}
		if similar(proposal.Title, attempt.Title) {
			return attempt.ID
		}
	}
	return ""
}

// proposalSourceRef builds the provenance recorded on a promoted record.
func proposalSourceRef(proposal domain.Proposal) *domain.SourceRef {
	source := &domain.SourceRef{
		ProjectID:    proposal.ProjectID,
		ConnectionID: proposal.ConnectionID,
		ExternalID:   proposal.ExternalID,
		ProposalID:   proposal.ID,
		Confidence:   proposal.Confidence,
	}
	for _, citation := range proposal.Citations {
		if citation.ItemKey != "" {
			source.ItemKey = citation.ItemKey
		}
		if source.URL == "" && citation.URL != "" {
			source.URL = citation.URL
		}
		if source.ObservedAt.IsZero() && !citation.ObservedAt.IsZero() {
			source.ObservedAt = citation.ObservedAt
		}
	}
	return source
}

// proposalSection returns the first section with the given heading.
func proposalSection(proposal domain.Proposal, heading string) string {
	for _, section := range proposal.Sections {
		if strings.EqualFold(strings.TrimSpace(section.Heading), heading) {
			return section.Body
		}
	}
	return ""
}

// proposalNoteBody keeps the bounded source excerpt and appends the ordered
// sections so a promoted note does not lose provenance.
func proposalNoteBody(proposal domain.Proposal) string {
	var out strings.Builder
	out.WriteString(proposal.Body)
	for _, section := range proposal.Sections {
		if strings.TrimSpace(section.Body) == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		fmt.Fprintf(&out, "## %s\n\n%s", section.Heading, section.Body)
	}
	return out.String()
}
