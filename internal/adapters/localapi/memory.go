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
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Status    string `json:"status,omitempty"`
	Path      string `json:"path"`
	Permalink string `json:"permalink,omitempty"`
	Subtype   string `json:"subtype,omitempty"`
	Scope     string `json:"scope,omitempty"`
	TopicKey  string `json:"topic_key,omitempty"`
	// Tags are the note tags; omitted for every other kind.
	Tags        []string        `json:"tags,omitempty"`
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
	Target string `json:"target"`
	// TargetKind is the kind of the target record when it is still live and
	// known to the project; empty when the target is gone.
	TargetKind string    `json:"target_kind,omitempty"`
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
	// ProjectIDs spans several projects when AllProjects is set. Empty means
	// every project the engine serves.
	ProjectIDs  []string
	AllProjects bool
	Kinds       []string
	Status      string
	Subtype     string
	Scope       string
	TopicKey    string
	Since       time.Time
	DueReview   bool
	Limit       int
}

// RecordDetailQuery selects one record.
type RecordDetailQuery struct {
	ProjectID string
	ID        string
	MaxTokens int
	History   bool
}

// RecordEditRequest changes the text of one record. Nil fields are left alone.
// ExpectedUpdatedAt is zero for a last-write-wins edit; a non-zero value is the
// updated_at the client read and makes the edit a compare-and-swap.
type RecordEditRequest struct {
	ProjectID         string
	ID                string
	Title             *string
	Body              *string
	Tags              *[]string
	ExpectedUpdatedAt time.Time
	AllowDuplicate    bool
}

// SearchQuery is one ranked search request. Raw-zone kinds are never requested,
// so a search can only answer from curated records.
type SearchQuery struct {
	Text string
	// ProjectID selects one project. AllProjects spans ProjectIDs, or every
	// project the engine serves when ProjectIDs is empty.
	ProjectID   string
	ProjectIDs  []string
	AllProjects bool
	Kinds       []string
	Limit       int
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
	// ProjectID names the project of an all-projects review; it is empty when
	// the report covers one project.
	ProjectID string `json:"project_id,omitempty"`
	Kind      string `json:"kind"`
	Severity  string `json:"severity"`
	Message   string `json:"message"`
	Why       string `json:"why"`
	Next      string `json:"next"`
	Confirm   string `json:"confirm"`
	Evidence  string `json:"evidence,omitempty"`
	Fixable   bool   `json:"fixable"`
	Fix       string `json:"fix,omitempty"`
}

// DueReviewView is one record, state or proposal past its review date.
type DueReviewView struct {
	// ProjectID names the project of an all-projects review.
	ProjectID      string    `json:"project_id,omitempty"`
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
	// SnoozedUntil is when a snoozed candidate returns to the inbox.
	// Snoozed reports that the instant is still in the future.
	SnoozedUntil time.Time `json:"snoozed_until,omitzero"`
	Snoozed      bool      `json:"snoozed"`
	// Body is the redacted candidate text, present only when the caller asked
	// for bodies. BodyOmitted marks an item whose body did not fit the page
	// budget; it is never silently empty.
	Body          string `json:"body,omitempty"`
	BodyTruncated bool   `json:"body_truncated,omitempty"`
	BodyTokens    int    `json:"body_tokens,omitempty"`
	MaxTokens     int    `json:"max_tokens,omitempty"`
	BodyOmitted   bool   `json:"body_omitted,omitempty"`
}

// ProposalDetailQuery selects one candidate.
type ProposalDetailQuery struct {
	ProjectID string
	ID        string
	MaxTokens int
}

// ProposalDetailView is one candidate with its bounded body.
type ProposalDetailView struct {
	ID            string    `json:"id"`
	ProjectID     string    `json:"project_id"`
	Kind          string    `json:"kind"`
	Title         string    `json:"title"`
	Status        string    `json:"status"`
	SnoozedUntil  time.Time `json:"snoozed_until,omitzero"`
	Snoozed       bool      `json:"snoozed"`
	UpdatedAt     time.Time `json:"updated_at,omitzero"`
	Body          string    `json:"body"`
	BodyTruncated bool      `json:"body_truncated"`
	BodyTokens    int       `json:"body_tokens"`
	MaxTokens     int       `json:"max_tokens"`
}

// ProposalSummaryView is the pending-proposal summary of a review report. The
// pending count excludes candidates hidden by an active snooze; SnoozedCount
// reports those separately.
type ProposalSummaryView struct {
	PendingCount int            `json:"pending_count"`
	SnoozedCount int            `json:"snoozed_count"`
	Items        []ProposalView `json:"items"`
}

// ReviewView is the read-only review report: findings, due reviews, pending
// proposals and the quarantine size. An all-projects review sets AllProjects,
// names its scope in ProjectIDs and sets ProjectID to AllProjectsProjectID;
// every finding and due review then carries its own project_id.
type ReviewView struct {
	ProjectID        string              `json:"project_id"`
	ProjectIDs       []string            `json:"project_ids,omitempty"`
	AllProjects      bool                `json:"all_projects,omitempty"`
	Findings         []FindingView       `json:"findings"`
	DueReviews       []DueReviewView     `json:"due_reviews"`
	Proposals        ProposalSummaryView `json:"proposals"`
	QuarantinedCount int                 `json:"quarantined_count"`
	// The truncated flags report that the all-projects aggregate hit a cap.
	FindingsTruncated   bool `json:"findings_truncated,omitempty"`
	DueReviewsTruncated bool `json:"due_reviews_truncated,omitempty"`
	ProposalsTruncated  bool `json:"proposals_truncated,omitempty"`
}

// ReviewQuery selects the project scope of a review report.
type ReviewQuery struct {
	ProjectID   string
	ProjectIDs  []string
	AllProjects bool
}

// AllProjectsProjectID is the project_id an all-projects review reports.
const AllProjectsProjectID = "*"

// Caps of an all-projects review. A single-project report is uncapped, matching
// the CLI; the aggregate list stops at these bounds and says so by length.
const (
	AllProjectsFindingsCap   = 1000
	AllProjectsDueReviewsCap = 1000
	AllProjectsProposalsCap  = 100
)

// ReviewFixRequest is one `brain review --fix` equivalent. Apply is the
// explicit confirmation; DryRun is the default and never writes.
type ReviewFixRequest struct {
	ProjectID string
	DryRun    bool
	Apply     bool
	Kinds     []string
}

// ReviewFixChange is one state field a safe review fix writes or would write.
type ReviewFixChange struct {
	Kind     string `json:"kind"`
	Action   string `json:"action"`
	Field    string `json:"field"`
	Value    string `json:"value"`
	Evidence string `json:"evidence,omitempty"`
}

// ReviewFixView is the dry-run or applied result of the safe review fixes.
type ReviewFixView struct {
	ProjectID string            `json:"project_id"`
	DryRun    bool              `json:"dry_run"`
	Findings  []FindingView     `json:"findings"`
	Changes   []ReviewFixChange `json:"changes"`
	State     StateView         `json:"state"`
}

// QuarantineQuery selects one page of the quarantine inbox.
type QuarantineQuery struct {
	ProjectID string
	Limit     int
}

// QuarantineItemView is one quarantined record without its body.
type QuarantineItemView struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	ProjectID  string    `json:"project_id"`
	Title      string    `json:"title"`
	Reason     string    `json:"reason"`
	Origin     string    `json:"origin,omitempty"`
	Trust      string    `json:"trust,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitzero"`
	AgeSeconds int64     `json:"age_seconds"`
}

// QuarantineReviewDecision reviews one quarantined record. Trust optionally
// replaces the stored trust; an empty value keeps it.
type QuarantineReviewDecision struct {
	ProjectID string
	ID        string
	Trust     string
}

// QuarantineReviewView is the record after its review. A record with an
// unknown origin stays quarantined: review cannot repair a malformed origin.
type QuarantineReviewView struct {
	Record     RecordView     `json:"record"`
	Provenance ProvenanceView `json:"provenance"`
}

// ProposalQuery narrows a proposal listing.
type ProposalQuery struct {
	ProjectID   string
	Statuses    []string
	Kind        string
	Limit       int
	IncludeBody bool
	BodyTokens  int
	// IncludeSnoozed keeps candidates hidden by an active snooze.
	IncludeSnoozed bool
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

// ProposalSnoozeRequest sets or clears the review snooze of one candidate.
// Clear wins over Until; ExpectedUpdatedAt is zero for an unconditional write.
type ProposalSnoozeRequest struct {
	ProjectID         string
	ProposalID        string
	Until             time.Time
	Clear             bool
	ExpectedUpdatedAt time.Time
}

// ProposalSnoozeView is the candidate after a snooze write.
type ProposalSnoozeView struct {
	ProjectID    string    `json:"project_id"`
	ProposalID   string    `json:"proposal_id"`
	Status       string    `json:"status"`
	Snoozed      bool      `json:"snoozed"`
	SnoozedUntil time.Time `json:"snoozed_until,omitzero"`
	UpdatedAt    time.Time `json:"updated_at,omitzero"`
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
	EditRecord(context.Context, RecordEditRequest) (RecordDetailView, error)
	TrashRecord(context.Context, string, string) (RecordView, error)
	RestoreRecord(context.Context, string, string) (RecordView, error)
	Search(context.Context, SearchQuery) (SearchResult, error)
	State(context.Context, string) (StateView, error)
	SetState(context.Context, string, StateWrite) (StateView, error)
	Review(context.Context, ReviewQuery) (ReviewView, error)
	ReviewFix(context.Context, ReviewFixRequest) (ReviewFixView, error)
	ListQuarantined(context.Context, QuarantineQuery) ([]QuarantineItemView, error)
	ReviewQuarantine(context.Context, QuarantineReviewDecision) (QuarantineReviewView, error)
	ListProposals(context.Context, ProposalQuery) (ProposalPage, error)
	ProposalDetail(context.Context, ProposalDetailQuery) (ProposalDetailView, error)
	Promote(context.Context, ProposalDecision) (ProposalOutcome, error)
	SnoozeProposal(context.Context, ProposalSnoozeRequest) (ProposalSnoozeView, error)
	Catchup(context.Context, CatchupQuery) (CatchupView, error)
	// ResolveProject maps an id, alias or path to its registered project so
	// the handler can enforce the project allowlist on the canonical id.
	ResolveProject(context.Context, string) (Project, error)
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

// quarantinePage is the response of GET /v1/quarantine.
type quarantinePage struct {
	Items []QuarantineItemView `json:"items"`
	pageView
}

// projectScope is the resolved project scope of a collection route. Exactly one
// of ProjectID and AllProjects is set.
type projectScope struct {
	ProjectID   string
	ProjectIDs  []string
	AllProjects bool
}

// fingerprint binds a page cursor to the scope it was issued for.
func (s projectScope) fingerprint() string {
	if !s.AllProjects {
		return "project:" + s.ProjectID
	}
	return "all:" + strings.Join(s.ProjectIDs, ",")
}

// resolveProjectID maps a selector to its canonical project id and enforces the
// API's project allowlist. A selector cannot escape the allowlist through an
// alias or path because resolution happens first.
func (h *Handler) resolveProjectID(w http.ResponseWriter, r *http.Request, selector string) (string, bool) {
	resolved, err := h.opts.Memory.ResolveProject(r.Context(), selector)
	if err != nil {
		h.writeMemoryError(w, err)
		return "", false
	}
	if !h.projectAllowed(resolved.ID) {
		writeError(w, http.StatusBadRequest, "validation", "project is outside this API's allowed projects")
		return "", false
	}
	return resolved.ID, true
}

// requiredProjectQuery resolves the required project selector of a route.
func (h *Handler) requiredProjectQuery(w http.ResponseWriter, r *http.Request) (string, bool) {
	selector, ok := h.requiredQuery(w, r, "project")
	if !ok {
		return "", false
	}
	return h.resolveProjectID(w, r, selector)
}

// projectScope resolves the project selector of a collection route and enforces
// the API's project allowlist. An unscoped read is allowed only when the policy
// serves every project; a one-project allowlist binds to that project, and a
// wider allowlist requires an explicit project.
func (h *Handler) projectScope(w http.ResponseWriter, r *http.Request) (projectScope, bool) {
	project := queryText(r, "project")
	allProjects, ok := h.queryFlag(w, r, "all_projects")
	if !ok {
		return projectScope{}, false
	}
	if project != "" && allProjects {
		writeError(w, http.StatusBadRequest, "validation", "pass either project or all_projects, not both")
		return projectScope{}, false
	}
	if project != "" {
		resolved, ok := h.resolveProjectID(w, r, project)
		if !ok {
			return projectScope{}, false
		}
		return projectScope{ProjectID: resolved}, true
	}
	switch len(h.opts.ProjectAllowlist) {
	case 0:
		return projectScope{AllProjects: true}, true
	case 1:
		return projectScope{ProjectID: h.opts.ProjectAllowlist[0]}, true
	default:
		writeError(w, http.StatusBadRequest, "validation",
			"project is required: this API is limited to several projects, so an all-projects read is not allowed")
		return projectScope{}, false
	}
}

// projectAllowed reports whether a canonical project id is served.
func (h *Handler) projectAllowed(id string) bool {
	if len(h.opts.ProjectAllowlist) == 0 {
		return true
	}
	for _, allowed := range h.opts.ProjectAllowlist {
		if allowed == id {
			return true
		}
	}
	return false
}

func (h *Handler) records(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	scope, ok := h.projectScope(w, r)
	if !ok {
		return
	}
	limit, ok := h.queryLimit(w, r, recordsDefaultPageSize, recordsMaxPageSize)
	if !ok {
		return
	}
	status, subtype, topic := queryText(r, "status"), queryText(r, "subtype"), queryText(r, "topic")
	recordScope, ok := h.queryScope(w, r)
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
	offset, filter, ok := h.pageCursor(w, r, "records", recordsWindow, scope.fingerprint(), strings.Join(kinds, ","), status, subtype, recordScope, topic, queryText(r, "since"), strconv.FormatBool(due))
	if !ok {
		return
	}
	items, err := h.opts.Memory.ListRecords(r.Context(), RecordQuery{
		ProjectID: scope.ProjectID, ProjectIDs: scope.ProjectIDs, AllProjects: scope.AllProjects,
		Kinds: kinds, Status: status, Subtype: subtype,
		Scope: recordScope, TopicKey: topic, Since: since, DueReview: due,
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
	if !ok || len(parts) == 0 || len(parts) > 2 {
		writeError(w, http.StatusNotFound, "not_found", "record not found")
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "trash":
			if !method(w, r, http.MethodPost) {
				return
			}
			h.recordLifecycle(w, r, parts[0], false)
		case "restore":
			if !method(w, r, http.MethodPost) {
				return
			}
			h.recordLifecycle(w, r, parts[0], true)
		default:
			writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.getRecord(w, r, parts[0])
	case http.MethodPatch:
		h.editRecord(w, r, parts[0])
	default:
		method(w, r, http.MethodGet, http.MethodPatch)
	}
}

func (h *Handler) getRecord(w http.ResponseWriter, r *http.Request, id string) {
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredProjectQuery(w, r)
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
		ProjectID: project, ID: id, MaxTokens: maxTokens, History: history,
	})
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, detail, h.redact)
}

// recordLifecycle trashes or restores one record. Both are reversible state
// moves through the same use case the CLI uses; the API never hard-deletes.
func (h *Handler) recordLifecycle(w http.ResponseWriter, r *http.Request, id string, restore bool) {
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredProjectQuery(w, r)
	if !ok {
		return
	}
	var view RecordView
	var err error
	if restore {
		view, err = h.opts.Memory.RestoreRecord(r.Context(), project, id)
	} else {
		view, err = h.opts.Memory.TrashRecord(r.Context(), project, id)
	}
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, view, h.redact)
}

// recordEditMaxTags bounds the tag list a PATCH may carry. The byte cap is
// enforced by the write limits; this only stops a pathological JSON array
// before it reaches the use case.
const recordEditMaxTags = 256

// editRecord applies PATCH /v1/records/{id}. Only the text fields are
// editable; an expected_updated_at makes the write a compare-and-swap and a
// stale value returns 409 with the current revision. Provenance and review
// state are preserved, so an edit never marks a quarantined record reviewed.
func (h *Handler) editRecord(w http.ResponseWriter, r *http.Request, id string) {
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredProjectQuery(w, r)
	if !ok {
		return
	}
	var body struct {
		Title             *string   `json:"title"`
		Body              *string   `json:"body"`
		Tags              *[]string `json:"tags"`
		ExpectedUpdatedAt *string   `json:"expected_updated_at"`
		AllowDuplicate    bool      `json:"allow_duplicate"`
	}
	if !h.decode(w, r, &body) {
		return
	}
	if body.Title == nil && body.Body == nil && body.Tags == nil {
		writeError(w, http.StatusBadRequest, "validation", "nothing to change; pass title, body or tags")
		return
	}
	if body.Tags != nil && len(*body.Tags) > recordEditMaxTags {
		writeError(w, http.StatusBadRequest, "validation", "tags must name at most 256 values")
		return
	}
	request := RecordEditRequest{
		ProjectID: project, ID: id,
		Title: body.Title, Body: body.Body, Tags: body.Tags,
		AllowDuplicate: body.AllowDuplicate,
	}
	expected, ok := h.expectedUpdatedAt(w, body.ExpectedUpdatedAt)
	if !ok {
		return
	}
	request.ExpectedUpdatedAt = expected
	detail, err := h.opts.Memory.EditRecord(r.Context(), request)
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, detail, h.redact)
}

// snoozeProposal sets or clears one candidate's review snooze. POST carries
// {until, expected_updated_at?}; DELETE, {until:null} or {until:""} clears it.
// The route is idempotent: repeating the stored value writes nothing.
func (h *Handler) snoozeProposal(w http.ResponseWriter, r *http.Request, projectID, proposalID string) {
	if !method(w, r, http.MethodPost, http.MethodDelete) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	projectID, ok := h.resolveProjectID(w, r, projectID)
	if !ok {
		return
	}
	request := ProposalSnoozeRequest{
		ProjectID: projectID, ProposalID: proposalID,
		Clear: r.Method == http.MethodDelete,
	}
	if !request.Clear {
		if r.ContentLength == 0 {
			writeError(w, http.StatusBadRequest, "validation",
				"until is required; pass an RFC3339 time, a date, a duration or null to clear the snooze")
			return
		}
		var body struct {
			Until             json.RawMessage `json:"until"`
			ExpectedUpdatedAt *string         `json:"expected_updated_at"`
		}
		if !h.decode(w, r, &body) {
			return
		}
		until, clear, ok := parseSnoozeUntil(w, body.Until, h.opts.Now())
		if !ok {
			return
		}
		expected, ok := h.expectedUpdatedAt(w, body.ExpectedUpdatedAt)
		if !ok {
			return
		}
		request.Until, request.Clear, request.ExpectedUpdatedAt = until, clear, expected
	}
	view, err := h.opts.Memory.SnoozeProposal(r.Context(), request)
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, view, h.redact)
}

// parseSnoozeUntil decodes the until field of a snooze request. It reports the
// parsed instant, whether the request clears the snooze, and whether the field
// was acceptable.
func parseSnoozeUntil(w http.ResponseWriter, raw json.RawMessage, now time.Time) (time.Time, bool, bool) {
	const message = "until must be an RFC3339 time, a date, a positive duration (7d) or null to clear the snooze"
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		writeError(w, http.StatusBadRequest, "validation", message)
		return time.Time{}, false, false
	}
	if trimmed == "null" {
		return time.Time{}, true, true
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		writeError(w, http.StatusBadRequest, "validation", message)
		return time.Time{}, false, false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}, true, true
	}
	until, err := domain.ParseSnoozeUntil(text, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation", message)
		return time.Time{}, false, false
	}
	return until, false, true
}

// expectedUpdatedAt parses the optional compare-and-swap revision shared by the
// record edit and proposal snooze routes.
func (h *Handler) expectedUpdatedAt(w http.ResponseWriter, raw *string) (time.Time, bool) {
	if raw == nil {
		return time.Time{}, true
	}
	value := strings.TrimSpace(*raw)
	if value == "" {
		writeError(w, http.StatusBadRequest, "validation", "expected_updated_at must be an RFC3339 timestamp or omitted")
		return time.Time{}, false
	}
	expected, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation", "expected_updated_at must be an RFC3339 timestamp or omitted")
		return time.Time{}, false
	}
	return expected.UTC(), true
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	scope, ok := h.projectScope(w, r)
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
	offset, filter, ok := h.pageCursor(w, r, "search", searchWindow, scope.fingerprint(), query, strings.Join(kinds, ","), mode, strconv.FormatFloat(minScore, 'g', -1, 64), strconv.FormatBool(explain))
	if !ok {
		return
	}
	result, err := h.opts.Memory.Search(r.Context(), SearchQuery{
		Text: query, ProjectID: scope.ProjectID, ProjectIDs: scope.ProjectIDs, AllProjects: scope.AllProjects,
		Kinds: kinds, Limit: scanLimit(offset, limit, searchWindow),
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
	projectID, ok := h.resolveProjectID(w, r, projectID)
	if !ok {
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
	projectID, ok := h.resolveProjectID(w, r, projectID)
	if !ok {
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
	scope, ok := h.projectScope(w, r)
	if !ok {
		return
	}
	view, err := h.opts.Memory.Review(r.Context(), ReviewQuery{
		ProjectID: scope.ProjectID, ProjectIDs: scope.ProjectIDs, AllProjects: scope.AllProjects,
	})
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, view, h.redact)
}

// reviewFix is POST /v1/review/fix?project= with an explicit confirmation. The
// default is a dry run; writing needs apply=true, and dry_run=true with apply
// is refused rather than resolved silently.
func (h *Handler) reviewFix(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredProjectQuery(w, r)
	if !ok {
		return
	}
	var body struct {
		DryRun *bool    `json:"dry_run"`
		Apply  bool     `json:"apply"`
		Kinds  []string `json:"kinds"`
	}
	if r.ContentLength != 0 && !h.decode(w, r, &body) {
		return
	}
	if len(body.Kinds) > 32 {
		writeError(w, http.StatusBadRequest, "validation", "kinds must name at most 32 finding kinds")
		return
	}
	if body.Apply && body.DryRun != nil && *body.DryRun {
		writeError(w, http.StatusBadRequest, "validation", "dry_run and apply cannot both be true")
		return
	}
	if !body.Apply && body.DryRun != nil && !*body.DryRun {
		writeError(w, http.StatusBadRequest, "confirmation_required", "apply=true is required to write; omit dry_run or pass dry_run=true to preview")
		return
	}
	apply := body.Apply
	dryRun := !apply
	view, err := h.opts.Memory.ReviewFix(r.Context(), ReviewFixRequest{
		ProjectID: project, DryRun: dryRun, Apply: body.Apply, Kinds: body.Kinds,
	})
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	writeSafeJSON(w, http.StatusOK, view, h.redact)
}

// quarantine is GET /v1/quarantine?project=: the review inbox of records that
// are excluded from trusted context. Bodies stay out of this route.
func (h *Handler) quarantine(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	project, ok := h.requiredProjectQuery(w, r)
	if !ok {
		return
	}
	limit, ok := h.queryLimit(w, r, quarantineDefaultPageSize, quarantineMaxPageSize)
	if !ok {
		return
	}
	offset, filter, ok := h.pageCursor(w, r, "quarantine", quarantineWindow, project)
	if !ok {
		return
	}
	items, err := h.opts.Memory.ListQuarantined(r.Context(), QuarantineQuery{
		ProjectID: project, Limit: scanLimit(offset, limit, quarantineWindow),
	})
	if err != nil {
		h.writeMemoryError(w, err)
		return
	}
	page, pageInfo := paginate(items, offset, limit, quarantineWindow, "quarantine", filter)
	writeSafeJSON(w, http.StatusOK, quarantinePage{Items: nonNilSlice(page), pageView: pageInfo}, h.redact)
}

func (h *Handler) quarantineRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts, ok := pathParts(rest)
	if !ok || len(parts) != 2 || parts[1] != "review" {
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
	project, ok := h.requiredProjectQuery(w, r)
	if !ok {
		return
	}
	var body struct {
		Trust string `json:"trust"`
	}
	if r.ContentLength != 0 && !h.decode(w, r, &body) {
		return
	}
	view, err := h.opts.Memory.ReviewQuarantine(r.Context(), QuarantineReviewDecision{
		ProjectID: project, ID: parts[0], Trust: body.Trust,
	})
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
	project, ok := h.requiredProjectQuery(w, r)
	if !ok {
		return
	}
	limit, ok := h.queryLimit(w, r, proposalsDefaultPageSize, proposalsMaxPageSize)
	if !ok {
		return
	}
	statuses := queryKindsNamed(r, "status")
	kind := queryText(r, "kind")
	includeBody, ok := h.queryFlag(w, r, "include_body")
	if !ok {
		return
	}
	includeSnoozed, ok := h.queryFlag(w, r, "include_snoozed")
	if !ok {
		return
	}
	maxTokens, ok := h.queryBoundedInt(w, r, "max_tokens", proposalBodyDefaultTokens, 1, proposalBodyMaxTokens)
	if !ok {
		return
	}
	// include_snoozed joins the cursor fingerprint only when set, so cursors
	// issued before the field existed keep working for the default listing.
	cursorFilter := []string{project, strings.Join(statuses, ","), kind, strconv.FormatBool(includeBody), strconv.Itoa(maxTokens)}
	if includeSnoozed {
		cursorFilter = append(cursorFilter, "include_snoozed=1")
	}
	offset, filter, ok := h.pageCursor(w, r, "proposals", proposalsWindow, cursorFilter...)
	if !ok {
		return
	}
	page, err := h.opts.Memory.ListProposals(r.Context(), ProposalQuery{
		ProjectID: project, Statuses: statuses, Kind: kind,
		Limit:       scanLimit(offset, limit, proposalsWindow),
		IncludeBody: includeBody, BodyTokens: maxTokens, IncludeSnoozed: includeSnoozed,
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
	if !ok || len(parts) < 2 || len(parts) > 3 {
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	projectID, proposalID := parts[0], parts[1]
	if len(parts) == 2 {
		if !method(w, r, http.MethodGet) {
			return
		}
		if h.opts.Memory == nil {
			unavailable(w)
			return
		}
		projectID, ok := h.resolveProjectID(w, r, projectID)
		if !ok {
			return
		}
		maxTokens, ok := h.queryBoundedInt(w, r, "max_tokens", proposalBodyDefaultTokens, 1, recordBodyMaxTokens)
		if !ok {
			return
		}
		detail, err := h.opts.Memory.ProposalDetail(r.Context(), ProposalDetailQuery{
			ProjectID: projectID, ID: proposalID, MaxTokens: maxTokens,
		})
		if err != nil {
			h.writeMemoryError(w, err)
			return
		}
		writeSafeJSON(w, http.StatusOK, detail, h.redact)
		return
	}
	action := parts[2]
	if action != "promote" && action != "dismiss" && action != "snooze" {
		writeError(w, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	if action == "snooze" {
		h.snoozeProposal(w, r, projectID, proposalID)
		return
	}
	if !method(w, r, http.MethodPost) {
		return
	}
	if h.opts.Memory == nil {
		unavailable(w)
		return
	}
	projectID, ok = h.resolveProjectID(w, r, projectID)
	if !ok {
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
	project, ok := h.requiredProjectQuery(w, r)
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
