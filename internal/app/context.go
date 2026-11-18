package app

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
)

// Record kinds that brain context ranks. Handoffs and state have their own
// sections; connector dumps are never part of the context.
const (
	ContextKindDecision = "decision"
	ContextKindAttempt  = "attempt"
	ContextKindNote     = "note"
)

// ContextKinds lists the kinds accepted by ContextOptions.Kinds.
var ContextKinds = []string{ContextKindDecision, ContextKindAttempt, ContextKindNote}

const (
	// contextSearchLimit is how many hits are fetched before re-ranking.
	contextSearchLimit = 50
	// contextMaxFindings bounds the review findings section.
	contextMaxFindings = 5
	// contextRecencyDays is the age at which the recency signal halves.
	contextRecencyDays = 30.0
)

// Ranking weights: search rank dominates, recency and kind break near ties.
const (
	contextWeightRank    = 0.6
	contextWeightRecency = 0.25
	contextWeightKind    = 0.15
)

var contextKindWeights = map[string]float64{
	ContextKindDecision: 1.0,
	ContextKindAttempt:  0.7,
	ContextKindNote:     0.4,
}

// ContextOptions selects the task context of a project.
type ContextOptions struct {
	ProjectID string
	// Query describes the task; it drives the search for relevant records.
	Query string
	// Budget bounds Summary in runes; zero applies no rune cap and lets the
	// token budget below decide the size.
	Budget int
	// TokenBudget is the total budget of the layered packer in tokens. Zero
	// resolves to the context.budget configuration, then to
	// DefaultContextBudgetTokens.
	TokenBudget int
	// Kinds keeps only these record kinds (decision, attempt, note) in the
	// relevant records; empty keeps all three. Failed attempts matching the
	// task are listed regardless.
	Kinds []string
	// Subtype, Scope and TopicKey narrow the relevant records to one
	// classification. Empty keeps every value.
	Subtype  string
	Scope    string
	TopicKey string
	// Since drops records and handoffs whose content did not change within a
	// window (30m, 24h, 7d, 2w) or after a date (2026-11-01, RFC 3339). A
	// metadata-only write (review, snooze, validity) does not make a record
	// recent. Empty keeps everything.
	Since string
	// Raw requests raw-zone excerpts from explicitly granted connections when
	// the curated context is thin. Grants are default-deny (context-allow.json).
	Raw bool
	// IncludeUnreviewed includes quarantined records (unreviewed agent,
	// connector or import writes, or an unknown origin). They are excluded
	// from trusted context by default.
	IncludeUnreviewed bool
	// AsOf gathers the context as it was valid at that instant instead of now.
	AsOf time.Time
	// IncludeSuperseded keeps records whose validity ended. An AsOf instant
	// selects the point in time and takes precedence.
	IncludeSuperseded bool
}

// ContextRecord is one record relevant to the task.
type ContextRecord struct {
	ID        string
	Kind      string
	Title     string
	Status    string // decisions only
	Detail    string // first line of the context, reason or body
	CreatedAt time.Time
	UpdatedAt time.Time
	// ContentUpdatedAt is the last title or body change; Since compares it.
	ContentUpdatedAt time.Time
	// Path is the logical data-directory path of the record.
	Path  string
	Score float64
	// Validity is the record's temporal validity; the default context keeps
	// current records only.
	Validity domain.Validity
	// Quarantined marks a record that is shown only because the caller asked
	// for unreviewed records. Its text is not trusted.
	Quarantined bool
	// Pinned is the explicit pin flag set by the owner.
	Pinned bool
}

// ContextExcerpt is one raw-zone item body allowed by context-allow.json.
// Bodies are re-redacted at read time and marked untrusted: they are external
// text, not curated knowledge.
type ContextExcerpt struct {
	ConnectionID string
	ProjectID    string
	ItemKey      string
	ExternalID   string
	Kind         string
	Title        string
	URL          string
	UpdatedAt    time.Time
	Citation     string
	Body         string
	Truncated    bool
}

// ContextResult is the task context of a project. Summary is the Markdown
// rendering bounded by Budget; the other fields carry the same content.
type ContextResult struct {
	Project domain.Project
	Mode    string
	Query   string
	Since   time.Time // zero when not filtered
	// AsOf is the instant the records and the handoff were selected at. State
	// and review findings have no history: they always describe the current
	// data, which the summary and the JSON label report.
	AsOf   time.Time
	Budget int
	State  domain.State
	// Handoff is the newest handoff (within Since), nil when none.
	Handoff *domain.Handoff
	// Records are the top-ranked decisions, attempts and notes.
	Records []ContextRecord
	// FailedAttempts are further attempts matching the task, newest first.
	FailedAttempts []ContextRecord
	Findings       []Finding
	// PendingProposals counts pending candidates; nil when the proposal store
	// is unavailable. Context never includes proposal bodies.
	PendingProposals *int
	// Quarantined lists the ids of records excluded from trusted context.
	Quarantined []string
	// Excluded lists every record left out of the answer with the stable
	// reason code. It is the record-level view of Pack.Truncated.
	Excluded []ContextExclusion
	// ExcludedTotal counts every excluded record, even when a transport caps
	// the list.
	ExcludedTotal int
	// NearMisses are the top records that matched the task but ranked below
	// the budget cut, with their scores.
	NearMisses []ContextExclusion
	// StateQuarantined reports that the project state is quarantined and was
	// not injected as trusted text.
	StateQuarantined bool
	// RawExcerpts are granted raw-zone excerpts, ordered by search relevance.
	// Empty unless Raw was requested, grants match and curated context is thin.
	RawExcerpts []ContextExcerpt
	// IncludeUnreviewed reports that quarantined records were included in the
	// summary at the caller's request.
	IncludeUnreviewed bool
	// Pack is the layered token budget accounting of this answer.
	Pack ContextPack
	// Skipped names the parts that are unavailable in this mode, with why.
	Skipped []string
	Summary string
}

// Context gathers what an agent needs for one task: project state, review
// findings, the latest handoff, the records the search ranks highest for the
// task, and failed attempts matching it. Search or review that is not
// available in the current mode is skipped and reported in Skipped.
func (a *App) Context(ctx context.Context, opts ContextOptions) (ContextResult, error) {
	if err := a.requireStore(); err != nil {
		return ContextResult{}, err
	}
	query := strings.TrimSpace(opts.Query)
	if query == "" {
		return ContextResult{}, domain.NewValidationError("context.query", "is required")
	}
	kinds, err := normalizeContextKinds(opts.Kinds)
	if err != nil {
		return ContextResult{}, err
	}
	tokenBudget, err := a.contextTokenBudget(opts.TokenBudget)
	if err != nil {
		return ContextResult{}, err
	}
	now := a.now()
	since, err := domain.ParseSince(opts.Since, now)
	if err != nil {
		return ContextResult{}, err
	}
	filter := validityFilter(opts.AsOf, opts.IncludeSuperseded)
	subtype, scope, topic, err := validateClassificationFilters(opts.Subtype, opts.Scope, opts.TopicKey)
	if err != nil {
		return ContextResult{}, err
	}
	opts.Subtype, opts.Scope, opts.TopicKey = subtype, scope, topic
	project, err := a.resolveProject(ctx, opts.ProjectID)
	if err != nil {
		return ContextResult{}, err
	}
	state, err := a.loadState(ctx, project.ID)
	if err != nil {
		return ContextResult{}, err
	}
	catalog, err := a.contextCatalog(ctx, project.ID)
	if err != nil {
		return ContextResult{}, err
	}
	handoffs, err := a.listHandoffs(ctx, project.ID)
	if err != nil {
		return ContextResult{}, err
	}
	quarantined := append([]string{}, catalog.quarantined...)
	for _, handoff := range handoffs {
		if handoff.Quarantined() {
			quarantined = append(quarantined, handoff.ID)
		}
	}
	sort.Strings(quarantined)
	handoff := latestHandoff(filterHandoffs(handoffs, filter, now), since, opts.IncludeUnreviewed)

	result := ContextResult{
		Project: project, Mode: a.cfg.Mode, Query: query, Since: since, AsOf: opts.AsOf,
		Budget: opts.Budget, State: state, Handoff: handoff, Quarantined: quarantined,
		StateQuarantined:  state.Provenance.Quarantined(),
		IncludeUnreviewed: opts.IncludeUnreviewed,
	}
	if result.Budget < 0 {
		result.Budget = 0
	}
	ranking := contextRanking{
		catalog: catalog, projectID: project.ID, since: since, now: now,
		includeUnreviewed: opts.IncludeUnreviewed, filter: filter,
	}

	hits, err := a.contextSearch(ctx, query, project.ID, kinds, since, opts)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ContextResult{}, ctxErr
		}
		result.Skipped = append(result.Skipped, "relevant records: "+err.Error())
	} else {
		// No fixed record ceiling: the budget decides how many relevant
		// records fit, and the packer reports the rest as dropped.
		result.Records = ranking.rank(hits, nil, 0)
	}
	// Failed attempts are listed whatever Kinds says: they warn against
	// repeating an approach that already failed.
	if err == nil {
		attemptHits, attemptErr := a.contextSearch(ctx, query, project.ID, []string{ContextKindAttempt}, since, opts)
		if attemptErr != nil {
			result.Skipped = append(result.Skipped, "failed attempts: "+attemptErr.Error())
		} else {
			result.FailedAttempts = ranking.newestAttempts(attemptHits, recordIDs(result.Records), 0)
		}
	}

	findings, err := a.Findings(ctx, project.ID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ContextResult{}, ctxErr
		}
		result.Skipped = append(result.Skipped, "review findings: "+err.Error())
	} else {
		result.Findings = findings
	}
	if a.deps.Proposals != nil {
		count, countErr := a.pendingProposalCount(ctx, project.ID)
		if countErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ContextResult{}, ctxErr
			}
			result.Skipped = append(result.Skipped, "pending proposals: "+countErr.Error())
		} else {
			result.PendingProposals = &count
		}
	}
	pack, packErr := a.packContext(ctx, result, &ranking, kinds, tokenBudget, now)
	if packErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ContextResult{}, ctxErr
		}
		result.Skipped = append(result.Skipped, "linked records: "+packErr.Error())
	}
	result.Pack = pack
	applyContextExclusions(&result)
	failedIDs := recordIDs(result.FailedAttempts)
	result.Records, result.FailedAttempts = splitQuerySection(pack, failedIDs)
	if opts.Raw {
		curated := len(result.Records) + len(result.FailedAttempts) + len(result.Findings)
		excerpts, skipped, excerptErr := a.contextExcerpts(ctx, project, query, since, now, curated)
		if excerptErr != nil {
			return ContextResult{}, excerptErr
		}
		result.RawExcerpts = excerpts
		result.Skipped = append(result.Skipped, skipped...)
	}

	summary := fitContextPack(&result, a.revision(ctx), now, failedIDs)
	if result.Budget > 0 {
		summary = truncateSummary(summary, result.Budget)
	}
	result.Summary = summary
	return result, nil
}

func (a *App) revision(ctx context.Context) string {
	revision, err := a.deps.Store.CurrentRevision(ctx)
	if err != nil {
		return ""
	}
	return revision.Value
}

func (a *App) contextSearch(ctx context.Context, query, projectID string, kinds []string, since time.Time, opts ContextOptions) ([]ports.Hit, error) {
	return a.SearchWith(ctx, SearchOptions{
		Text: query, ProjectID: projectID, Limit: contextSearchLimit, Kinds: kinds, Since: since,
		AsOf: opts.AsOf, IncludeSuperseded: opts.IncludeSuperseded,
		Subtype: opts.Subtype, Scope: opts.Scope, TopicKey: opts.TopicKey,
	})
}

// latestHandoff returns the newest handoff not older than since. Quarantined
// handoffs are skipped unless includeUnreviewed is set.
func latestHandoff(handoffs []domain.Handoff, since time.Time, includeUnreviewed bool) *domain.Handoff {
	var latest *domain.Handoff
	for i := range handoffs {
		handoff := handoffs[i]
		if !includeUnreviewed && handoff.Quarantined() {
			continue
		}
		if !withinSince(handoff.ContentUpdatedAt, handoff.UpdatedAt, handoff.CreatedAt, since) {
			continue
		}
		if latest == nil || handoff.CreatedAt.After(latest.CreatedAt) {
			latest = &handoff
		}
	}
	return latest
}

// contextCatalog maps record ids to their details. A kind whose store cannot
// list records is absent from listed, so hits of that kind are kept as the
// index returned them.
type contextCatalog struct {
	records     map[string]ContextRecord
	listed      map[string]bool
	quarantined []string
}

func (a *App) contextCatalog(ctx context.Context, projectID string) (contextCatalog, error) {
	catalog := contextCatalog{records: map[string]ContextRecord{}, listed: map[string]bool{}}
	if _, ok := a.deps.Store.(decisionLister); ok {
		catalog.listed[ContextKindDecision] = true
		decisions, err := a.listDecisions(ctx, projectID)
		if err != nil {
			return contextCatalog{}, err
		}
		for _, decision := range decisions {
			catalog.records[decision.ID] = ContextRecord{
				ID: decision.ID, Kind: ContextKindDecision, Title: decision.Title, Status: string(decision.Status),
				Detail: SummarizeLine(decision.Context, promptLineRunes), CreatedAt: decision.CreatedAt,
				UpdatedAt: decision.UpdatedAt, ContentUpdatedAt: decision.ContentUpdatedAt,
				Path:     contextRecordPath(projectID, ContextKindDecision, decision.ID),
				Validity: decision.Validity, Quarantined: decision.Quarantined(),
			}
			if decision.Quarantined() {
				catalog.quarantined = append(catalog.quarantined, decision.ID)
			}
		}
	}
	if _, ok := a.deps.Store.(attemptLister); ok {
		catalog.listed[ContextKindAttempt] = true
		attempts, err := a.listAttempts(ctx, projectID)
		if err != nil {
			return contextCatalog{}, err
		}
		for _, attempt := range attempts {
			catalog.records[attempt.ID] = ContextRecord{
				ID: attempt.ID, Kind: ContextKindAttempt, Title: attempt.Title,
				Detail: attemptDetail(attempt), CreatedAt: attempt.CreatedAt,
				UpdatedAt: attempt.UpdatedAt, ContentUpdatedAt: attempt.ContentUpdatedAt,
				Path:     contextRecordPath(projectID, ContextKindAttempt, attempt.ID),
				Validity: attempt.Validity, Quarantined: attempt.Quarantined(),
			}
			if attempt.Quarantined() {
				catalog.quarantined = append(catalog.quarantined, attempt.ID)
			}
		}
	}
	if _, ok := a.deps.Store.(noteLister); ok {
		catalog.listed[ContextKindNote] = true
		notes, err := a.listNotes(ctx, projectID)
		if err != nil {
			return contextCatalog{}, err
		}
		for _, note := range notes {
			catalog.records[note.ID] = ContextRecord{
				ID: note.ID, Kind: ContextKindNote, Title: valueOr(note.Title, SummarizeLine(note.Body, 80)),
				Detail: SummarizeLine(note.Body, promptLineRunes), CreatedAt: note.CreatedAt,
				UpdatedAt: note.UpdatedAt, ContentUpdatedAt: note.ContentUpdatedAt,
				Path:     contextRecordPath(projectID, ContextKindNote, note.ID),
				Validity: note.Validity, Quarantined: note.Quarantined(),
			}
			if note.Quarantined() {
				catalog.quarantined = append(catalog.quarantined, note.ID)
			}
		}
	}
	return catalog, nil
}

func attemptDetail(attempt domain.Attempt) string {
	for _, text := range []string{attempt.Reason, attempt.Result, attempt.Hypothesis} {
		if line := SummarizeLine(text, promptLineRunes); line != "" {
			return line
		}
	}
	return ""
}

// lookup returns the record behind a hit. ok is false for a hit that must be
// dropped: a record the store lists no longer (trashed, stale index).
func (c contextCatalog) lookup(hit ports.Hit) (ContextRecord, bool) {
	if record, found := c.records[hit.ID]; found && record.Kind == hit.Kind {
		return record, true
	}
	if c.listed[hit.Kind] {
		return ContextRecord{}, false
	}
	return ContextRecord{ID: hit.ID, Kind: hit.Kind, Title: hit.Title, Detail: SummarizeLine(hit.Snippet, promptLineRunes)}, true
}

type contextRanking struct {
	catalog           contextCatalog
	projectID         string
	since             time.Time
	now               time.Time
	includeUnreviewed bool
	filter            domain.ValidityFilter
	// dropped is the exclusion bookkeeping the filters fill before packing.
	dropped []ContextTruncation
	// droppedIDs keeps one reason per record, whichever stage dropped it
	// first: the filter reasons are more specific than a later budget miss.
	droppedIDs map[string]bool
}

// drop records why one record is not part of the answer. The first reason
// wins, so a record is never listed twice.
func (r *contextRanking) drop(record ContextRecord, section, code, detail string) {
	if record.ID == "" {
		return
	}
	if r.droppedIDs == nil {
		r.droppedIDs = map[string]bool{}
	}
	if r.droppedIDs[record.ID] {
		return
	}
	r.droppedIDs[record.ID] = true
	r.dropped = append(r.dropped, ContextTruncation{
		ID: record.ID, Kind: record.Kind, Section: section, Code: code,
		Reason: code + ": " + detail, Score: record.Score,
	})
}

// admit reports why the filters keep a record out of the context. An empty
// code means the record is admitted.
func (r *contextRanking) admit(record ContextRecord) (code, detail string) {
	if record.Quarantined && !r.includeUnreviewed {
		return ContextExcludeQuarantined, "unreviewed write; include_unreviewed would show it"
	}
	at := r.filter.Now(r.now)
	if !r.filter.Allows(record.Validity, record.CreatedAt, r.now) {
		if record.Validity.EndedAt(at) {
			detail := "validity ended"
			if record.Validity.SupersededBy != "" {
				detail = "replaced by " + record.Validity.SupersededBy
			}
			return ContextExcludeSuperseded, detail
		}
		return ContextExcludeStale, "not valid at the requested instant"
	}
	if !r.since.IsZero() && !withinSince(record.ContentUpdatedAt, record.UpdatedAt, record.CreatedAt, r.since) {
		return ContextExcludeStale, "content is older than the since window"
	}
	return "", ""
}

// candidates resolves hits to records in search order, dropping unknown
// records, records older than since, quarantined records the caller did not
// ask for, ids in exclude and duplicate ids. Every dropped record is recorded
// with its reason; an id in exclude is already shown elsewhere, not dropped.
func (r *contextRanking) candidates(hits []ports.Hit, exclude map[string]bool, layer string) []ContextRecord {
	seen := map[string]bool{}
	out := make([]ContextRecord, 0, len(hits))
	for _, hit := range hits {
		if seen[hit.ID] {
			continue
		}
		seen[hit.ID] = true
		if exclude[hit.ID] {
			continue
		}
		record, ok := r.catalog.lookup(hit)
		if !ok {
			r.drop(ContextRecord{ID: hit.ID, Kind: hit.Kind, Title: hit.Title}, layer, ContextExcludeStale,
				"the index still lists it but the store no longer has it (stale index row)")
			continue
		}
		if code, detail := r.admit(record); code != "" {
			r.drop(record, layer, code, detail)
			continue
		}
		if record.Path == "" {
			record.Path = contextRecordPath(r.projectID, record.Kind, record.ID)
		}
		out = append(out, record)
	}
	return out
}

// rank scores the hits by search rank, recency and kind and keeps the best
// limit records. Ties fall back to the id, so the order is deterministic.
func (r *contextRanking) rank(hits []ports.Hit, exclude map[string]bool, limit int) []ContextRecord {
	records := r.candidates(hits, exclude, ContextLayerQuery)
	for i := range records {
		records[i].Score = contextScore(i, len(records), records[i], r.now)
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Score != records[j].Score {
			return records[i].Score > records[j].Score
		}
		return records[i].ID < records[j].ID
	})
	return headRecords(records, limit)
}

// newestAttempts lists matching attempts not shown yet, newest first.
func (r *contextRanking) newestAttempts(hits []ports.Hit, exclude map[string]bool, limit int) []ContextRecord {
	records := r.candidates(hits, exclude, ContextLayerQuery)
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].CreatedAt.After(records[j].CreatedAt)
		}
		return records[i].ID < records[j].ID
	})
	return headRecords(records, limit)
}

func headRecords(records []ContextRecord, limit int) []ContextRecord {
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	return records
}

// contextScore combines the search position (1 for the best hit), the
// record age and its kind into one value in [0, 1].
func contextScore(position, total int, record ContextRecord, now time.Time) float64 {
	relevance := 1 - float64(position)/float64(total)
	recency := 0.0
	if !record.CreatedAt.IsZero() {
		ageDays := now.Sub(record.CreatedAt).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		recency = 1 / (1 + ageDays/contextRecencyDays)
	}
	score := contextWeightRank*relevance + contextWeightRecency*recency + contextWeightKind*contextKindWeights[record.Kind]
	return math.Round(score*1000) / 1000
}

func recordIDs(records []ContextRecord) map[string]bool {
	ids := make(map[string]bool, len(records))
	for _, record := range records {
		ids[record.ID] = true
	}
	return ids
}

// applyContextExclusions refreshes the record-level exclusion view of the pack:
// every dropped record with its reason, minus the ones another layer packed,
// plus the near misses among the records that missed the query cut.
func applyContextExclusions(result *ContextResult) {
	packed := make(map[string]bool, 64)
	for _, section := range result.Pack.Sections {
		for _, record := range section.Records {
			packed[record.ID] = true
		}
	}
	excluded := exclusionsFromTruncations(result.Pack.Truncated)
	kept := make([]ContextExclusion, 0, len(excluded))
	near := make([]ContextExclusion, 0, contextNearMissLimit)
	for _, exclusion := range excluded {
		if packed[exclusion.ID] {
			continue
		}
		kept = append(kept, exclusion)
		if exclusion.Section == ContextLayerQuery && contextNearMissReason(exclusion.Reason) {
			near = append(near, exclusion)
		}
	}
	if len(near) > contextNearMissLimit {
		near = near[:contextNearMissLimit]
	}
	result.Excluded, result.ExcludedTotal, result.NearMisses = kept, len(kept), near
}

func normalizeContextKinds(values []string) ([]string, error) {
	var kinds []string
	for _, value := range values {
		kind := strings.ToLower(strings.TrimSpace(value))
		if kind == "" {
			continue
		}
		if !slices.Contains(ContextKinds, kind) {
			return nil, domain.NewValidationError("context.kind",
				fmt.Sprintf("unknown kind %q (known: %s)", value, strings.Join(ContextKinds, ", ")))
		}
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return slices.Clone(ContextKinds), nil
	}
	return kinds, nil
}

// renderContextHeader writes the project identity and task metadata. It is not
// part of a layer, but its tokens count against the core cap in the packer.
func renderContextHeader(result ContextResult, revision string) string {
	var out strings.Builder
	name := valueOr(result.Project.Name, result.Project.ID)
	fmt.Fprintf(&out, "# %s — context\n", name)
	fmt.Fprintf(&out, "project_id: %s\n", result.Project.ID)
	fmt.Fprintf(&out, "mode: %s\n", result.Mode)
	if revision != "" {
		fmt.Fprintf(&out, "revision: %s\n", revision)
	}
	fmt.Fprintf(&out, "task: %s\n", SummarizeLine(result.Query, promptLineRunes))
	if !result.Since.IsZero() {
		fmt.Fprintf(&out, "since: %s\n", formatTime(result.Since))
	}
	if !result.AsOf.IsZero() {
		fmt.Fprintf(&out, "as_of: %s\n", formatTime(result.AsOf))
		// State and findings have no history, so the answer must say that the
		// past-instant request did not reach them.
		fmt.Fprintf(&out, "state_as_of: current (state and review findings describe the current data)\n")
	}
	if result.PendingProposals != nil {
		fmt.Fprintf(&out, "pending_proposals: %d\n", *result.PendingProposals)
	}
	if line := quarantineLine(result.Quarantined, result.IncludeUnreviewed); line != "" {
		out.WriteString(line + "\n")
	}
	for _, skipped := range result.Skipped {
		fmt.Fprintf(&out, "skipped: %s\n", SummarizeLine(skipped, promptLineRunes))
	}
	return out.String()
}

// writeContextState writes the project state block of the core layer.
func writeContextState(out *strings.Builder, state domain.State, now time.Time, includeUnreviewed bool) {
	fmt.Fprintf(out, "state_updated: %s (age %s)\n", formatTime(state.UpdatedAt), stateAge(now, state))
	fmt.Fprintf(out, "freshness: %s\n", freshness(now, state))
	if state.Provenance.Quarantined() {
		fmt.Fprintf(out, "state_quarantined: true\n")
	}
	if state.Provenance.Quarantined() && !includeUnreviewed {
		fmt.Fprintf(out, "goal: (quarantined state omitted)\n")
		fmt.Fprintf(out, "next_action: (quarantined state omitted)\n")
		writePromptList(out, "active", nil)
		writePromptList(out, "blocked", nil)
		writePromptList(out, "open_questions", nil)
		return
	}
	fmt.Fprintf(out, "goal: %s\n", valueOr(state.Goal, "not set"))
	fmt.Fprintf(out, "next_action: %s\n", valueOr(state.NextAction, "not set"))
	writePromptList(out, "active", state.Active)
	writePromptList(out, "blocked", state.Blocked)
	writePromptList(out, "open_questions", state.OpenQuestions)
}

// renderContextCore renders the always-present core layer (state, latest
// handoff, review findings) and the record metadata that accounts for it.
func renderContextCore(result ContextResult, now time.Time) (string, []ContextSectionRecord) {
	var out strings.Builder
	writeContextState(&out, result.State, now, result.IncludeUnreviewed)
	stateTokens := textbudget.Tokens(out.String())
	records := []ContextSectionRecord{{
		ID: "state", Kind: "state", Title: "Project state", Path: "state.md",
		Tokens: stateTokens, Score: 1,
	}}

	start := out.Len()
	writeContextHandoff(&out, result.Handoff)
	handoffTokens := textbudget.Tokens(out.String()[start:])
	if result.Handoff != nil {
		records = append(records, ContextSectionRecord{
			ID: result.Handoff.ID, Kind: "handoff", Title: handoffTitle(*result.Handoff),
			Path:      recordDataPath(result.Project.ID, "handoff", result.Handoff.ID+".md"),
			CreatedAt: result.Handoff.CreatedAt, UpdatedAt: result.Handoff.CreatedAt,
			Tokens: handoffTokens, Score: 1,
		})
	}

	start = out.Len()
	writeContextFindings(&out, result.Findings)
	findingsTokens := textbudget.Tokens(out.String()[start:])
	if len(result.Findings) > 0 {
		records = append(records, ContextSectionRecord{
			ID: "review-findings", Kind: "finding", Title: fmt.Sprintf("Review findings (%d)", len(result.Findings)),
			Path: "review", Tokens: findingsTokens, Score: 1,
		})
	}
	return out.String(), records
}

// renderContext writes the Markdown context from the packed sections. Layers
// are ordered by how much they matter, so a reader meets the state, the latest
// handoff and the records that match the task first.
func renderContext(result ContextResult, revision string, now time.Time) string {
	var out strings.Builder
	out.WriteString(renderContextHeader(result, revision))
	if core, ok := contextSection(result.Pack, ContextLayerCore); ok {
		out.WriteString(core.Text)
	} else {
		coreText, _ := renderContextCore(result, now)
		out.WriteString(coreText)
	}
	out.WriteString("\n## Relevant records\n")
	writeContextRecords(&out, result.Records, "- none matched the task\n")
	out.WriteString("\n## Failed attempts matching the task\n")
	writeContextRecords(&out, result.FailedAttempts, "- none beyond the relevant records\n")
	writeContextDropped(&out, result.Excluded, ContextLayerQuery)
	for _, layer := range []struct {
		name    string
		heading string
	}{
		{ContextLayerDecisions, "\n## Current decisions\n"},
		{ContextLayerAttempts, "\n## Recent attempts\n"},
		{ContextLayerLinked, "\n## Linked records\n"},
	} {
		records := sectionRecords(result.Pack, layer.name)
		dropped := contextDroppedCount(result.Pack.Truncated, layer.name)
		if len(records) == 0 && dropped == 0 {
			continue
		}
		out.WriteString(layer.heading)
		writeContextRecords(&out, records, "")
		writeContextDropped(&out, result.Excluded, layer.name)
	}
	writeContextExcerpts(&out, result.RawExcerpts)
	return out.String()
}

// writeContextDropped reports how many records of one layer did not make it
// and why, so a reader can tell a short layer from a complete one. The
// near-miss line follows the query layer.
func writeContextDropped(out *strings.Builder, excluded []ContextExclusion, layer string) {
	out.WriteString(contextDroppedText(excluded, layer))
}

func writeContextFindings(out *strings.Builder, findings []Finding) {
	out.WriteString("\n## Review findings\n")
	if len(findings) == 0 {
		out.WriteString("- none\n")
		return
	}
	for i, finding := range findings {
		if i == contextMaxFindings {
			fmt.Fprintf(out, "- … %d more (brain review)\n", len(findings)-i)
			break
		}
		fmt.Fprintf(out, "- [%s] %s: %s\n", finding.Severity, finding.Kind, SummarizeLine(finding.Message, promptLineRunes))
	}
}

func writeContextHandoff(out *strings.Builder, handoff *domain.Handoff) {
	out.WriteString("\n## Latest handoff\n")
	if handoff == nil {
		out.WriteString("- none recorded\n")
		return
	}
	fmt.Fprintf(out, "- %s\n", formatTime(handoff.CreatedAt))
	if line := firstLine(handoff.Summary); line != "" {
		fmt.Fprintf(out, "  summary: %s\n", line)
	}
	if line := firstLine(handoff.NextAction); line != "" {
		fmt.Fprintf(out, "  next: %s\n", line)
	}
}

func writeContextRecords(out *strings.Builder, records []ContextRecord, empty string) {
	if len(records) == 0 {
		out.WriteString(empty)
		return
	}
	for _, record := range records {
		out.WriteString(contextRecordText(record))
	}
}

// contextRecordText is the exact Markdown of one record. The packer counts its
// tokens, so the budget and the printed answer cannot drift apart.
func contextRecordText(record ContextRecord) string {
	var out strings.Builder
	label := record.Kind
	if record.Status != "" {
		label += "/" + record.Status
	}
	if record.Quarantined {
		label += "/quarantined"
	}
	fmt.Fprintf(&out, "- [%s] %s (%s, %s)\n", label, SummarizeLine(record.Title, promptLineRunes), formatDate(record.CreatedAt), record.ID)
	if record.Detail != "" && record.Detail != SummarizeLine(record.Title, promptLineRunes) {
		fmt.Fprintf(&out, "  %s\n", record.Detail)
	}
	return out.String()
}
