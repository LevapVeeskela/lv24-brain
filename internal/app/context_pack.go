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
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
)

// Context layer names, in packing order. The order is the priority: an
// earlier layer is filled before a later one and the core is never dropped.
const (
	ContextLayerCore      = "core"
	ContextLayerDecisions = "decisions"
	ContextLayerAttempts  = "attempts"
	ContextLayerQuery     = "query"
	ContextLayerLinked    = "linked"
)

// Per-layer shares of the total token budget. The cap of a layer is
// proportional to the budget instead of a fixed number, so a 500-token answer
// and a 32000-token answer both spend their tokens where they matter. The
// shares sum to one; a layer may borrow the part of an earlier share that was
// left unused, which is what makes the caps fungible.
//
// Packing order is the priority: the query layer, the records that match the
// task, comes first after the core; the curated background and the 1-hop
// neighbours follow.
var contextLayerOrder = []struct {
	name  string
	share float64
}{
	{ContextLayerCore, 0.25},
	{ContextLayerQuery, 0.35},
	{ContextLayerDecisions, 0.20},
	{ContextLayerAttempts, 0.12},
	{ContextLayerLinked, 0.08},
}

// ContextSectionRecord is one record inside a packed layer. The fields are the
// metadata an agent needs to decide whether to expand the record; Detail is
// the one-line preview the Markdown summary renders and is not repeated in the
// MCP section metadata.
type ContextSectionRecord struct {
	ID          string
	Kind        string
	Title       string
	Status      string
	Path        string
	Detail      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Quarantined bool
	Tokens      int
	Score       float64
}

// ContextSection is one packed layer: its configured cap, the tokens it uses
// and the records that fit.
type ContextSection struct {
	Name    string
	Cap     int
	Tokens  int
	Records []ContextSectionRecord
	// Text is the rendered core block. Only the core section sets it: the
	// other layers are rendered from Records by section name.
	Text string
}

// ContextTruncation explains one record or block that did not fit.
type ContextTruncation struct {
	ID      string
	Section string
	Reason  string
}

// ContextPack is the budget accounting of one context answer.
type ContextPack struct {
	// Budget is the total token budget that was applied.
	Budget int
	// TotalTokens is the sum of every section's tokens.
	TotalTokens int
	Sections    []ContextSection
	Truncated   []ContextTruncation
	// Quarantined is the number of ids excluded from trusted context.
	Quarantined int
}

// contextPackInput is what the packer needs to fill the layers.
type contextPackInput struct {
	Budget      int
	Header      string
	CoreText    string
	CoreRecords []ContextSectionRecord
	Decisions   []ContextRecord
	Attempts    []ContextRecord
	Query       []ContextRecord
	Quarantined int
	// QueryAlways renders the query layer even when it packs nothing: `brain
	// context` prints its headings and "none" lines in every answer, so their
	// cost must be part of the budget. Bootstrap has no query layer.
	QueryAlways bool
	// Linked is called only when the earlier layers left room. It returns the
	// 1-hop neighbours of the selected records.
	Linked func(selected []string) ([]ContextRecord, error)
}

// buildContextPack fills the layers in order under one total budget. A record
// is never split: a record that does not fit is skipped and listed in
// Truncated. The core layer is always present; only its prose is truncated,
// with a marker, when it alone exceeds its cap. Every other layer may borrow
// the share an earlier layer left unused, so a thin core does not waste the
// tokens it did not spend.
func buildContextPack(in contextPackInput) (ContextPack, error) {
	pack := ContextPack{Budget: in.Budget, Quarantined: in.Quarantined, Sections: []ContextSection{}}
	remaining := in.Budget
	borrowed := 0

	// Core: state, latest handoff and findings. The header (project identity
	// and mode) counts against the core share too, so the rendered answer stays
	// inside the total budget.
	coreAllowance := minInt(contextLayerCap(ContextLayerCore, in.Budget), in.Budget)
	core := ContextSection{Name: ContextLayerCore, Cap: coreAllowance}
	headerTokens := textbudget.Tokens(in.Header)
	bodyBudget := coreAllowance - headerTokens
	coreText := ""
	truncated := in.CoreText != ""
	if bodyBudget > 0 {
		coreText, truncated = textbudget.Truncate(in.CoreText, bodyBudget)
	}
	if truncated {
		pack.Truncated = append(pack.Truncated, ContextTruncation{
			ID: "core", Section: ContextLayerCore,
			Reason: fmt.Sprintf("core needs more than %d tokens; prose truncated with a marker", coreAllowance),
		})
	}
	core.Text = coreText
	core.Records = in.CoreRecords
	core.Tokens = headerTokens + textbudget.Tokens(coreText)
	pack.Sections = append(pack.Sections, core)
	remaining -= core.Tokens
	borrowed = contextBorrowLeft(borrowed, coreAllowance, core.Tokens)
	if remaining < 0 {
		remaining = 0
	}

	selected := make(map[string]bool, 32)
	selectedOrder := make([]string, 0, 32)
	addLayer := func(name string, candidates []ContextRecord, always bool) {
		share := contextLayerCap(name, in.Budget)
		allowance := minInt(share+borrowed, remaining)
		if allowance < 0 {
			allowance = 0
		}
		sortContextCandidates(candidates)
		// The "… N more dropped" line is part of the layer, so its room is
		// reserved before the first record is packed.
		marker := contextDroppedMarkerTokens(len(candidates))
		section := ContextSection{
			Name: name, Cap: allowance, Records: []ContextSectionRecord{},
			Tokens: contextSectionHeaderTokenCost(name),
		}
		for _, record := range candidates {
			if record.ID == "" || selected[record.ID] {
				continue
			}
			tokens := contextRecordTokens(record)
			left := allowance - section.Tokens - marker
			if tokens > left {
				reason := fmt.Sprintf("does not fit the %s layer: needs %d tokens, %d left", name, tokens, left)
				if left <= 0 {
					reason = fmt.Sprintf("budget exhausted before the %s layer; needs %d tokens", name, tokens)
				}
				pack.Truncated = append(pack.Truncated, ContextTruncation{ID: record.ID, Section: name, Reason: reason})
				continue
			}
			section.Records = append(section.Records, contextSectionRecord(record))
			section.Tokens += tokens
			selected[record.ID] = true
			selectedOrder = append(selectedOrder, record.ID)
		}
		if len(section.Records) == 0 {
			dropped := contextDroppedCount(pack.Truncated, name)
			switch {
			case always:
				// The query headings and their "none" lines are printed by
				// every answer, even when nothing matched, so they cost budget.
				section.Tokens = contextQueryEmptyTokens()
				if dropped > 0 {
					section.Tokens += contextDroppedMarkerTokens(dropped)
				}
			case dropped == 0:
				return
			default:
				// Nothing fit, but the layer must still report how many
				// records the budget left out.
				section.Tokens = contextSectionHeaderTokenCost(name) + contextDroppedMarkerTokens(dropped)
			}
		} else if dropped := contextDroppedCount(pack.Truncated, name); dropped > 0 {
			section.Tokens += contextDroppedMarkerTokens(dropped)
		}
		pack.Sections = append(pack.Sections, section)
		remaining -= section.Tokens
		if remaining < 0 {
			remaining = 0
		}
		borrowed = contextBorrowLeft(borrowed, share, section.Tokens)
	}

	addLayer(ContextLayerQuery, in.Query, in.QueryAlways)
	addLayer(ContextLayerDecisions, in.Decisions, false)
	addLayer(ContextLayerAttempts, in.Attempts, false)

	// The linked layer exists only when the earlier layers left room. Its
	// lookup is skipped entirely when there is none, so a tight budget costs
	// no extra store reads.
	if remaining > 0 && in.Linked != nil {
		linked, err := in.Linked(selectedOrder)
		if err != nil {
			return ContextPack{}, err
		}
		addLayer(ContextLayerLinked, linked, false)
	}

	for _, section := range pack.Sections {
		pack.TotalTokens += section.Tokens
	}
	return pack, nil
}

// contextLayerCap is the proportional cap of one layer.
func contextLayerCap(name string, budget int) int {
	for _, layer := range contextLayerOrder {
		if layer.name != name {
			continue
		}
		capTokens := int(math.Round(layer.share * float64(budget)))
		if capTokens < 1 {
			capTokens = 1
		}
		return capTokens
	}
	return budget
}

// contextBorrowLeft tracks the budget earlier layers left unused. A layer that
// spends less than its share adds to the pool the later layers may borrow; a
// layer that spends more pays it back, never below zero.
func contextBorrowLeft(borrowed, share, spent int) int {
	if spent < share {
		return borrowed + (share - spent)
	}
	borrowed -= spent - share
	if borrowed < 0 {
		return 0
	}
	return borrowed
}

// contextDroppedMarkerTokens counts the per-layer line that reports the records
// the budget left out.
func contextDroppedMarkerTokens(dropped int) int {
	return textbudget.Tokens(fmt.Sprintf("- … %d more dropped\n", dropped))
}

// contextDroppedCount is how many records of one layer the packer left out.
func contextDroppedCount(truncations []ContextTruncation, layer string) int {
	count := 0
	for _, truncation := range truncations {
		if truncation.Section == layer && truncation.ID != "core" && truncation.ID != "summary" {
			count++
		}
	}
	return count
}

// contextQueryEmptyTokens counts the headings and the two "none" lines the
// query layer prints when nothing matched.
func contextQueryEmptyTokens() int {
	return textbudget.Tokens("\n## Relevant records\n- none matched the task\n" +
		"\n## Failed attempts matching the task\n- none beyond the relevant records\n")
}

// contextSectionHeaderTokenCost counts the Markdown heading(s) a layer adds.
func contextSectionHeaderTokenCost(name string) int {
	switch name {
	case ContextLayerDecisions:
		return textbudget.Tokens("\n## Current decisions\n")
	case ContextLayerAttempts:
		return textbudget.Tokens("\n## Recent attempts\n")
	case ContextLayerQuery:
		return textbudget.Tokens("\n## Relevant records\n\n## Failed attempts matching the task\n")
	case ContextLayerLinked:
		return textbudget.Tokens("\n## Linked records\n")
	}
	return 0
}

// sortContextCandidates orders a layer by score, then update time, then id.
func sortContextCandidates(records []ContextRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Score != records[j].Score {
			return records[i].Score > records[j].Score
		}
		if !records[i].UpdatedAt.Equal(records[j].UpdatedAt) {
			return records[i].UpdatedAt.After(records[j].UpdatedAt)
		}
		return records[i].ID < records[j].ID
	})
}

func contextSectionRecord(record ContextRecord) ContextSectionRecord {
	return ContextSectionRecord{
		ID: record.ID, Kind: record.Kind, Title: record.Title, Status: record.Status,
		Path: record.Path, Detail: record.Detail, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		Quarantined: record.Quarantined, Tokens: contextRecordTokens(record), Score: record.Score,
	}
}

// contextRecordPath is the logical data-directory path of a record. It uses the
// record id as the file stem because the physical name also carries a slug;
// the id stays the stable, human-readable address.
func contextRecordPath(projectID, kind, id string) string {
	return recordDataPath(projectID, kind, id+".md")
}

// contextRecordTokens counts the Markdown line(s) one record adds. It is the
// same text writeContextRecords emits, so the budget and the answer agree.
func contextRecordTokens(record ContextRecord) int {
	return textbudget.Tokens(contextRecordText(record))
}

// sectionRecords converts a packed layer back to the record shape the renderer
// and the compatibility fields use.
func sectionRecords(pack ContextPack, name string) []ContextRecord {
	section, ok := contextSection(pack, name)
	if !ok {
		return nil
	}
	out := make([]ContextRecord, 0, len(section.Records))
	for _, record := range section.Records {
		out = append(out, ContextRecord{
			ID: record.ID, Kind: record.Kind, Title: record.Title, Status: record.Status,
			Detail: record.Detail, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
			Score: record.Score, Quarantined: record.Quarantined, Path: record.Path,
		})
	}
	return out
}

func contextSection(pack ContextPack, name string) (ContextSection, bool) {
	for _, section := range pack.Sections {
		if section.Name == name {
			return section, true
		}
	}
	return ContextSection{}, false
}

// ExplainLines is the human-readable budget table printed by
// `brain context --explain` and returned by brain_context with explain=true.
func (p ContextPack) ExplainLines() []string {
	lines := make([]string, 0, len(contextLayerOrder)+2)
	for _, layer := range contextLayerOrder {
		section, ok := contextSection(p, layer.name)
		tokens, records := 0, 0
		capTokens := contextLayerCap(layer.name, p.Budget)
		if ok {
			tokens, records = section.Tokens, len(section.Records)
			capTokens = section.Cap
		}
		lines = append(lines, fmt.Sprintf("%s: %d/%d tokens, %d records", layer.name, tokens, capTokens, records))
	}
	lines = append(lines, fmt.Sprintf("total: %d/%d tokens, %d dropped", p.TotalTokens, p.Budget, len(p.Truncated)))
	return lines
}

// ExplainText renders ExplainLines as one block.
func (p ContextPack) ExplainText() string {
	return strings.Join(p.ExplainLines(), "\n")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// contextTokenBudget resolves the token budget of one context call: an
// explicit value, then the configuration, then the documented default.
func (a *App) contextTokenBudget(requested int) (int, error) {
	if requested < 0 {
		return 0, domain.NewValidationError("context.budget", "must not be negative")
	}
	budget := requested
	if budget == 0 {
		budget = a.cfg.EffectiveContextBudget()
	}
	if budget <= 0 {
		budget = domain.DefaultContextBudgetTokens
	}
	if budget > domain.MaxContextBudgetTokens {
		return 0, domain.NewValidationError("context.budget",
			fmt.Sprintf("must be at most %d tokens", domain.MaxContextBudgetTokens))
	}
	return budget, nil
}

// packContext fills the layers for one task context. A failure in the optional
// linked lookup retries without that layer and returns the reason, so a link
// read problem never costs the curated answer.
func (a *App) packContext(
	ctx context.Context, result ContextResult, ranking contextRanking,
	kinds []string, tokenBudget int, now time.Time,
) (ContextPack, error) {
	header := renderContextHeader(result, a.revision(ctx))
	coreText, coreRecords := renderContextCore(result, now)
	query := append(append([]ContextRecord{}, result.Records...), result.FailedAttempts...)
	exclude := recordIDs(query)

	var decisions, attempts []ContextRecord
	if slices.Contains(kinds, ContextKindDecision) {
		decisions = ranking.background(ContextKindDecision, exclude)
	}
	if slices.Contains(kinds, ContextKindAttempt) {
		attempts = ranking.background(ContextKindAttempt, exclude)
	}
	input := contextPackInput{
		Budget: tokenBudget, Header: header, CoreText: coreText, CoreRecords: coreRecords,
		Decisions: decisions, Attempts: attempts, Query: query, QueryAlways: true,
		Quarantined: len(result.Quarantined),
		Linked: func(selected []string) ([]ContextRecord, error) {
			return a.contextLinkedCandidates(ctx, ranking, selected, kinds)
		},
	}
	pack, err := buildContextPack(input)
	if err == nil {
		return pack, nil
	}
	input.Linked = nil
	fallback, fallbackErr := buildContextPack(input)
	if fallbackErr != nil {
		return ContextPack{}, fallbackErr
	}
	return fallback, err
}

// background lists the current records of one kind for its curated layer.
// Decisions keep accepted ones first; attempts are ordered by update time by
// the packer.
func (r contextRanking) background(kind string, exclude map[string]bool) []ContextRecord {
	out := make([]ContextRecord, 0, len(r.catalog.records))
	for _, record := range r.catalog.records {
		if record.Kind != kind || exclude[record.ID] {
			continue
		}
		if record.Quarantined && !r.includeUnreviewed {
			continue
		}
		if !r.filter.Allows(record.Validity, record.CreatedAt, r.now) {
			continue
		}
		if !r.since.IsZero() && !withinSince(record.ContentUpdatedAt, record.UpdatedAt, record.CreatedAt, r.since) {
			continue
		}
		record.Path = contextRecordPath(r.projectID, record.Kind, record.ID)
		if kind == ContextKindDecision {
			record.Score = decisionContextScore(record.Status, record.Pinned)
		}
		out = append(out, record)
	}
	return out
}

// contextLinkedCandidates returns the 1-hop neighbours of the selected records.
// Only references inside the same project resolve: a cross-project link would
// widen the answer beyond the curated project. The target metadata comes from
// the catalog already loaded for this call, and the selected records themselves
// are read in one batch listing: one listing per selected record would make the
// layer cost N full parses of the project.
func (a *App) contextLinkedCandidates(
	ctx context.Context, ranking contextRanking, selected []string, kinds []string,
) ([]ContextRecord, error) {
	if len(selected) == 0 || len(ranking.catalog.records) == 0 {
		return nil, nil
	}
	allowed := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = true
	}
	chosen := make(map[string]bool, len(selected))
	for _, id := range selected {
		chosen[id] = true
	}
	sources, err := a.RecordsByIDs(ctx, ranking.projectID, selected)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	references := map[string]int{}
	for _, id := range selected {
		record, ok := sources[id]
		if !ok {
			// A record can be removed between the listing and the read; a
			// missing neighbour is not a reason to fail the context.
			continue
		}
		for _, raw := range recordRawLinks(record) {
			if raw.projectID != ranking.projectID || chosen[raw.link.ID] {
				continue
			}
			references[raw.link.ID]++
		}
	}
	out := make([]ContextRecord, 0, len(references))
	for id, count := range references {
		record, ok := ranking.catalog.records[id]
		if !ok || !allowed[record.Kind] {
			continue
		}
		if record.Quarantined && !ranking.includeUnreviewed {
			continue
		}
		if !ranking.filter.Allows(record.Validity, record.CreatedAt, ranking.now) {
			continue
		}
		if !ranking.since.IsZero() && !withinSince(record.ContentUpdatedAt, record.UpdatedAt, record.CreatedAt, ranking.since) {
			continue
		}
		record.Score = float64(count)
		out = append(out, record)
	}
	return out, nil
}

// contextRecordFromRecord converts a loaded record to the context shape.
func contextRecordFromRecord(record Record) ContextRecord {
	summary := record.Summary()
	out := ContextRecord{
		ID: summary.ID, Kind: summary.Kind, Title: summary.Title, Status: summary.Status,
		CreatedAt: summary.CreatedAt, UpdatedAt: summary.UpdatedAt, ContentUpdatedAt: summary.ContentUpdatedAt,
		Validity: record.Validity(), Quarantined: recordQuarantined(record),
		Pinned: record.Pinned(),
	}
	out.Path = contextRecordPath(summary.ProjectID, summary.Kind, summary.ID)
	switch {
	case record.Note != nil:
		out.Detail = SummarizeLine(record.Note.Body, promptLineRunes)
	case record.Decision != nil:
		out.Detail = SummarizeLine(record.Decision.Context, promptLineRunes)
	case record.Attempt != nil:
		out.Detail = attemptDetail(*record.Attempt)
	case record.Handoff != nil:
		out.Detail = SummarizeLine(record.Handoff.Summary, promptLineRunes)
	}
	return out
}

func recordQuarantined(record Record) bool {
	switch {
	case record.Note != nil:
		return record.Note.Quarantined()
	case record.Decision != nil:
		return record.Decision.Quarantined()
	case record.Attempt != nil:
		return record.Attempt.Quarantined()
	case record.Handoff != nil:
		return record.Handoff.Quarantined()
	}
	return false
}

// decisionContextScore ranks current decisions: an accepted decision is
// pinned, a proposal is current but weaker, and the rest sit behind them.
// An explicitly pinned record ranks above its unpinned peers.
func decisionContextScore(status string, pinned bool) float64 {
	base := 0.25
	switch status {
	case string(domain.DecisionAccepted):
		base = 1
	case string(domain.DecisionProposed):
		base = 0.5
	case string(domain.DecisionSuperseded):
		base = 0.1
	}
	if pinned {
		base += 2
	}
	return base
}

// splitQuerySection splits the packed query layer back into ranked records and
// failed attempts, so the compatibility fields keep their meaning.
func splitQuerySection(pack ContextPack, failed map[string]bool) ([]ContextRecord, []ContextRecord) {
	packed := sectionRecords(pack, ContextLayerQuery)
	records := make([]ContextRecord, 0, len(packed))
	attempts := make([]ContextRecord, 0, len(packed))
	for _, record := range packed {
		if failed[record.ID] {
			attempts = append(attempts, record)
			continue
		}
		records = append(records, record)
	}
	return records, attempts
}

// fitContextPack renders the packed answer and drops records until the rendered
// text fits the token budget. The packer is conservative, but only the rendered
// text is authoritative: a dropped-record line, an empty-section placeholder or
// a rounding step can still push the estimate over. Dropping the lowest
// priority packed record keeps "never exceed the budget" true without lying
// about the sections.
func fitContextPack(result *ContextResult, revision string, now time.Time, failed map[string]bool) string {
	budget := result.Pack.Budget
	limit := fitAttempts(result.Pack) + 1
	for attempt := 0; attempt < limit; attempt++ {
		summary := renderContext(*result, revision, now)
		if budget <= 0 || textbudget.Tokens(summary) <= budget {
			return summary
		}
		if !dropLowestPriorityRecord(&result.Pack) {
			// Even the fixed header does not fit; shorten the text itself so a
			// tiny budget is still honoured.
			cut, truncated := textbudget.Truncate(summary, budget)
			if truncated {
				result.Pack.Truncated = append(result.Pack.Truncated, ContextTruncation{
					ID: "summary", Section: ContextLayerCore,
					Reason: "the fixed header alone exceeds the budget; the answer was cut",
				})
				result.Pack.TotalTokens = textbudget.Tokens(cut)
			}
			return cut
		}
		result.Records, result.FailedAttempts = splitQuerySection(result.Pack, failed)
	}
	return renderContext(*result, revision, now)
}

// fitAttempts bounds the fitting loop by the number of packed records: each
// step drops exactly one.
func fitAttempts(pack ContextPack) int {
	total := 0
	for _, section := range pack.Sections {
		total += len(section.Records)
	}
	return total
}

// dropLowestPriorityRecord removes the last record of the least important
// packed layer and records why. It reports false when nothing is left to drop.
func dropLowestPriorityRecord(pack *ContextPack) bool {
	for _, name := range []string{ContextLayerLinked, ContextLayerAttempts, ContextLayerDecisions, ContextLayerQuery} {
		for index := range pack.Sections {
			section := &pack.Sections[index]
			if section.Name != name || len(section.Records) == 0 {
				continue
			}
			last := section.Records[len(section.Records)-1]
			section.Records = section.Records[:len(section.Records)-1]
			section.Tokens -= last.Tokens
			pack.TotalTokens -= last.Tokens
			if pack.TotalTokens < 0 {
				pack.TotalTokens = 0
			}
			pack.Truncated = append(pack.Truncated, ContextTruncation{
				ID: last.ID, Section: name,
				Reason: "dropped after rendering: the answer would have exceeded the token budget",
			})
			return true
		}
	}
	return false
}
