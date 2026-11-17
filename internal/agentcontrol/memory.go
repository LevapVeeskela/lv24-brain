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
	EditRecord(context.Context, app.RecordEdit) (app.RecordEditResult, error)
	Timeline(context.Context, string, string, int) (app.TimelineResult, error)
	Summarize(app.Record) app.RecordSummary
	QuarantinedCount(context.Context, string) (int, error)
	ListQuarantined(context.Context, string, int) ([]app.QuarantinedRecord, error)
	ReviewRecords(context.Context, app.ReviewRecordOptions) (app.ReviewRecordResult, error)
	ReviewRecord(context.Context, app.ReviewRecordInput) (app.RecordSummary, error)
	SearchWithReport(context.Context, app.SearchOptions) (app.SearchReport, error)
	GetState(context.Context, string) (domain.Project, domain.State, error)
	SetStateIfRevision(context.Context, app.StateInput, string) (domain.State, error)
	ReviewState(context.Context, app.ReviewStateOptions) (domain.State, bool, error)
	Findings(context.Context, string) ([]app.Finding, error)
	ProposalFindings(context.Context, string) ([]app.Finding, error)
	PlanFixFindings(context.Context, string, []app.FindingKind) (app.FixPlan, error)
	FixFindings(context.Context, string, []app.FindingKind) (app.FixResult, error)
	DueReviews(context.Context, string) ([]app.DueReview, error)
	PendingProposals(context.Context, string) ([]app.ProposalListItem, int, error)
	SnoozedProposalCount(context.Context, string) (int, error)
	ListProposals(context.Context, app.ProposalQuery) (app.ProposalListing, error)
	ProposalBody(context.Context, string, string, int) (app.ProposalBody, error)
	Promote(context.Context, app.PromoteOptions) (app.PromoteResult, error)
	SnoozeProposal(context.Context, app.SnoozeOptions) (app.SnoozeResult, error)
	Catchup(context.Context, app.CatchupOptions) (app.CatchupResult, error)
	ResolveSelector(context.Context, string) (app.Resolved, error)
	ListProjects(context.Context) ([]domain.Project, error)
	TrashRecord(context.Context, string, string) (app.Record, error)
	RestoreRecord(context.Context, string, string) (app.Record, error)
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
	now := s.now().UTC()
	if !query.AllProjects {
		records, err := s.listProjectRecords(ctx, query, query.ProjectID, limit)
		if err != nil {
			return nil, err
		}
		return recordViewsOf(records, now), nil
	}
	projects, err := s.scopeProjects(ctx, query.ProjectIDs)
	if err != nil {
		return nil, err
	}
	views := make([]localapi.RecordView, 0, limit)
	for _, projectID := range projects {
		records, err := s.listProjectRecords(ctx, query, projectID, limit)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			views = append(views, recordView(record, now))
		}
	}
	sortRecords(views)
	if len(views) > limit {
		views = views[:limit]
	}
	return views, nil
}

// listProjectRecords lists one project's newest matching records. With a due
// filter it scans the whole window so a filtered record cannot hide a due one.
func (s *memoryService) listProjectRecords(ctx context.Context, query localapi.RecordQuery, projectID string, limit int) ([]app.RecordSummary, error) {
	scan := limit
	var due map[string]bool
	if query.DueReview {
		scan = localapi.RecordScanWindow
		reviews, err := s.app.DueReviews(ctx, projectID)
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
		ProjectID: projectID, Kinds: query.Kinds, Status: query.Status,
		Subtype: query.Subtype, Scope: query.Scope, TopicKey: query.TopicKey,
		Since: query.Since, Limit: scan,
	})
	if err != nil {
		return nil, err
	}
	if due == nil {
		return records, nil
	}
	filtered := make([]app.RecordSummary, 0, len(records))
	for _, record := range records {
		if due[record.ID] {
			filtered = append(filtered, record)
			if len(filtered) == limit {
				break
			}
		}
	}
	return filtered, nil
}

// scopeProjects resolves the project scope of an all-projects read. Empty ids
// mean every registered project; the result is bounded by the registry.
func (s *memoryService) scopeProjects(ctx context.Context, projectIDs []string) ([]string, error) {
	if len(projectIDs) > 0 {
		return append([]string(nil), projectIDs...), nil
	}
	projects, err := s.app.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	return ids, nil
}

// recordViewsOf maps a project's records to listing rows.
func recordViewsOf(records []app.RecordSummary, now time.Time) []localapi.RecordView {
	views := make([]localapi.RecordView, 0, len(records))
	for _, record := range records {
		views = append(views, recordView(record, now))
	}
	return views
}

// sortRecords orders rows newest first with the record id as the tie-break, so
// an all-projects page is deterministic.
func sortRecords(views []localapi.RecordView) {
	sort.SliceStable(views, func(i, j int) bool {
		if !views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].CreatedAt.After(views[j].CreatedAt)
		}
		return views[i].ID < views[j].ID
	})
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
			Target: normalized.Target, TargetKind: links.TargetKinds[normalized.Target],
			Relation: string(normalized.Relation),
			Reason:   normalized.Reason, Confidence: normalized.Confidence,
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

// EditRecord applies one text edit and returns the record detail after the
// write, so the desktop can refresh the row and body in one round trip.
func (s *memoryService) EditRecord(ctx context.Context, request localapi.RecordEditRequest) (localapi.RecordDetailView, error) {
	if _, err := s.app.EditRecord(ctx, app.RecordEdit{
		ProjectID: request.ProjectID, ID: request.ID,
		Title: request.Title, Body: request.Body, Tags: request.Tags,
		ExpectedUpdatedAt: request.ExpectedUpdatedAt, AllowDuplicate: request.AllowDuplicate,
	}); err != nil {
		return localapi.RecordDetailView{}, err
	}
	return s.RecordDetail(ctx, localapi.RecordDetailQuery{
		ProjectID: request.ProjectID, ID: request.ID,
		MaxTokens: localapi.RecordBodyDefaultTokens,
	})
}

// Search runs a curated search. Raw-zone kinds are refused here as well as in
// the engine: the local API never returns raw item text without a grant.
func (s *memoryService) Search(ctx context.Context, query localapi.SearchQuery) (localapi.SearchResult, error) {
	if err := validateSearchKinds(query.Kinds); err != nil {
		return localapi.SearchResult{}, err
	}
	if !query.AllProjects {
		return s.searchOne(ctx, query, query.ProjectID)
	}
	// Without an explicit allowlist the index itself answers the unscoped
	// query, which is bounded by the requested window. With an allowlist the
	// scope is exactly its projects, so every hit is inside the policy.
	if len(query.ProjectIDs) == 0 {
		return s.searchOne(ctx, query, "")
	}
	merged := localapi.SearchResult{Hits: make([]localapi.SearchHitView, 0, query.Limit)}
	for _, projectID := range query.ProjectIDs {
		part, err := s.searchOne(ctx, query, projectID)
		if err != nil {
			return localapi.SearchResult{}, err
		}
		merged.Relaxed = merged.Relaxed || part.Relaxed
		if merged.Notice == "" {
			merged.Notice = part.Notice
		}
		merged.Hits = append(merged.Hits, part.Hits...)
	}
	sort.SliceStable(merged.Hits, func(i, j int) bool {
		if merged.Hits[i].Score != merged.Hits[j].Score {
			return merged.Hits[i].Score > merged.Hits[j].Score
		}
		if merged.Hits[i].ProjectID != merged.Hits[j].ProjectID {
			return merged.Hits[i].ProjectID < merged.Hits[j].ProjectID
		}
		return merged.Hits[i].ID < merged.Hits[j].ID
	})
	if query.Limit > 0 && len(merged.Hits) > query.Limit {
		merged.Hits = merged.Hits[:query.Limit]
	}
	return merged, nil
}

// searchOne runs one curated search in one project (empty means every project
// the index serves).
func (s *memoryService) searchOne(ctx context.Context, query localapi.SearchQuery, projectID string) (localapi.SearchResult, error) {
	report, err := s.app.SearchWithReport(ctx, app.SearchOptions{
		Text: query.Text, ProjectID: projectID, Kinds: query.Kinds,
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

// Review builds the read-only review report. An all-projects review aggregates
// the same report over the allowed scope, caps the combined lists and names the
// project of every finding, due review and proposal.
func (s *memoryService) Review(ctx context.Context, query localapi.ReviewQuery) (localapi.ReviewView, error) {
	if !query.AllProjects {
		return s.reviewOne(ctx, query.ProjectID)
	}
	projects, err := s.scopeProjects(ctx, query.ProjectIDs)
	if err != nil {
		return localapi.ReviewView{}, err
	}
	view := localapi.ReviewView{
		ProjectID:   localapi.AllProjectsProjectID,
		ProjectIDs:  projects,
		AllProjects: true,
		Findings:    make([]localapi.FindingView, 0, 16),
		DueReviews:  make([]localapi.DueReviewView, 0, 16),
		Proposals:   localapi.ProposalSummaryView{Items: make([]localapi.ProposalView, 0, 16)},
	}
	for _, projectID := range projects {
		one, err := s.reviewOne(ctx, projectID)
		if err != nil {
			return localapi.ReviewView{}, err
		}
		view.Findings = append(view.Findings, one.Findings...)
		view.DueReviews = append(view.DueReviews, one.DueReviews...)
		view.Proposals.PendingCount += one.Proposals.PendingCount
		view.Proposals.SnoozedCount += one.Proposals.SnoozedCount
		view.Proposals.Items = append(view.Proposals.Items, one.Proposals.Items...)
		view.QuarantinedCount += one.QuarantinedCount
	}
	sortFindings(view.Findings)
	if len(view.Findings) > localapi.AllProjectsFindingsCap {
		view.Findings = view.Findings[:localapi.AllProjectsFindingsCap]
		view.FindingsTruncated = true
	}
	if len(view.DueReviews) > localapi.AllProjectsDueReviewsCap {
		view.DueReviews = view.DueReviews[:localapi.AllProjectsDueReviewsCap]
		view.DueReviewsTruncated = true
	}
	if len(view.Proposals.Items) > localapi.AllProjectsProposalsCap {
		view.Proposals.Items = view.Proposals.Items[:localapi.AllProjectsProposalsCap]
		view.ProposalsTruncated = true
	}
	return view, nil
}

// reviewOne builds the review report of exactly one project.
func (s *memoryService) reviewOne(ctx context.Context, projectID string) (localapi.ReviewView, error) {
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
	snoozedCount, err := s.app.SnoozedProposalCount(ctx, projectID)
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
			ProjectID: projectID,
			Kind:      string(finding.Kind), Severity: string(finding.Severity), Message: finding.Message,
			Why: finding.Why, Next: finding.Next, Confirm: finding.Confirm,
			Evidence: finding.Evidence, Fixable: finding.Fixable, Fix: finding.Fix,
		})
	}
	sortFindings(view.Findings)
	for _, review := range due {
		overdue := review.Overdue
		if overdue < 0 {
			overdue = 0
		}
		view.DueReviews = append(view.DueReviews, localapi.DueReviewView{
			ProjectID: projectID,
			Kind:      review.Kind, ID: review.ID, Title: review.Title,
			ReviewAfter: review.ReviewAfter, OverdueSeconds: int64(overdue.Seconds()),
			Source: review.Source,
		})
	}
	view.Proposals = localapi.ProposalSummaryView{PendingCount: pendingCount, SnoozedCount: snoozedCount, Items: proposalViews(pending)}
	return view, nil
}

// sortFindings orders findings error -> warn -> info, then by kind and message.
func sortFindings(findings []localapi.FindingView) {
	sort.SliceStable(findings, func(i, j int) bool {
		left, right := severityRank(findings[i].Severity), severityRank(findings[j].Severity)
		if left != right {
			return left < right
		}
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Message < findings[j].Message
	})
}

// ReviewFix plans or applies the same safe fixes as `brain review --fix`. It
// never performs a semantic repair: only adopting a later handoff's next action
// and removing an open question a later record answers.
func (s *memoryService) ReviewFix(ctx context.Context, request localapi.ReviewFixRequest) (localapi.ReviewFixView, error) {
	kinds, err := app.ParseFindingKinds(strings.Join(request.Kinds, ","))
	if err != nil {
		return localapi.ReviewFixView{}, err
	}
	view := localapi.ReviewFixView{
		ProjectID: request.ProjectID, DryRun: request.DryRun,
		Findings: make([]localapi.FindingView, 0, 4),
		Changes:  make([]localapi.ReviewFixChange, 0, 4),
	}
	var applied []app.Finding
	var changes []app.FixChange
	var state domain.State
	if request.Apply {
		result, err := s.app.FixFindings(ctx, request.ProjectID, kinds)
		if err != nil {
			return localapi.ReviewFixView{}, err
		}
		applied, changes, state = result.Applied, result.Changes, result.State
	} else {
		plan, err := s.app.PlanFixFindings(ctx, request.ProjectID, kinds)
		if err != nil {
			return localapi.ReviewFixView{}, err
		}
		applied, changes, state = plan.Findings, plan.Changes, plan.State
	}
	for _, finding := range applied {
		view.Findings = append(view.Findings, localapi.FindingView{
			ProjectID: request.ProjectID,
			Kind:      string(finding.Kind), Severity: string(finding.Severity), Message: finding.Message,
			Why: finding.Why, Next: finding.Next, Confirm: finding.Confirm,
			Evidence: finding.Evidence, Fixable: finding.Fixable, Fix: finding.Fix,
		})
	}
	for _, change := range changes {
		view.Changes = append(view.Changes, localapi.ReviewFixChange{
			Kind: string(change.Kind), Action: change.Action, Field: change.Field,
			Value: change.Value, Evidence: change.Evidence,
		})
	}
	view.State = stateView(state)
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

// ListProposals returns one window of candidates with their source health and,
// when asked, a bounded body per candidate. The combined body budget bounds the
// response: items that do not fit are marked body_omitted instead of silently
// shrinking the page.
func (s *memoryService) ListProposals(ctx context.Context, query localapi.ProposalQuery) (localapi.ProposalPage, error) {
	listing, err := s.app.ListProposals(ctx, app.ProposalQuery{
		ProjectID: query.ProjectID, Statuses: proposalStatuses(query.Statuses),
		Kind: domain.ProposalKind(strings.TrimSpace(query.Kind)), Limit: query.Limit,
		IncludeBody: query.IncludeBody, BodyTokens: query.BodyTokens,
		IncludeSnoozed: query.IncludeSnoozed,
	})
	if err != nil {
		return localapi.ProposalPage{}, err
	}
	views := proposalViews(listing.Items)
	if query.IncludeBody {
		spent := 0
		for index := range views {
			if spent+views[index].BodyTokens > localapi.ProposalBodiesBudgetTokens {
				views[index].Body = ""
				views[index].BodyTruncated = false
				views[index].BodyTokens = 0
				views[index].MaxTokens = 0
				views[index].BodyOmitted = true
				continue
			}
			spent += views[index].BodyTokens
		}
	}
	return localapi.ProposalPage{ProjectID: listing.ProjectID, Items: views, Total: listing.Total}, nil
}

// ProposalDetail returns one candidate with its bounded body.
func (s *memoryService) ProposalDetail(ctx context.Context, query localapi.ProposalDetailQuery) (localapi.ProposalDetailView, error) {
	body, err := s.app.ProposalBody(ctx, query.ProjectID, query.ID, query.MaxTokens)
	if err != nil {
		return localapi.ProposalDetailView{}, err
	}
	return localapi.ProposalDetailView{
		ID: body.ID, ProjectID: body.ProjectID, Kind: string(body.Kind),
		Title: body.Title, Status: string(body.Status),
		SnoozedUntil: body.SnoozedUntil, Snoozed: !body.SnoozedUntil.IsZero() && body.SnoozedUntil.After(s.now()),
		UpdatedAt: body.UpdatedAt,
		Body:      body.Body, BodyTruncated: body.Truncated, BodyTokens: body.Tokens, MaxTokens: body.MaxTokens,
	}, nil
}

// SnoozeProposal sets or clears the review snooze of one candidate and returns
// the receipt the desktop shows.
func (s *memoryService) SnoozeProposal(ctx context.Context, request localapi.ProposalSnoozeRequest) (localapi.ProposalSnoozeView, error) {
	until := request.Until
	if request.Clear {
		until = time.Time{}
	}
	result, err := s.app.SnoozeProposal(ctx, app.SnoozeOptions{
		ProjectID: request.ProjectID, ProposalID: request.ProposalID,
		Until: until, ExpectedUpdatedAt: request.ExpectedUpdatedAt,
	})
	if err != nil {
		return localapi.ProposalSnoozeView{}, err
	}
	return localapi.ProposalSnoozeView{
		ProjectID: result.Proposal.ProjectID, ProposalID: result.Proposal.ID,
		Status: string(result.Proposal.StatusOrPending()), Snoozed: result.Snoozed,
		SnoozedUntil: result.Proposal.SnoozedUntil, UpdatedAt: result.Proposal.UpdatedAt,
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

// ListQuarantined returns the records excluded from trusted context, newest
// first, without their bodies.
func (s *memoryService) ListQuarantined(ctx context.Context, query localapi.QuarantineQuery) ([]localapi.QuarantineItemView, error) {
	records, err := s.app.ListQuarantined(ctx, query.ProjectID, query.Limit)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	views := make([]localapi.QuarantineItemView, 0, len(records))
	for _, record := range records {
		views = append(views, localapi.QuarantineItemView{
			ID: record.ID, Kind: record.Kind, ProjectID: record.ProjectID,
			Title: record.Title, Reason: record.Reason, Origin: record.Origin,
			Trust: record.Trust, CreatedAt: record.CreatedAt,
			AgeSeconds: ageSeconds(now, record.CreatedAt),
		})
	}
	return views, nil
}

// ReviewQuarantine marks one quarantined record reviewed, optionally replacing
// its trust, and returns the record after the write.
func (s *memoryService) ReviewQuarantine(ctx context.Context, decision localapi.QuarantineReviewDecision) (localapi.QuarantineReviewView, error) {
	trust := domain.Trust(strings.TrimSpace(decision.Trust))
	if _, err := s.app.ReviewRecords(ctx, app.ReviewRecordOptions{
		ProjectID: decision.ProjectID, ID: decision.ID, Trust: trust,
	}); err != nil {
		return localapi.QuarantineReviewView{}, err
	}
	links, err := s.app.RecordLinks(ctx, decision.ProjectID, decision.ID)
	if err != nil {
		return localapi.QuarantineReviewView{}, err
	}
	summary := s.app.Summarize(links.Record)
	return localapi.QuarantineReviewView{
		Record:     recordView(summary, s.now().UTC()),
		Provenance: provenanceView(summary.Provenance),
	}, nil
}

// ReviewState reviews one project state's provenance and returns the state
// with its revision. The write is provenance-only, so the revision does not
// move and a repeated call is an idempotent no-op.
func (s *memoryService) ReviewState(ctx context.Context, request localapi.StateReviewRequest) (localapi.StateView, error) {
	state, _, err := s.app.ReviewState(ctx, app.ReviewStateOptions{
		ProjectID:        request.ProjectID,
		Trust:            domain.Trust(strings.TrimSpace(request.Trust)),
		ExpectedRevision: request.ExpectedRevision,
	})
	if err != nil {
		return localapi.StateView{}, err
	}
	return stateView(state), nil
}

// ReviewRecord reviews one record by id alone and returns the record summary
// after the write. Trust is validated by the use case; an unknown id is
// domain.ErrNotFound and a stale expected_updated_at is a
// domain.RevisionConflictError, which the handler maps to 409.
func (s *memoryService) ReviewRecord(ctx context.Context, request localapi.RecordReviewRequest) (localapi.RecordView, error) {
	summary, err := s.app.ReviewRecord(ctx, app.ReviewRecordInput{
		ProjectID:         request.ProjectID,
		ID:                request.ID,
		Trust:             domain.Trust(strings.TrimSpace(request.Trust)),
		ExpectedUpdatedAt: request.ExpectedUpdatedAt,
	})
	if err != nil {
		return localapi.RecordView{}, err
	}
	return recordView(summary, s.now().UTC()), nil
}

// ResolveProject maps a project id, alias or path to its registered project,
// using the same rules as every CLI command.
func (s *memoryService) ResolveProject(ctx context.Context, selector string) (localapi.Project, error) {
	resolved, err := s.app.ResolveSelector(ctx, selector)
	if err != nil {
		return localapi.Project{}, err
	}
	return localapi.Project{ID: resolved.Project.ID, Name: resolved.Project.Name}, nil
}

// TrashRecord moves one record to the project trash; it never deletes a file.
func (s *memoryService) TrashRecord(ctx context.Context, projectID, id string) (localapi.RecordView, error) {
	record, err := s.app.TrashRecord(ctx, projectID, id)
	if err != nil {
		return localapi.RecordView{}, err
	}
	return recordView(s.app.Summarize(record), s.now().UTC()), nil
}

// RestoreRecord brings one trashed record back.
func (s *memoryService) RestoreRecord(ctx context.Context, projectID, id string) (localapi.RecordView, error) {
	record, err := s.app.RestoreRecord(ctx, projectID, id)
	if err != nil {
		return localapi.RecordView{}, err
	}
	return recordView(s.app.Summarize(record), s.now().UTC()), nil
}

// ageSeconds is the whole-second age of a timestamp, never negative.
func ageSeconds(now, at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	age := now.Sub(at)
	if age < 0 {
		return 0
	}
	return int64(age.Seconds())
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
		Revision:   app.StateRevision(state),
		Provenance: provenanceView(state.Provenance),
	}
}

// recordView maps one listing row.
func recordView(record app.RecordSummary, now time.Time) localapi.RecordView {
	view := localapi.RecordView{
		ID: record.ID, ProjectID: record.ProjectID, Kind: record.Kind, Title: record.Title,
		Status: record.Status, Path: app.RecordPath(record.ProjectID, record.Kind, record.ID),
		Permalink: record.Permalink, Subtype: record.Subtype, Scope: record.Scope,
		TopicKey: record.TopicKey, Occurrences: record.Occurrences,
		Tags:      append([]string(nil), record.Tags...),
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
			SnoozedUntil: item.SnoozedUntil, Snoozed: item.Snoozed,
			Body: item.Body, BodyTruncated: item.BodyTruncated,
			BodyTokens: item.BodyTokens, MaxTokens: item.MaxTokens,
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
