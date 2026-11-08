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
	// contextMaxRecords bounds the relevant records section.
	contextMaxRecords = 8
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
	// Budget bounds Summary in runes; zero selects DefaultBootstrapBudget.
	Budget int
	// Kinds keeps only these record kinds (decision, attempt, note) in the
	// relevant records; empty keeps all three. Failed attempts matching the
	// task are listed regardless.
	Kinds []string
	// Since drops records and handoffs older than a window (30m, 24h, 7d, 2w)
	// or a date (2026-11-01, RFC 3339). Empty keeps everything.
	Since string
}

// ContextRecord is one record relevant to the task.
type ContextRecord struct {
	ID        string
	Kind      string
	Title     string
	Status    string // decisions only
	Detail    string // first line of the context, reason or body
	CreatedAt time.Time
	Score     float64
}

// ContextResult is the task context of a project. Summary is the Markdown
// rendering bounded by Budget; the other fields carry the same content.
type ContextResult struct {
	Project domain.Project
	Mode    string
	Query   string
	Since   time.Time // zero when not filtered
	Budget  int
	State   domain.State
	// Handoff is the newest handoff (within Since), nil when none.
	Handoff *domain.Handoff
	// Records are the top-ranked decisions, attempts and notes.
	Records []ContextRecord
	// FailedAttempts are further attempts matching the task, newest first.
	FailedAttempts []ContextRecord
	Findings       []Finding
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
	now := a.now()
	since, err := domain.ParseSince(opts.Since, now)
	if err != nil {
		return ContextResult{}, err
	}
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
	handoff, err := a.latestHandoff(ctx, project.ID, since)
	if err != nil {
		return ContextResult{}, err
	}

	result := ContextResult{
		Project: project, Mode: a.cfg.Mode, Query: query, Since: since,
		Budget: opts.Budget, State: state, Handoff: handoff,
	}
	if result.Budget <= 0 {
		result.Budget = DefaultBootstrapBudget
	}
	ranking := contextRanking{catalog: catalog, since: since, now: now}

	hits, err := a.contextSearch(ctx, query, project.ID, kinds, since)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ContextResult{}, ctxErr
		}
		result.Skipped = append(result.Skipped, "relevant records: "+err.Error())
	} else {
		result.Records = ranking.rank(hits, nil, contextMaxRecords)
	}
	// Failed attempts are listed whatever Kinds says: they warn against
	// repeating an approach that already failed.
	if err == nil {
		attemptHits, attemptErr := a.contextSearch(ctx, query, project.ID, []string{ContextKindAttempt}, since)
		if attemptErr != nil {
			result.Skipped = append(result.Skipped, "failed attempts: "+attemptErr.Error())
		} else {
			result.FailedAttempts = ranking.newestAttempts(attemptHits, recordIDs(result.Records), maxListedItems)
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

	result.Summary = truncateSummary(renderContext(result, a.revision(ctx), now), result.Budget)
	return result, nil
}

func (a *App) revision(ctx context.Context) string {
	revision, err := a.deps.Store.CurrentRevision(ctx)
	if err != nil {
		return ""
	}
	return revision.Value
}

func (a *App) contextSearch(ctx context.Context, query, projectID string, kinds []string, since time.Time) ([]ports.Hit, error) {
	return a.SearchWith(ctx, SearchOptions{Text: query, ProjectID: projectID, Limit: contextSearchLimit, Kinds: kinds, Since: since})
}

// latestHandoff returns the newest handoff not older than since.
func (a *App) latestHandoff(ctx context.Context, projectID string, since time.Time) (*domain.Handoff, error) {
	handoffs, err := a.listHandoffs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var latest *domain.Handoff
	for i := range handoffs {
		handoff := handoffs[i]
		if !since.IsZero() && handoff.CreatedAt.Before(since) {
			continue
		}
		if latest == nil || handoff.CreatedAt.After(latest.CreatedAt) {
			latest = &handoff
		}
	}
	return latest, nil
}

// contextCatalog maps record ids to their details. A kind whose store cannot
// list records is absent from listed, so hits of that kind are kept as the
// index returned them.
type contextCatalog struct {
	records map[string]ContextRecord
	listed  map[string]bool
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
	catalog contextCatalog
	since   time.Time
	now     time.Time
}

// candidates resolves hits to records in search order, dropping unknown
// records, records older than since, ids in exclude and duplicate ids.
func (r contextRanking) candidates(hits []ports.Hit, exclude map[string]bool) []ContextRecord {
	seen := map[string]bool{}
	out := make([]ContextRecord, 0, len(hits))
	for _, hit := range hits {
		if seen[hit.ID] || exclude[hit.ID] {
			continue
		}
		seen[hit.ID] = true
		record, ok := r.catalog.lookup(hit)
		if !ok {
			continue
		}
		if !r.since.IsZero() && !record.CreatedAt.IsZero() && record.CreatedAt.Before(r.since) {
			continue
		}
		out = append(out, record)
	}
	return out
}

// rank scores the hits by search rank, recency and kind and keeps the best
// limit records. Ties fall back to the id, so the order is deterministic.
func (r contextRanking) rank(hits []ports.Hit, exclude map[string]bool, limit int) []ContextRecord {
	records := r.candidates(hits, exclude)
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
func (r contextRanking) newestAttempts(hits []ports.Hit, exclude map[string]bool, limit int) []ContextRecord {
	records := r.candidates(hits, exclude)
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].CreatedAt.After(records[j].CreatedAt)
		}
		return records[i].ID < records[j].ID
	})
	return headRecords(records, limit)
}

func headRecords(records []ContextRecord, limit int) []ContextRecord {
	if len(records) > limit {
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

// renderContext writes the Markdown context. Sections are ordered by how much
// they matter, because the budget truncates from the end.
func renderContext(result ContextResult, revision string, now time.Time) string {
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
	for _, skipped := range result.Skipped {
		fmt.Fprintf(&out, "skipped: %s\n", SummarizeLine(skipped, promptLineRunes))
	}
	state := result.State
	fmt.Fprintf(&out, "state_updated: %s (age %s)\n", formatTime(state.UpdatedAt), stateAge(now, state))
	fmt.Fprintf(&out, "freshness: %s\n", freshness(now, state))
	fmt.Fprintf(&out, "goal: %s\n", valueOr(state.Goal, "not set"))
	fmt.Fprintf(&out, "next_action: %s\n", valueOr(state.NextAction, "not set"))
	writePromptList(&out, "active", state.Active)
	writePromptList(&out, "blocked", state.Blocked)
	writePromptList(&out, "open_questions", state.OpenQuestions)

	writeContextFindings(&out, result.Findings)
	writeContextHandoff(&out, result.Handoff)

	out.WriteString("\n## Relevant records\n")
	writeContextRecords(&out, result.Records, "- none matched the task\n")
	out.WriteString("\n## Failed attempts matching the task\n")
	writeContextRecords(&out, result.FailedAttempts, "- none beyond the relevant records\n")
	return out.String()
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
		fmt.Fprintf(out, "- [%s] %s\n", finding.Kind, SummarizeLine(finding.Message, promptLineRunes))
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
		label := record.Kind
		if record.Status != "" {
			label += "/" + record.Status
		}
		fmt.Fprintf(out, "- [%s] %s (%s, %s)\n", label, SummarizeLine(record.Title, promptLineRunes), formatDate(record.CreatedAt), record.ID)
		if record.Detail != "" && record.Detail != SummarizeLine(record.Title, promptLineRunes) {
			fmt.Fprintf(out, "  %s\n", record.Detail)
		}
	}
}
