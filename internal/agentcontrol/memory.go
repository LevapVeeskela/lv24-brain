package agentcontrol

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/localapi"
	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// MemoryApp is the slice of the brain use cases the memory surface needs. It is
// an interface so the local API adapter stays free of the concrete App, and so
// the composition can be tested without a full engine.
type MemoryApp interface {
	ListRecords(context.Context, app.ListRecordsOptions) ([]app.RecordSummary, error)
	RecordBody(context.Context, string, string, int) (app.RecordBody, error)
	RecordLinks(context.Context, string, string) (app.RecordLinks, error)
	Timeline(context.Context, string, string, int) (app.TimelineResult, error)
	Summarize(app.Record) app.RecordSummary
	QuarantinedCount(context.Context, string) (int, error)
	SearchWithReport(context.Context, app.SearchOptions) (app.SearchReport, error)
	GetState(context.Context, string) (domain.Project, domain.State, error)
	SetStateIfRevision(context.Context, app.StateInput, string) (domain.State, error)
	Findings(context.Context, string) ([]app.Finding, error)
	ProposalFindings(context.Context, string) ([]app.Finding, error)
	DueReviews(context.Context, string) ([]app.DueReview, error)
	PendingProposals(context.Context, string) ([]app.ProposalListItem, int, error)
	ListProposals(context.Context, app.ProposalQuery) (app.ProposalListing, error)
	Promote(context.Context, app.PromoteOptions) (app.PromoteResult, error)
	Catchup(context.Context, app.CatchupOptions) (app.CatchupResult, error)
}

// memoryService maps the brain use cases onto the local API's memory views. It
// is the only place that knows which engine field feeds which wire field, and
// it never returns raw-zone text: records, search, review and catchup read
// curated data only.
type memoryService struct {
	app MemoryApp
	now func() time.Time
}

func newMemoryService(memory MemoryApp, now func() time.Time) *memoryService {
	if now == nil {
		now = time.Now
	}
	return &memoryService{app: memory, now: now}
}

// ListRecords returns the newest matching records. A due-review filter is
// applied to the whole scan window before the page bound, so a page never hides
// a due record behind a filtered one.
func (s *memoryService) ListRecords(ctx context.Context, query localapi.RecordQuery) ([]localapi.RecordView, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 50
	}
	scan := limit
	var due map[string]bool
	if query.DueReview {
		scan = localapi.RecordScanWindow
		reviews, err := s.app.DueReviews(ctx, query.ProjectID)
		if err != nil {
			return nil, err
		}
		due = make(map[string]bool, len(reviews))
		for _, review := range reviews {
			if review.Kind != "state" && review.Kind != "proposal" {
				due[review.ID] = true
			}
		}
	}
	records, err := s.app.ListRecords(ctx, app.ListRecordsOptions{
		ProjectID: query.ProjectID, Kinds: query.Kinds, Status: query.Status,
		Subtype: query.Subtype, Scope: query.Scope, TopicKey: query.TopicKey,
		Since: query.Since, Limit: scan,
	})
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	views := make([]localapi.RecordView, 0, len(records))
	for _, record := range records {
		if due != nil && !due[record.ID] {
			continue
		}
		views = append(views, recordView(record, now))
		if len(views) == limit {
			break
		}
	}
	return views, nil
}

// RecordDetail returns one record with its body, links, relations and optional
// timeline.
func (s *memoryService) RecordDetail(ctx context.Context, query localapi.RecordDetailQuery) (localapi.RecordDetailView, error) {
	links, err := s.app.RecordLinks(ctx, query.ProjectID, query.ID)
	if err != nil {
		return localapi.RecordDetailView{}, err
	}
	body, err := s.app.RecordBody(ctx, query.ProjectID, query.ID, query.MaxTokens)
	if err != nil {
		return localapi.RecordDetailView{}, err
	}
	now := s.now().UTC()
	detail := localapi.RecordDetailView{
		RecordView:    recordView(s.app.Summarize(links.Record), now),
		Body:          body.Body,
		BodyTruncated: body.Truncated,
		BodyTokens:    body.Tokens,
		MaxTokens:     body.MaxTokens,
		Links:         make([]localapi.RecordLinkView, 0, len(links.Links)),
		Backlinks:     make([]localapi.BacklinkView, 0, len(links.Backlinks)),
		Relations:     make([]localapi.RelationView, 0, len(links.Record.Relations())),
	}
	for _, link := range links.Links {
		view := localapi.RecordLinkView{
			Field: link.Field, Ref: link.Ref, Text: link.Text, ProjectID: link.ProjectID,
			ID: link.ID, State: link.State, Detail: link.Detail,
		}
		if link.State == app.LinkFound {
			target := recordView(link.Target, now)
			view.Target = &target
		}
		detail.Links = append(detail.Links, view)
	}
	for _, backlink := range links.Backlinks {
		detail.Backlinks = append(detail.Backlinks, localapi.BacklinkView{
			Field: backlink.Field, Source: recordView(backlink.Source, now),
		})
	}
	for _, relation := range links.Record.Relations() {
		normalized := relation.Normalize()
		detail.Relations = append(detail.Relations, localapi.RelationView{
			Target: normalized.Target, Relation: string(normalized.Relation),
			Reason: normalized.Reason, Confidence: normalized.Confidence,
			Status: string(normalized.Status), JudgedAt: normalized.JudgedAt,
			VerdictBy: string(normalized.VerdictBy),
		})
	}
	if query.History {
		timeline, err := s.app.Timeline(ctx, query.ProjectID, query.ID, 0)
		if err != nil {
			return localapi.RecordDetailView{}, err
		}
		history := &localapi.RecordHistoryView{
			Records:     make([]localapi.RecordView, 0, len(timeline.Records)),
			AnchorIndex: timeline.AnchorIndex,
			Before:      timeline.Before,
			After:       timeline.After,
			Total:       timeline.Total,
		}
		for _, record := range timeline.Records {
			history.Records = append(history.Records, recordView(record, now))
		}
		detail.History = history
	}
	return detail, nil
}

// Search runs a curated search. Raw-zone kinds are refused here as well as in
// the engine: the local API never returns raw item text without a grant.
func (s *memoryService) Search(ctx context.Context, query localapi.SearchQuery) (localapi.SearchResult, error) {
	if err := validateSearchKinds(query.Kinds); err != nil {
		return localapi.SearchResult{}, err
	}
	report, err := s.app.SearchWithReport(ctx, app.SearchOptions{
		Text: query.Text, ProjectID: query.ProjectID, Kinds: query.Kinds,
		Limit: query.Limit, MatchMode: query.Mode, MinScore: query.MinScore, Explain: query.Explain,
	})
	if err != nil {
		return localapi.SearchResult{}, err
	}
	result := localapi.SearchResult{Relaxed: report.Relaxed, Notice: report.Notice}
	result.Hits = make([]localapi.SearchHitView, 0, len(report.Hits))
	for _, hit := range report.Hits {
		view := localapi.SearchHitView{
			ID: hit.ID, ProjectID: hit.ProjectID, Kind: hit.Kind, Title: hit.Title,
			Preview: hit.Snippet, Score: hit.Score, Subtype: hit.Subtype, Scope: hit.Scope,
			TopicKey: hit.TopicKey, Occurrences: hit.Occurrences,
			ValidFrom: hit.ValidFrom, ValidTo: hit.ValidTo, SupersededBy: hit.SupersededBy,
			MatchReasons: hit.MatchReasons,
		}
		if hit.Explain != nil {
			view.Explain = &localapi.SearchExplain{
				BM25: hit.Explain.BM25, Recency: hit.Explain.Recency,
				Importance: hit.Explain.Importance, Pin: hit.Explain.Pin, Link: hit.Explain.Link,
			}
		}
		result.Hits = append(result.Hits, view)
	}
	return result, nil
}

// validateSearchKinds accepts curated kinds only.
func validateSearchKinds(kinds []string) error {
	for _, kind := range kinds {
		switch kind {
		case ports.RecordNote, ports.RecordDecision, ports.RecordAttempt, ports.RecordHandoff, "state":
		default:
			return domain.NewValidationError("kind",
				fmt.Sprintf("unknown searchable kind %q (note, decision, attempt, handoff, state); raw-zone kinds are not searchable here", kind))
		}
	}
	return nil
}

// State returns a project's state with its revision.
func (s *memoryService) State(ctx context.Context, projectID string) (localapi.StateView, error) {
	_, state, err := s.app.GetState(ctx, projectID)
	if err != nil {
		return localapi.StateView{}, err
	}
	return stateView(state), nil
}

// SetState applies a state write, optionally as a compare-and-swap on the
// revision the client read.
func (s *memoryService) SetState(ctx context.Context, projectID string, write localapi.StateWrite) (localapi.StateView, error) {
	input := app.StateInput{
		ProjectID: projectID, Goal: write.Goal, NextAction: write.NextAction,
		Active: write.Active, Blocked: write.Blocked, OpenQuestions: write.OpenQuestions,
	}
	if write.FreshUntil != nil {
		value := strings.TrimSpace(*write.FreshUntil)
		if value == "" {
			zero := time.Time{}
			input.FreshUntil = &zero
		} else {
			at, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return localapi.StateView{}, domain.NewValidationError("fresh_until",
					"must be an RFC3339 time or an empty string")
			}
			utc := at.UTC()
			input.FreshUntil = &utc
		}
	}
	state, err := s.app.SetStateIfRevision(ctx, input, write.ExpectedRevision)
	if err != nil {
		return localapi.StateView{}, err
	}
	return stateView(state), nil
}

// Review builds the read-only review report.
func (s *memoryService) Review(ctx context.Context, projectID string) (localapi.ReviewView, error) {
	findings, err := s.app.Findings(ctx, projectID)
	if err != nil {
		return localapi.ReviewView{}, err
	}
	proposalFindings, err := s.app.ProposalFindings(ctx, projectID)
	if err != nil {
		return localapi.ReviewView{}, err
	}
	findings = append(findings, proposalFindings...)
	due, err := s.app.DueReviews(ctx, projectID)
	if err != nil {
		return localapi.ReviewView{}, err
	}
	pending, pendingCount, err := s.app.PendingProposals(ctx, projectID)
	if err != nil {
		return localapi.ReviewView{}, err
	}
	quarantined, err := s.app.QuarantinedCount(ctx, projectID)
	if err != nil {
		return localapi.ReviewView{}, err
	}
	view := localapi.ReviewView{
		ProjectID:        projectID,
		Findings:         make([]localapi.FindingView, 0, len(findings)),
		DueReviews:       make([]localapi.DueReviewView, 0, len(due)),
		QuarantinedCount: quarantined,
	}
	for _, finding := range findings {
		view.Findings = append(view.Findings, localapi.FindingView{
			Kind: string(finding.Kind), Severity: string(finding.Severity), Message: finding.Message,
			Why: finding.Why, Next: finding.Next, Confirm: finding.Confirm,
			Evidence: finding.Evidence, Fixable: finding.Fixable, Fix: finding.Fix,
		})
	}
	sort.SliceStable(view.Findings, func(i, j int) bool {
		left, right := severityRank(view.Findings[i].Severity), severityRank(view.Findings[j].Severity)
		if left != right {
			return left < right
		}
		if view.Findings[i].Kind != view.Findings[j].Kind {
			return view.Findings[i].Kind < view.Findings[j].Kind
		}
		return view.Findings[i].Message < view.Findings[j].Message
	})
	for _, review := range due {
		overdue := review.Overdue
		if overdue < 0 {
			overdue = 0
		}
		view.DueReviews = append(view.DueReviews, localapi.DueReviewView{
			Kind: review.Kind, ID: review.ID, Title: review.Title,
			ReviewAfter: review.ReviewAfter, OverdueSeconds: int64(overdue.Seconds()),
			Source: review.Source,
		})
	}
	view.Proposals = localapi.ProposalSummaryView{PendingCount: pendingCount, Items: proposalViews(pending)}
	return view, nil
}

// severityRank orders findings by how much they need attention.
func severityRank(severity string) int {
	switch app.Severity(severity) {
	case app.SeverityError:
		return 0
	case app.SeverityWarn:
		return 1
	default:
		return 2
	}
}

// ListProposals returns one window of candidates with their source health.
func (s *memoryService) ListProposals(ctx context.Context, query localapi.ProposalQuery) (localapi.ProposalPage, error) {
	listing, err := s.app.ListProposals(ctx, app.ProposalQuery{
		ProjectID: query.ProjectID, Statuses: proposalStatuses(query.Statuses),
		Kind: domain.ProposalKind(strings.TrimSpace(query.Kind)), Limit: query.Limit,
	})
	if err != nil {
		return localapi.ProposalPage{}, err
	}
	return localapi.ProposalPage{
		ProjectID: listing.ProjectID, Items: proposalViews(listing.Items), Total: listing.Total,
	}, nil
}

// proposalStatuses converts the wire status filter; an empty filter keeps every
// status.
func proposalStatuses(statuses []string) []domain.ProposalStatus {
	out := make([]domain.ProposalStatus, 0, len(statuses))
	for _, status := range statuses {
		out = append(out, domain.ProposalStatus(status))
	}
	return out
}

// Promote promotes or dismisses one proposal with the CLI's safety flags.
func (s *memoryService) Promote(ctx context.Context, decision localapi.ProposalDecision) (localapi.ProposalOutcome, error) {
	result, err := s.app.Promote(ctx, app.PromoteOptions{
		ProposalID: decision.ProposalID, ProjectID: decision.ProjectID,
		As:     domain.ProposalKind(strings.TrimSpace(decision.As)),
		Status: domain.DecisionStatus(strings.TrimSpace(decision.Status)),
		Title:  decision.Title, Dismiss: decision.Dismiss,
		Force: decision.Force, Yes: decision.Yes,
		AllowProtected: decision.AllowProtected, AllowDuplicate: decision.AllowDuplicate,
		DryRun: decision.DryRun,
	})
	if err != nil {
		return localapi.ProposalOutcome{}, err
	}
	outcome := localapi.ProposalOutcome{
		ProjectID: result.Proposal.ProjectID, ProposalID: result.Proposal.ID,
		ProposalStatus: string(result.Proposal.StatusOrPending()),
		RecordKind:     string(result.Record.Kind), RecordID: result.Record.ID,
		RecordTitle: result.Record.Title, Already: result.Already,
		Dismissed: result.Dismissed, DryRun: result.DryRun,
	}
	if outcome.ProjectID == "" {
		outcome.ProjectID = decision.ProjectID
	}
	return outcome, nil
}

// Catchup reports what changed since the baseline.
func (s *memoryService) Catchup(ctx context.Context, query localapi.CatchupQuery) (localapi.CatchupView, error) {
	result, err := s.app.Catchup(ctx, app.CatchupOptions{
		ProjectID: query.ProjectID, Since: query.Since,
		IncludeUnreviewed: query.IncludeUnreviewed, Limit: query.Limit,
	})
	if err != nil {
		return localapi.CatchupView{}, err
	}
	now := s.now().UTC()
	view := localapi.CatchupView{
		Project: localapi.Project{ID: result.Project.ID, Name: result.Project.Name},
		Since:   result.Since, SinceSource: result.SinceSource,
		State: stateView(result.State), StateChanged: result.StateChanged,
		Notes:             s.recordViews(noteRecords(result.Notes), now),
		Decisions:         s.recordViews(decisionRecords(result.Decisions), now),
		Attempts:          s.recordViews(attemptRecords(result.Attempts), now),
		Handoffs:          s.recordViews(handoffRecords(result.Handoffs), now),
		Journal:           make([]localapi.JournalView, 0, len(result.Journal)),
		QuarantinedCount:  len(result.Quarantined),
		IncludeUnreviewed: result.IncludeUnreviewed,
	}
	if result.Baseline != nil {
		view.BaselineHandoffID = result.Baseline.ID
	}
	for _, entry := range result.Journal {
		view.Journal = append(view.Journal, journalView(entry))
	}
	return view, nil
}

// recordViews renders a record list as wire rows.
func (s *memoryService) recordViews(records []app.Record, now time.Time) []localapi.RecordView {
	views := make([]localapi.RecordView, 0, len(records))
	for _, record := range records {
		views = append(views, recordView(s.app.Summarize(record), now))
	}
	return views
}

// noteRecords adapts catchup notes to the record union.
func noteRecords(notes []domain.Note) []app.Record {
	records := make([]app.Record, 0, len(notes))
	for i := range notes {
		records = append(records, app.Record{Note: &notes[i]})
	}
	return records
}

// decisionRecords adapts catchup decisions to the record union.
func decisionRecords(decisions []domain.Decision) []app.Record {
	records := make([]app.Record, 0, len(decisions))
	for i := range decisions {
		records = append(records, app.Record{Decision: &decisions[i]})
	}
	return records
}

// attemptRecords adapts catchup attempts to the record union.
func attemptRecords(attempts []domain.Attempt) []app.Record {
	records := make([]app.Record, 0, len(attempts))
	for i := range attempts {
		records = append(records, app.Record{Attempt: &attempts[i]})
	}
	return records
}

// handoffRecords adapts catchup handoffs to the record union.
func handoffRecords(handoffs []domain.Handoff) []app.Record {
	records := make([]app.Record, 0, len(handoffs))
	for i := range handoffs {
		records = append(records, app.Record{Handoff: &handoffs[i]})
	}
	return records
}

// stateView maps a stored state to its wire form.
func stateView(state domain.State) localapi.StateView {
	return localapi.StateView{
		ProjectID: state.ProjectID, Goal: state.Goal, NextAction: state.NextAction,
		Active: state.Active, Blocked: state.Blocked, OpenQuestions: state.OpenQuestions,
		UpdatedAt: state.UpdatedAt, FreshUntil: state.FreshUntil,
		Revision: app.StateRevision(state),
	}
}

// recordView maps one listing row.
func recordView(record app.RecordSummary, now time.Time) localapi.RecordView {
	view := localapi.RecordView{
		ID: record.ID, ProjectID: record.ProjectID, Kind: record.Kind, Title: record.Title,
		Status: record.Status, Path: app.RecordPath(record.ProjectID, record.Kind, record.ID),
		Permalink: record.Permalink, Subtype: record.Subtype, Scope: record.Scope,
		TopicKey: record.TopicKey, Occurrences: record.Occurrences,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		Validity: localapi.ValidityView{
			ValidFrom: record.ValidFrom, ValidTo: record.ValidTo,
			SupersededBy: record.SupersededBy, Supersedes: record.Supersedes,
			Confidence: record.Confidence,
			Current:    record.ValidTo.IsZero() || record.ValidTo.After(now),
		},
		Provenance: provenanceView(record.Provenance),
		Review: localapi.ReviewStateView{
			ReviewAfter: record.ReviewAfter, ReviewSource: record.ReviewSource,
			Due: !record.ReviewAfter.IsZero() && !record.ReviewAfter.After(now),
		},
	}
	return view
}

// provenanceView folds the stored provenance to its documented defaults.
func provenanceView(provenance domain.Provenance) localapi.ProvenanceView {
	return localapi.ProvenanceView{
		Origin:      string(provenance.Origin.Effective()),
		Trust:       string(provenance.Trust.Effective()),
		Reviewed:    provenance.ReviewedState(),
		Quarantined: provenance.Quarantined(),
		NeedsReview: provenance.NeedsReview(),
	}
}

// proposalViews maps candidate rows.
func proposalViews(items []app.ProposalListItem) []localapi.ProposalView {
	views := make([]localapi.ProposalView, 0, len(items))
	for _, item := range items {
		views = append(views, localapi.ProposalView{
			ID: item.ID, ProjectID: item.ProjectID, ExternalID: item.ExternalID,
			Kind: string(item.Kind), Title: item.Title, Rule: item.Rule,
			Confidence: item.Confidence, Status: string(item.Status),
			ConnectionID: item.ConnectionID, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt,
			SourceStale: item.Stale, SourceOrphan: item.Orphan, LocalOnly: item.LocalOnly,
			SourceHealth: item.HealthReason,
		})
	}
	return views
}

// journalView maps one activity line, keeping only the commit fields the
// activity screen shows. A remote URL stays out: it can embed credentials.
func journalView(entry ports.JournalEntry) localapi.JournalView {
	view := localapi.JournalView{
		Time: entry.Time, Event: entry.Event, Project: entry.Project,
		ID: entry.ID, Title: entry.Title,
	}
	for _, key := range []string{"branch", "head", "subject"} {
		if value := strings.TrimSpace(entry.Fields[key]); value != "" {
			if view.Fields == nil {
				view.Fields = map[string]string{}
			}
			view.Fields[key] = value
		}
	}
	return view
}
