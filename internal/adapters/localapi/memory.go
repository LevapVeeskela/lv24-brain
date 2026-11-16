package localapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// The memory routes expose the curated record zone, search, project state,
// review, proposals and catchup to the desktop. Field names are snake_case,
// matching the engine's search-v2 and MCP contract; the older routes predate
// that convention and keep their frozen camelCase fields.

// ProvenanceView is the authorship, review state and trust of a record. Empty
// stored values are folded to their documented defaults: a human write is
// reviewed and verified.
type ProvenanceView struct {
	Origin      string `json:"origin"`
	Trust       string `json:"trust"`
	Reviewed    bool   `json:"reviewed"`
	Quarantined bool   `json:"quarantined"`
	NeedsReview bool   `json:"needs_review"`
}

// ValidityView is the temporal validity and supersede state of a record.
type ValidityView struct {
	ValidFrom    time.Time `json:"valid_from,omitzero"`
	ValidTo      time.Time `json:"valid_to,omitzero"`
	SupersededBy string    `json:"superseded_by,omitempty"`
	Supersedes   string    `json:"supersedes,omitempty"`
	Confidence   float64   `json:"confidence,omitempty"`
	// Current reports that the record has not ended.
	Current bool `json:"current"`
}

// ReviewStateView is the review schedule of a record.
type ReviewStateView struct {
	ReviewAfter  time.Time `json:"review_after,omitzero"`
	ReviewSource string    `json:"review_source,omitempty"`
	// Due reports that the review date has passed.
	Due bool `json:"due"`
}

// RecordView is one record in a listing.
type RecordView struct {
	ID          string          `json:"id"`
	ProjectID   string          `json:"project_id"`
	Kind        string          `json:"kind"`
	Title       string          `json:"title"`
	Status      string          `json:"status,omitempty"`
	Path        string          `json:"path"`
	Permalink   string          `json:"permalink,omitempty"`
	Subtype     string          `json:"subtype,omitempty"`
	Scope       string          `json:"scope,omitempty"`
	TopicKey    string          `json:"topic_key,omitempty"`
	Occurrences int             `json:"occurrences,omitempty"`
	CreatedAt   time.Time       `json:"created_at,omitzero"`
	UpdatedAt   time.Time       `json:"updated_at,omitzero"`
	Validity    ValidityView    `json:"validity"`
	Provenance  ProvenanceView  `json:"provenance"`
	Review      ReviewStateView `json:"review"`
}

// RecordLinkView is one resolved outgoing [[...]] reference.
type RecordLinkView struct {
	Field     string      `json:"field"`
	Ref       string      `json:"ref"`
	Text      string      `json:"text,omitempty"`
	ProjectID string      `json:"project_id,omitempty"`
	ID        string      `json:"id,omitempty"`
	State     string      `json:"state"`
	Detail    string      `json:"detail,omitempty"`
	Target    *RecordView `json:"target,omitempty"`
}

// BacklinkView is a record of the same project that references this one.
type BacklinkView struct {
	Field  string     `json:"field"`
	Source RecordView `json:"source"`
}

// RelationView is one typed relation of a record. Targets are record ids of the
// same project, so a client can open them with the record route.
type RelationView struct {
	Target     string    `json:"target"`
	Relation   string    `json:"relation"`
	Reason     string    `json:"reason,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
	Status     string    `json:"status"`
	JudgedAt   time.Time `json:"judged_at,omitzero"`
	VerdictBy  string    `json:"verdict_by,omitempty"`
}

// RecordHistoryView is the timeline around one record: its neighbours of the
// same kind, oldest first, including records whose validity ended.
type RecordHistoryView struct {
	Records     []RecordView `json:"records"`
	AnchorIndex int          `json:"anchor_index"`
	Before      int          `json:"before"`
	After       int          `json:"after"`
	Total       int          `json:"total"`
}

// RecordDetailView is one record with its body, links, relations and optional
// history.
type RecordDetailView struct {
	RecordView
	Body          string             `json:"body"`
	BodyTruncated bool               `json:"body_truncated"`
	BodyTokens    int                `json:"body_tokens"`
	MaxTokens     int                `json:"max_tokens"`
	Links         []RecordLinkView   `json:"links"`
	Backlinks     []BacklinkView     `json:"backlinks"`
	Relations     []RelationView     `json:"relations"`
	History       *RecordHistoryView `json:"history,omitempty"`
}

// RecordQuery narrows a record listing. Limit is the number of newest matching
// records the service must return so the handler can page deterministically;
// the handler always asks for its full scan window.
type RecordQuery struct {
	ProjectID string
	Kinds     []string
	Status    string
	Subtype   string
	Scope     string
	TopicKey  string
	Since     time.Time
	DueReview bool
	Limit     int
}

// RecordDetailQuery selects one record.
type RecordDetailQuery struct {
	ProjectID string
	ID        string
	MaxTokens int
	History   bool
}

// SearchQuery is one ranked search request. Raw-zone kinds are never requested,
// so a search can only answer from curated records.
type SearchQuery struct {
	Text      string
	ProjectID string
	Kinds     []string
	Limit     int
	// Mode is all or any; empty keeps the strict-then-relaxed history.
	Mode     string
	MinScore float64
	Explain  bool
}

// SearchExplain is the weighted contribution of every ranking signal.
type SearchExplain struct {
	BM25       float64 `json:"bm25"`
	Recency    float64 `json:"recency"`
	Importance float64 `json:"importance"`
	Pin        float64 `json:"pin"`
	Link       float64 `json:"link"`
}

// SearchHitView is one ranked result. Preview is a bounded snippet, never the
// whole body.
type SearchHitView struct {
	ID           string         `json:"id"`
	ProjectID    string         `json:"project_id"`
	Kind         string         `json:"kind"`
	Title        string         `json:"title"`
	Preview      string         `json:"preview"`
	Score        float64        `json:"score"`
	Subtype      string         `json:"subtype,omitempty"`
	Scope        string         `json:"scope,omitempty"`
	TopicKey     string         `json:"topic_key,omitempty"`
	Occurrences  int            `json:"occurrences,omitempty"`
	ValidFrom    time.Time      `json:"valid_from,omitzero"`
	ValidTo      time.Time      `json:"valid_to,omitzero"`
	SupersededBy string         `json:"superseded_by,omitempty"`
	MatchReasons []string       `json:"match_reasons,omitempty"`
	Explain      *SearchExplain `json:"explain,omitempty"`
}

// SearchResult is one page of ranked hits plus what the query had to do.
type SearchResult struct {
	Hits    []SearchHitView
	Relaxed bool
	Notice  string
}

// StateView is a project's curated state with its CAS revision.
type StateView struct {
	ProjectID     string    `json:"project_id"`
	Goal          string    `json:"goal,omitempty"`
	NextAction    string    `json:"next_action,omitempty"`
	Active        []string  `json:"active,omitempty"`
	Blocked       []string  `json:"blocked,omitempty"`
	OpenQuestions []string  `json:"open_questions,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitzero"`
	FreshUntil    time.Time `json:"fresh_until,omitzero"`
	Revision      string    `json:"revision"`
}

// StateWrite changes selected state fields; nil keeps the stored value and an
// empty slice or string clears it. FreshUntil is an RFC3339 time, an empty
// string to clear it, or nil to keep it. ExpectedRevision is optional: when set,
// the write is refused with 409 unless the stored state still has it.
type StateWrite struct {
	Goal             *string   `json:"goal"`
	NextAction       *string   `json:"next_action"`
	Active           *[]string `json:"active"`
	Blocked          *[]string `json:"blocked"`
	OpenQuestions    *[]string `json:"open_questions"`
	FreshUntil       *string   `json:"fresh_until"`
	ExpectedRevision string    `json:"expected_revision"`
}

// Empty reports whether the write changes nothing.
func (w StateWrite) Empty() bool {
	return w.Goal == nil && w.NextAction == nil && w.Active == nil && w.Blocked == nil &&
		w.OpenQuestions == nil && w.FreshUntil == nil
}

// FindingView is one review finding.
type FindingView struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Why      string `json:"why"`
	Next     string `json:"next"`
	Confirm  string `json:"confirm"`
	Evidence string `json:"evidence,omitempty"`
	Fixable  bool   `json:"fixable"`
	Fix      string `json:"fix,omitempty"`
}

// DueReviewView is one record, state or proposal past its review date.
type DueReviewView struct {
	Kind           string    `json:"kind"`
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	ReviewAfter    time.Time `json:"review_after"`
	OverdueSeconds int64     `json:"overdue_seconds"`
	Source         string    `json:"source"`
}

// ProposalView is one candidate record. It carries identity, confidence and
// source health; proposal bodies stay out of the list.
type ProposalView struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	ExternalID   string    `json:"external_id,omitempty"`
	Kind         string    `json:"kind"`
	Title        string    `json:"title"`
	Rule         string    `json:"rule,omitempty"`
	Confidence   float64   `json:"confidence"`
	Status       string    `json:"status"`
	ConnectionID string    `json:"connection_id"`
	CreatedAt    time.Time `json:"created_at,omitzero"`
	UpdatedAt    time.Time `json:"updated_at,omitzero"`
	// SourceStale, SourceOrphan and LocalOnly are the promotion blockers: a
	// stale or orphaned source needs force, a local-only source needs yes.
	SourceStale  bool   `json:"source_stale"`
	SourceOrphan bool   `json:"source_orphan"`
	LocalOnly    bool   `json:"local_only"`
	SourceHealth string `json:"source_health,omitempty"`
}

// ProposalSummaryView is the pending-proposal summary of a review report.
type ProposalSummaryView struct {
	PendingCount int            `json:"pending_count"`
	Items        []ProposalView `json:"items"`
}

// ReviewView is the read-only review report: findings, due reviews, pending
// proposals and the quarantine size.
type ReviewView struct {
	ProjectID        string              `json:"project_id"`
	Findings         []FindingView       `json:"findings"`
	DueReviews       []DueReviewView     `json:"due_reviews"`
	Proposals        ProposalSummaryView `json:"proposals"`
	QuarantinedCount int                 `json:"quarantined_count"`
}

// ProposalQuery narrows a proposal listing.
type ProposalQuery struct {
	ProjectID string
	Statuses  []string
	Kind      string
	Limit     int
}

// ProposalPage is one window of proposals plus the total matching count.
type ProposalPage struct {
	ProjectID string
	Items     []ProposalView
	Total     int
}

// ProposalDecision promotes or dismisses one proposal. Force and Yes are the
// explicit CLI overrides (--force/--yes); neither is ever implied.
type ProposalDecision struct {
	ProjectID      string
	ProposalID     string
	Dismiss        bool
	As             string
	Status         string
	Title          string
	Force          bool
	Yes            bool
	AllowDuplicate bool
	AllowProtected bool
	DryRun         bool
}

// ProposalOutcome is the promotion or dismissal receipt. Already reports an
// idempotent rerun.
type ProposalOutcome struct {
	ProjectID      string `json:"project_id"`
	ProposalID     string `json:"proposal_id"`
	ProposalStatus string `json:"proposal_status"`
	RecordKind     string `json:"record_kind,omitempty"`
	RecordID       string `json:"record_id,omitempty"`
	RecordTitle    string `json:"record_title,omitempty"`
	Already        bool   `json:"already"`
	Dismissed      bool   `json:"dismissed"`
	DryRun         bool   `json:"dry_run"`
}

// CatchupQuery selects the project and baseline of a catchup report.
type CatchupQuery struct {
	ProjectID         string
	Since             time.Time
	IncludeUnreviewed bool
	Limit             int
}

// JournalView is one activity line of a catchup report. Commit fields are the
// only map entries; user names are deliberately not exposed.
type JournalView struct {
	Time    time.Time         `json:"time"`
	Event   string            `json:"event"`
	Project string            `json:"project,omitempty"`
	ID      string            `json:"id,omitempty"`
	Title   string            `json:"title,omitempty"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// CatchupView reports what changed in a project since a baseline.
type CatchupView struct {
	Project           Project       `json:"project"`
	Since             time.Time     `json:"since"`
	SinceSource       string        `json:"since_source"`
	BaselineHandoffID string        `json:"baseline_handoff_id,omitempty"`
	State             StateView     `json:"state"`
	StateChanged      bool          `json:"state_changed"`
	Notes             []RecordView  `json:"notes"`
	Decisions         []RecordView  `json:"decisions"`
	Attempts          []RecordView  `json:"attempts"`
	Handoffs          []RecordView  `json:"handoffs"`
	Journal           []JournalView `json:"journal"`
	QuarantinedCount  int           `json:"quarantined_count"`
	IncludeUnreviewed bool          `json:"include_unreviewed"`
}

// MemoryService is the memory surface the desktop uses. Implementations must
// return domain errors unchanged so the handler can map them to the error
// envelope, and must never include secrets or raw-zone bodies.
type MemoryService interface {
	ListRecords(context.Context, RecordQuery) ([]RecordView, error)
	RecordDetail(context.Context, RecordDetailQuery) (RecordDetailView, error)
	Search(context.Context, SearchQuery) (SearchResult, error)
	State(context.Context, string) (StateView, error)
	SetState(context.Context, string, StateWrite) (StateView, error)
	Review(context.Context, string) (ReviewView, error)
	ListProposals(context.Context, ProposalQuery) (ProposalPage, error)
	Promote(context.Context, ProposalDecision) (ProposalOutcome, error)
	Catchup(context.Context, CatchupQuery) (CatchupView, error)
}

// recordPage is the response of GET /v1/records.
type recordPage struct {
	Items []RecordView `json:"items"`
	pageView
}

// proposalPageView is the response of GET /v1/proposals.
type proposalPageView struct {
	Items []ProposalView `json:"items"`
	Total int            `json:"total"`
	pageView
}

// searchPageView is the search-v2 response shape of GET /v1/search.
type searchPageView struct {
	Query   string          `json:"query"`
	Relaxed bool            `json:"relaxed"`
	Notice  string          `json:"notice,omitempty"`
	Hits    []SearchHitView `json:"hits"`
	pageView
}

func (h *Handler) records(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredQuery(w, r, "project")
	if !ok {
		return
	}
	limit, ok := h.queryLimit(w, r, recordsDefaultPageSize, recordsMaxPageSize)
	if !ok {
		return
	}
	status, subtype, topic := queryText(r, "status"), queryText(r, "subtype"), queryText(r, "topic")
	scope, ok := h.queryScope(w, r)
	if !ok {
		return
	}
	kinds := queryKinds(r)
	since, ok := h.querySince(w, r, "since")
	if !ok {
		return
	}
	due, ok := h.queryFlag(w, r, "due_review")
	if !ok {
		return
	}
	offset, filter, ok := h.pageCursor(w, r, "records", recordsWindow, project, strings.Join(kinds, ","), status, subtype, scope, topic, queryText(r, "since"), strconv.FormatBool(due))
	if !ok {
		return
	}
	items, err := h.opts.Memory.ListRecords(r.Context(), RecordQuery{
		ProjectID: project, Kinds: kinds, Status: status, Subtype: subtype,
		Scope: scope, TopicKey: topic, Since: since, DueReview: due,
		Limit: scanLimit(offset, limit, recordsWindow),
	})
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
	page, pageInfo := paginate(items, offset, limit, recordsWindow, "records", filter)
	writeSafeJSON(w, http.StatusOK, recordPage{Items: nonNilSlice(page), pageView: pageInfo}, h.redact)
}

func (h *Handler) recordRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts, ok := pathParts(rest)
	if !ok || len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not_found", "record not found")
		return
	}
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredQuery(w, r, "project")
	if !ok {
		return
	}
	maxTokens, ok := h.queryBoundedInt(w, r, "max_tokens", recordBodyDefaultTokens, 1, recordBodyMaxTokens)
	if !ok {
		return
	}
	history, ok := h.queryFlag(w, r, "history")
	if !ok {
		return
	}
	detail, err := h.opts.Memory.RecordDetail(r.Context(), RecordDetailQuery{
		ProjectID: project, ID: parts[0], MaxTokens: maxTokens, History: history,
	})
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, detail, h.redact)
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredQuery(w, r, "project")
	if !ok {
		return
	}
	query, ok := h.requiredQuery(w, r, "q")
	if !ok {
		return
	}
	limit, ok := h.queryLimit(w, r, searchDefaultPageSize, searchMaxPageSize)
	if !ok {
		return
	}
	mode := strings.ToLower(queryText(r, "mode"))
	if mode != "" && mode != "all" && mode != "any" {
		writeError(w, http.StatusBadRequest, "validation", "mode must be all or any")
		return
	}
	minScore, ok := h.queryScore(w, r)
	if !ok {
		return
	}
	explain, ok := h.queryFlag(w, r, "explain")
	if !ok {
		return
	}
	kinds := queryKinds(r)
	offset, filter, ok := h.pageCursor(w, r, "search", searchWindow, project, query, strings.Join(kinds, ","), mode, strconv.FormatFloat(minScore, 'g', -1, 64), strconv.FormatBool(explain))
	if !ok {
		return
	}
	result, err := h.opts.Memory.Search(r.Context(), SearchQuery{
		Text: query, ProjectID: project, Kinds: kinds, Limit: scanLimit(offset, limit, searchWindow),
		Mode: mode, MinScore: minScore, Explain: explain,
	})
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	page, pageInfo := paginate(result.Hits, offset, limit, searchWindow, "search", filter)
	writeSafeJSON(w, http.StatusOK, searchPageView{
		Query: query, Relaxed: result.Relaxed, Notice: result.Notice, Hits: nonNilSlice(page), pageView: pageInfo,
	}, h.redact)
}

func (h *Handler) projectRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts, ok := pathParts(rest)
	if !ok || len(parts) != 2 || parts[1] != "state" {
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.getState(w, r, parts[0])
	case http.MethodPut:
		h.putState(w, r, parts[0])
	default:
		method(w, r, http.MethodGet, http.MethodPut)
	}
}

func (h *Handler) getState(w http.ResponseWriter, r *http.Request, projectID string) {
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	state, err := h.opts.Memory.State(r.Context(), projectID)
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, state, h.redact)
}

func (h *Handler) putState(w http.ResponseWriter, r *http.Request, projectID string) {
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	var body StateWrite
	if !h.decode(w, r, &body) {
		return
	}
	if body.Empty() {
		writeError(w, http.StatusBadRequest, "validation", "nothing to change; pass at least one field")
		return
	}
	if body.FreshUntil != nil {
		value := strings.TrimSpace(*body.FreshUntil)
		if value != "" {
			if _, err := time.Parse(time.RFC3339, value); err != nil {
				writeError(w, http.StatusBadRequest, "validation", "fresh_until must be an RFC3339 time or an empty string")
				return
			}
		}
		// An empty string clears the freshness window; omitting the field
		// keeps it. The service maps "" to the zero time.
		body.FreshUntil = &value
	}
	state, err := h.opts.Memory.SetState(r.Context(), projectID, body)
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, state, h.redact)
}

func (h *Handler) review(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredQuery(w, r, "project")
	if !ok {
		return
	}
	view, err := h.opts.Memory.Review(r.Context(), project)
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, view, h.redact)
}

func (h *Handler) proposals(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredQuery(w, r, "project")
	if !ok {
		return
	}
	limit, ok := h.queryLimit(w, r, proposalsDefaultPageSize, proposalsMaxPageSize)
	if !ok {
		return
	}
	statuses := queryKindsNamed(r, "status")
	kind := queryText(r, "kind")
	offset, filter, ok := h.pageCursor(w, r, "proposals", proposalsWindow, project, strings.Join(statuses, ","), kind)
	if !ok {
		return
	}
	page, err := h.opts.Memory.ListProposals(r.Context(), ProposalQuery{
		ProjectID: project, Statuses: statuses, Kind: kind,
		Limit: scanLimit(offset, limit, proposalsWindow),
	})
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	items, pageInfo := paginate(page.Items, offset, limit, proposalsWindow, "proposals", filter)
	writeSafeJSON(w, http.StatusOK, proposalPageView{Items: nonNilSlice(items), Total: page.Total, pageView: pageInfo}, h.redact)
}

func (h *Handler) proposalRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts, ok := pathParts(rest)
	if !ok || len(parts) != 3 {
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	projectID, proposalID, action := parts[0], parts[1], parts[2]
	if action != "promote" && action != "dismiss" {
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	if !method(w, r, http.MethodPost) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	decision := ProposalDecision{ProjectID: projectID, ProposalID: proposalID, Dismiss: action == "dismiss"}
	if r.ContentLength != 0 {
		var body struct {
			As             string `json:"as"`
			Status         string `json:"status"`
			Title          string `json:"title"`
			Force          bool   `json:"force"`
			Yes            bool   `json:"yes"`
			AllowDuplicate bool   `json:"allow_duplicate"`
			AllowProtected bool   `json:"allow_protected"`
			DryRun         bool   `json:"dry_run"`
		}
		if !h.decode(w, r, &body) {
			return
		}
		decision.As, decision.Status, decision.Title = body.As, body.Status, body.Title
		decision.Force, decision.Yes = body.Force, body.Yes
		decision.AllowDuplicate, decision.AllowProtected = body.AllowDuplicate, body.AllowProtected
		decision.DryRun = body.DryRun
	}
	outcome, err := h.opts.Memory.Promote(r.Context(), decision)
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, outcome, h.redact)
}

func (h *Handler) catchup(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredQuery(w, r, "project")
	if !ok {
		return
	}
	since, ok := h.querySince(w, r, "since")
	if !ok {
		return
	}
	includeUnreviewed, ok := h.queryFlag(w, r, "include_unreviewed")
	if !ok {
		return
	}
	limit, ok := h.queryBoundedInt(w, r, "limit", catchupDefaultLimit, 1, catchupMaxLimit)
	if !ok {
		return
	}
	view, err := h.opts.Memory.Catchup(r.Context(), CatchupQuery{
		ProjectID: project, Since: since, IncludeUnreviewed: includeUnreviewed, Limit: limit,
	})
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, view, h.redact)
}

// requiredQuery reads a non-empty query parameter.
func (h *Handler) requiredQuery(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	value := queryText(r, name)
	if value == "" {
		writeError(w, http.StatusBadRequest, "validation", name+" is required")
		return "", false
	}
	return value, true
}

// queryLimit reads the page size of a list route.
func (h *Handler) queryLimit(w http.ResponseWriter, r *http.Request, def, maxValue int) (int, bool) {
	return h.queryBoundedInt(w, r, "limit", def, 1, maxValue)
}

// queryBoundedInt reads an optional integer parameter with an inclusive range.
func (h *Handler) queryBoundedInt(w http.ResponseWriter, r *http.Request, name string, def, minValue, maxValue int) (int, bool) {
	raw := queryText(r, name)
	if raw == "" {
		return def, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		writeError(w, http.StatusBadRequest, "validation",
			name+" must be an integer between "+strconv.Itoa(minValue)+" and "+strconv.Itoa(maxValue))
		return 0, false
	}
	return value, true
}

// queryFlag reads an optional boolean parameter.
func (h *Handler) queryFlag(w http.ResponseWriter, r *http.Request, name string) (bool, bool) {
	switch strings.ToLower(queryText(r, name)) {
	case "", "0", "false", "no":
		return false, true
	case "1", "true", "yes":
		return true, true
	default:
		writeError(w, http.StatusBadRequest, "validation", name+" must be 1 or 0")
		return false, false
	}
}

// queryScore reads an optional composite score threshold in [0, 1].
func (h *Handler) queryScore(w http.ResponseWriter, r *http.Request) (float64, bool) {
	raw := queryText(r, "min_score")
	if raw == "" {
		return 0, true
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 || value > 1 {
		writeError(w, http.StatusBadRequest, "validation", "min_score must be a number between 0 and 1")
		return 0, false
	}
	return value, true
}

// queryScope reads the optional record-scope filter.
func (h *Handler) queryScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	scope := strings.ToLower(queryText(r, "scope"))
	switch scope {
	case "", "project", "personal", "global":
		return scope, true
	default:
		writeError(w, http.StatusBadRequest, "validation", "scope must be project, personal or global")
		return "", false
	}
}

// querySince reads an optional since filter: a positive duration (24h, 7d, 2w),
// a date or an RFC3339 time.
func (h *Handler) querySince(w http.ResponseWriter, r *http.Request, name string) (time.Time, bool) {
	value, err := domain.ParseSince(queryText(r, name), h.opts.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation", err.Error())
		return time.Time{}, false
	}
	return value, true
}

// pageCursor resolves the cursor of one list route and binds it to the query.
func (h *Handler) pageCursor(w http.ResponseWriter, r *http.Request, scope string, window int, filterParts ...string) (int, string, bool) {
	filter := filterFingerprint(append([]string{scope}, filterParts...)...)
	offset, ok := decodeCursor(scope, queryText(r, "cursor"), filter)
	if !ok {
		writeError(w, http.StatusBadRequest, "validation", "cursor is not a cursor issued for this query")
		return 0, "", false
	}
	if offset >= window {
		writeError(w, http.StatusBadRequest, "validation",
			"cursor is beyond the "+strconv.Itoa(window)+"-result window; narrow the query and start again")
		return 0, "", false
	}
	return offset, filter, true
}

// queryText returns a trimmed query parameter.
func queryText(r *http.Request, name string) string {
	return strings.TrimSpace(r.URL.Query().Get(name))
}

// queryKinds collects the repeated and comma-separated kind filters.
func queryKinds(r *http.Request) []string { return queryKindsNamed(r, "kind") }

// queryKindsNamed collects a repeated and comma-separated multi-value filter.
func queryKindsNamed(r *http.Request, name string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range r.URL.Query()[name] {
		for _, part := range strings.Split(raw, ",") {
			value := strings.TrimSpace(part)
			if value == "" || seen[value] {
				continue
			}
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// writeSafeJSON redacts every string of a response body, including fields added
// in a future engine build, before it leaves the handler.
func writeSafeJSON(w http.ResponseWriter, status int, body any, redact func(string) string) {
	raw, err := json.Marshal(body)
	if err != nil {
		internal(w)
		return
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		internal(w)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(redactStrings(generic, redact))
}

// redactStrings walks a decoded JSON value and redacts every string.
func redactStrings(value any, redact func(string) string) any {
	switch typed := value.(type) {
	case string:
		return redact(typed)
	case []any:
		for i := range typed {
			typed[i] = redactStrings(typed[i], redact)
		}
		return typed
	case map[string]any:
		for key := range typed {
			typed[key] = redactStrings(typed[key], redact)
		}
		return typed
	default:
		return value
	}
}

// nonNilSlice keeps an empty page a JSON array instead of null.
func nonNilSlice[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
