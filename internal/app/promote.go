package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// PromoteOptions selects one proposal and how to promote or dismiss it.
// Promotion is the only path from the machine-local proposal zone into curated
// decisions, attempts and notes.
type PromoteOptions struct {
	// ProposalID is the deterministic proposal id.
	ProposalID string
	// ProjectID scopes the lookup; empty searches every registered project.
	ProjectID string
	// As overrides the record kind the proposal selected; empty keeps it.
	As domain.ProposalKind
	// Status is the promoted decision status (proposed or accepted). It is
	// rejected for attempts and notes.
	Status domain.DecisionStatus
	// Title overrides the proposal title on the created record.
	Title string
	// Dismiss marks the proposal dismissed instead of promoting it.
	Dismiss bool
	// Force allows a stale or orphan proposal.
	Force bool
	// Yes confirms promotion from a local-only source.
	Yes bool
	// DryRun reports what would happen without writing a record or status.
	DryRun bool
}

// PromoteRecord identifies the curated record a promotion created (or would
// create during --dry-run).
type PromoteRecord struct {
	Kind  domain.ProposalKind
	ID    string
	Title string
}

// PromoteResult is the promotion outcome. Already is true for an idempotent
// rerun, including the record-first crash repair path.
type PromoteResult struct {
	Proposal  domain.Proposal
	Record    PromoteRecord
	Already   bool
	Dismissed bool
	DryRun    bool
}

// Promote creates one curated record from a pending proposal, or dismisses the
// proposal. It writes through the ordinary Decision/Attempt/Note use cases and
// records source.proposal_id, so a retry never duplicates the record: the
// record is created first and the proposal status is repaired afterwards.
func (a *App) Promote(ctx context.Context, opts PromoteOptions) (PromoteResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PromoteResult{}, err
	}
	if a.deps.Proposals == nil {
		return PromoteResult{}, notImplemented("promote: proposal store")
	}
	if a.deps.Store == nil {
		return PromoteResult{}, notImplemented("promote: store")
	}
	proposalID := strings.TrimSpace(opts.ProposalID)
	if err := domain.ValidateProposalID("proposal.id", proposalID); err != nil {
		return PromoteResult{}, err
	}
	opts.ProposalID = proposalID
	proposal, err := a.loadProposal(ctx, opts.ProjectID, proposalID)
	if err != nil {
		return PromoteResult{}, err
	}
	targetKind, status, err := promoteTarget(proposal, opts)
	if err != nil {
		return PromoteResult{}, err
	}
	title := strings.TrimSpace(opts.Title)
	if title == "" {
		title = proposal.Title
	}

	proposalStatus := proposal.StatusOrPending()
	if proposalStatus == domain.ProposalPromoted {
		record, found, err := a.findPromotedRecord(ctx, proposal.ProjectID, proposal.ID)
		if err != nil {
			return PromoteResult{}, err
		}
		if !found {
			// The status is authoritative even when the scan cannot see the
			// record (for example a trashed record); report what the proposal
			// retained so a rerun stays idempotent.
			if proposal.PromotedRecordID == "" {
				return PromoteResult{}, fmt.Errorf("proposal %s is marked promoted but has no promoted record id", proposal.ID)
			}
			record = PromoteRecord{
				Kind:  proposalRecordKind(proposal.PromotedRecordID, proposal.Kind),
				ID:    proposal.PromotedRecordID,
				Title: proposal.Title,
			}
		}
		return PromoteResult{Proposal: proposal, Record: record, Already: true}, nil
	}
	if proposalStatus == domain.ProposalDismissed {
		if opts.Dismiss {
			return PromoteResult{Proposal: proposal, Dismissed: true, Already: true}, nil
		}
		return PromoteResult{}, fmt.Errorf("proposal %s is dismissed; rerun with --dismiss to confirm its status", proposal.ID)
	}

	// Record-first repair: a crash after AppendDecision but before SetStatus
	// leaves a pending proposal whose record already exists.
	record, found, err := a.findPromotedRecord(ctx, proposal.ProjectID, proposal.ID)
	if err != nil {
		return PromoteResult{}, err
	}
	if found {
		repaired, err := a.deps.Proposals.SetStatus(ctx, proposal.ProjectID, proposal.ID, domain.ProposalPromoted, record.ID)
		if err != nil {
			return PromoteResult{Proposal: proposal, Record: record, Already: true},
				fmt.Errorf("record %s exists but the proposal status could not be repaired: %w", record.ID, err)
		}
		return PromoteResult{Proposal: repaired, Record: record, Already: true}, nil
	}

	health := proposalSourceHealth{}
	if !opts.Dismiss {
		health, err = a.proposalSourceHealth(ctx, proposal)
		if err != nil {
			return PromoteResult{}, err
		}
		if health.Orphan || health.Stale {
			if !opts.Force {
				kind := "stale"
				if health.Orphan {
					kind = "orphaned"
				}
				return PromoteResult{}, fmt.Errorf("proposal %s is %s (%s); pass --force to promote it anyway",
					proposal.ID, kind, health.Reason)
			}
		}
		if health.LocalOnly && !opts.Yes {
			return PromoteResult{}, domain.NewValidationError("source",
				"connection "+proposal.ConnectionID+" is local-only; pass --yes to confirm that its content may enter curated records")
		}
	}

	if opts.Dismiss {
		if opts.DryRun {
			return PromoteResult{Proposal: proposal, Dismissed: true, DryRun: true}, nil
		}
		updated, err := a.deps.Proposals.SetStatus(ctx, proposal.ProjectID, proposal.ID, domain.ProposalDismissed, "")
		if err != nil {
			return PromoteResult{}, fmt.Errorf("dismiss proposal %s: %w", proposal.ID, err)
		}
		return PromoteResult{Proposal: updated, Dismissed: true}, nil
	}

	planned := PromoteRecord{Kind: targetKind, ID: proposalRecordID(targetKind, proposal.ID), Title: title}
	if opts.DryRun {
		return PromoteResult{Proposal: proposal, Record: planned, DryRun: true}, nil
	}

	source := proposalSourceRef(proposal)
	switch targetKind {
	case domain.ProposalDecision:
		result, err := a.Decision(ctx, DecisionInput{
			ProjectID: proposal.ProjectID,
			Title:     title,
			Status:    status,
			Context:   promoteDecisionContext(proposal),
			Evidence:  promoteEvidence(proposal),
			ID:        planned.ID,
			Source:    source,
		})
		if err != nil {
			return PromoteResult{}, fmt.Errorf("create decision from proposal %s: %w", proposal.ID, err)
		}
		planned = PromoteRecord{Kind: targetKind, ID: result.Decision.ID, Title: result.Decision.Title}
	case domain.ProposalAttempt:
		result, err := a.Attempt(ctx, AttemptInput{
			ProjectID:  proposal.ProjectID,
			Title:      title,
			Hypothesis: promoteAttemptHypothesis(proposal),
			Result:     proposalSection(proposal, "Result"),
			Reason:     promoteAttemptReason(proposal),
			ID:         planned.ID,
			Source:     source,
		})
		if err != nil {
			return PromoteResult{}, fmt.Errorf("create attempt from proposal %s: %w", proposal.ID, err)
		}
		planned = PromoteRecord{Kind: targetKind, ID: result.Attempt.ID, Title: result.Attempt.Title}
	case domain.ProposalNote:
		body := proposalNoteBody(proposal)
		if strings.TrimSpace(body) == "" {
			body = title
		}
		result, err := a.Note(ctx, NoteInput{
			ProjectID: proposal.ProjectID,
			Title:     title,
			Body:      body,
			ID:        planned.ID,
			Source:    source,
		})
		if err != nil {
			return PromoteResult{}, fmt.Errorf("create note from proposal %s: %w", proposal.ID, err)
		}
		planned = PromoteRecord{Kind: targetKind, ID: result.Note.ID, Title: result.Note.Title}
	default:
		return PromoteResult{}, domain.NewValidationError("proposal.kind",
			fmt.Sprintf("unknown target kind %q", targetKind))
	}

	updated, err := a.deps.Proposals.SetStatus(ctx, proposal.ProjectID, proposal.ID, domain.ProposalPromoted, planned.ID)
	if err != nil {
		return PromoteResult{Proposal: proposal, Record: planned},
			fmt.Errorf("record %s was created but proposal %s could not be marked promoted (rerun to repair): %w",
				planned.ID, proposal.ID, err)
	}
	return PromoteResult{Proposal: updated, Record: planned}, nil
}

// promoteTarget validates --as/--status/--dismiss combinations and returns the
// effective record kind and decision status.
func promoteTarget(proposal domain.Proposal, opts PromoteOptions) (domain.ProposalKind, domain.DecisionStatus, error) {
	if opts.Dismiss && (opts.As != "" || opts.Status != "" || strings.TrimSpace(opts.Title) != "") {
		return "", "", domain.NewValidationError("promote",
			"--dismiss cannot be combined with --as, --status or --title")
	}
	kind := opts.As
	if kind == "" {
		kind = proposal.Kind
	}
	if !kind.Valid() {
		return "", "", domain.NewValidationError("promote.as",
			fmt.Sprintf("unknown kind %q (use decision, attempt or note)", kind))
	}
	status := opts.Status
	if status == "" {
		status = domain.DecisionProposed
	}
	if kind != domain.ProposalDecision && opts.Status != "" {
		return "", "", domain.NewValidationError("promote.status",
			"--status is only valid for a decision")
	}
	if opts.Status != "" && status != domain.DecisionProposed && status != domain.DecisionAccepted {
		return "", "", domain.NewValidationError("promote.status",
			fmt.Sprintf("unknown status %q (use proposed or accepted)", opts.Status))
	}
	return kind, status, nil
}

// loadProposal resolves the proposal's project when --project is omitted by
// searching the registered projects for the deterministic id.
func (a *App) loadProposal(ctx context.Context, projectID, proposalID string) (domain.Proposal, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID != "" {
		project, err := a.ensureProject(ctx, projectID, false)
		if err != nil {
			return domain.Proposal{}, err
		}
		proposal, err := a.deps.Proposals.Get(ctx, project.ID, proposalID)
		if err != nil {
			return domain.Proposal{}, fmt.Errorf("load proposal %s: %w", proposalID, err)
		}
		return proposal, nil
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return domain.Proposal{}, err
	}
	for _, project := range projects {
		proposal, err := a.deps.Proposals.Get(ctx, project.ID, proposalID)
		if err == nil {
			return proposal, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.Proposal{}, fmt.Errorf("load proposal %s: %w", proposalID, err)
		}
	}
	return domain.Proposal{}, fmt.Errorf("load proposal %s: %w", proposalID, domain.ErrNotFound)
}

// findPromotedRecord scans curated records for source.proposal_id. The scan is
// what makes promotion idempotent across a crash between the record write and
// the proposal status update.
func (a *App) findPromotedRecord(ctx context.Context, projectID, proposalID string) (PromoteRecord, bool, error) {
	decisions, err := a.listDecisions(ctx, projectID)
	if err != nil {
		return PromoteRecord{}, false, err
	}
	for _, decision := range decisions {
		if decision.Source != nil && decision.Source.ProposalID == proposalID {
			return PromoteRecord{Kind: domain.ProposalDecision, ID: decision.ID, Title: decision.Title}, true, nil
		}
	}
	attempts, err := a.listAttempts(ctx, projectID)
	if err != nil {
		return PromoteRecord{}, false, err
	}
	for _, attempt := range attempts {
		if attempt.Source != nil && attempt.Source.ProposalID == proposalID {
			return PromoteRecord{Kind: domain.ProposalAttempt, ID: attempt.ID, Title: attempt.Title}, true, nil
		}
	}
	notes, err := a.listNotes(ctx, projectID)
	if err != nil {
		return PromoteRecord{}, false, err
	}
	for _, note := range notes {
		if note.Source != nil && note.Source.ProposalID == proposalID {
			return PromoteRecord{Kind: domain.ProposalNote, ID: note.ID, Title: note.Title}, true, nil
		}
	}
	return PromoteRecord{}, false, nil
}

// proposalRecordID is the deterministic curated id for one promotion. The kind
// prefix keeps a proposal promoted as another kind from colliding.
func proposalRecordID(kind domain.ProposalKind, proposalID string) string {
	return string(kind) + "-" + strings.TrimPrefix(proposalID, "proposal-")
}

// proposalRecordKind recovers the kind from a curated record id prefix, which
// is needed when the record itself is no longer listed.
func proposalRecordKind(recordID string, fallback domain.ProposalKind) domain.ProposalKind {
	for _, kind := range []domain.ProposalKind{domain.ProposalDecision, domain.ProposalAttempt, domain.ProposalNote} {
		if strings.HasPrefix(recordID, string(kind)+"-") {
			return kind
		}
	}
	return fallback
}

func promoteDecisionContext(proposal domain.Proposal) string {
	for _, value := range []string{
		proposalSection(proposal, "Context"),
		proposalSection(proposal, "Hypothesis"),
		proposalSection(proposal, "Reason"),
		proposal.Body,
	} {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return proposal.Title
}

func promoteEvidence(proposal domain.Proposal) []string {
	if evidence := strings.TrimSpace(proposalSection(proposal, "Evidence")); evidence != "" {
		return []string{evidence}
	}
	return nil
}

func promoteAttemptHypothesis(proposal domain.Proposal) string {
	if hypothesis := strings.TrimSpace(proposalSection(proposal, "Hypothesis")); hypothesis != "" {
		return hypothesis
	}
	return proposal.Title
}

func promoteAttemptReason(proposal domain.Proposal) string {
	var parts []string
	for _, heading := range []string{"Reason", "Evidence"} {
		if body := strings.TrimSpace(proposalSection(proposal, heading)); body != "" {
			parts = append(parts, body)
		}
	}
	if len(parts) == 0 {
		return proposal.Body
	}
	return strings.Join(parts, "\n\n")
}
