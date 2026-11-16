package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
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
	// IncludeBody renders the candidate text of every listed proposal under
	// BodyTokens. It is opt-in because a body is the largest part of a row.
	IncludeBody bool
	// BodyTokens bounds the rendered body; <= 0 uses DefaultProposalBodyTokens.
	BodyTokens int
	// IncludeSnoozed keeps proposals hidden by an active snooze. By default
	// they are omitted unless Statuses names an explicit status filter, which
	// lists them with Snoozed set.
	IncludeSnoozed bool
}

// DefaultProposalListLimit is the proposal window a caller gets without an
// explicit limit.
const DefaultProposalListLimit = 50

// DefaultProposalBodyTokens bounds a proposal body when the caller gives no
// budget. A candidate is a source excerpt, not a whole record.
const DefaultProposalBodyTokens = 600

// ProposalBody is the readable text of one candidate under a token budget.
type ProposalBody struct {
	Kind      domain.ProposalKind
	ID        string
	ProjectID string
	Title     string
	Status    domain.ProposalStatus
	// SnoozedUntil is the stored snooze instant; zero means not snoozed.
	SnoozedUntil time.Time
	UpdatedAt    time.Time
	Body         string
	// Truncated reports that the candidate continues past the budget.
	Truncated bool
	// Tokens is the approximate token count of Body.
	Tokens int
	// MaxTokens is the budget that was applied; zero means unlimited.
	MaxTokens int
}

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
	now := a.now()
	includeSnoozed := query.IncludeSnoozed || len(statuses) > 0
	if !includeSnoozed {
		active := make([]domain.Proposal, 0, len(stored))
		for _, proposal := range stored {
			if proposal.IsSnoozed(now) {
				continue
			}
			active = append(active, proposal)
		}
		stored = active
	}
	listing := ProposalListing{ProjectID: project.ID, Total: len(stored)}
	if len(stored) > limit {
		stored = stored[:limit]
	}
	bodyTokens := query.BodyTokens
	if bodyTokens <= 0 {
		bodyTokens = DefaultProposalBodyTokens
	}
	listing.Items = make([]ProposalListItem, 0, len(stored))
	for _, proposal := range stored {
		item := proposalListItem(proposal, now)
		if query.IncludeBody {
			body, truncated := textbudget.Truncate(ProposalText(proposal), bodyTokens)
			item.Body, item.BodyTruncated = body, truncated
			item.BodyTokens, item.MaxTokens = textbudget.Tokens(body), bodyTokens
		}
		health, err := a.proposalSourceHealth(ctx, proposal)
		if err != nil {
			return ProposalListing{}, err
		}
		item.Stale, item.Orphan, item.LocalOnly, item.HealthReason = health.Stale, health.Orphan, health.LocalOnly, health.Reason
		listing.Items = append(listing.Items, item)
	}
	return listing, nil
}

// ProposalBody returns the readable text of one candidate, cut to maxTokens.
// A non-positive budget returns the whole candidate. Proposal bodies are source
// excerpts; they are still redacted by the caller that renders them.
func (a *App) ProposalBody(ctx context.Context, projectID, id string, maxTokens int) (ProposalBody, error) {
	if err := a.requireStore(); err != nil {
		return ProposalBody{}, err
	}
	if a.deps.Proposals == nil {
		return ProposalBody{}, notImplemented("proposals: proposal store")
	}
	project, err := a.ensureProject(ctx, projectID, false)
	if err != nil {
		return ProposalBody{}, err
	}
	proposal, err := a.deps.Proposals.Get(ctx, project.ID, id)
	if err != nil {
		return ProposalBody{}, fmt.Errorf("load proposal %s: %w", id, err)
	}
	body, truncated := textbudget.Truncate(ProposalText(proposal), maxTokens)
	return ProposalBody{
		Kind: proposal.Kind, ID: proposal.ID, ProjectID: project.ID, Title: proposal.Title,
		Status: proposal.StatusOrPending(), SnoozedUntil: proposal.SnoozedUntil, UpdatedAt: proposal.UpdatedAt,
		Body: body, Truncated: truncated, Tokens: textbudget.Tokens(body), MaxTokens: maxTokens,
	}, nil
}

// ProposalText renders the free body and the ordered sections of a candidate.
// It is the text a promotion copies into the curated record, not a record body.
func ProposalText(proposal domain.Proposal) string {
	var out strings.Builder
	if body := strings.TrimSpace(proposal.Body); body != "" {
		out.WriteString(body)
	}
	for _, section := range proposal.Sections {
		heading, body := strings.TrimSpace(section.Heading), strings.TrimSpace(section.Body)
		if heading == "" && body == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteString("\n\n")
		}
		if heading != "" {
			out.WriteString(heading + ":\n")
		}
		out.WriteString(body)
	}
	return out.String()
}

// PendingProposals lists the pending and stale candidates a review inbox shows,
// newest first, together with the total count. Candidates hidden by an active
// snooze are excluded from both the list and the count; they return on their own
// when the snooze comes due. The returned items carry source health, so an
// inbox can show the promotion blockers without another call.
func (a *App) PendingProposals(ctx context.Context, projectID string) ([]ProposalListItem, int, error) {
	if a.deps.Proposals == nil {
		return nil, 0, nil
	}
	proposals, err := a.listPendingProposals(ctx, projectID)
	if err != nil {
		return nil, 0, err
	}
	now := a.now()
	active := make([]domain.Proposal, 0, len(proposals))
	for _, proposal := range proposals {
		if proposal.IsSnoozed(now) {
			continue
		}
		active = append(active, proposal)
	}
	items, total := proposalListItems(active, proposalReviewLimit, now)
	return items, total, nil
}

// SnoozedProposalCount reports how many pending or stale candidates are hidden
// by a snooze that has not come due yet.
func (a *App) SnoozedProposalCount(ctx context.Context, projectID string) (int, error) {
	if a.deps.Proposals == nil {
		return 0, nil
	}
	proposals, err := a.listPendingProposals(ctx, projectID)
	if err != nil {
		return 0, err
	}
	now := a.now()
	count := 0
	for _, proposal := range proposals {
		if proposal.IsSnoozed(now) {
			count++
		}
	}
	return count, nil
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
