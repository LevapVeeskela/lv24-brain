package app

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/searchnorm"
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
)

// DefaultSearchLimit is used when a search does not set a limit.
const DefaultSearchLimit = 20

// SearchFallbackThreshold is the number of strict hits below which the terms
// are retried joined with OR. A strict AND pass that finds a handful of records
// usually means the query was too specific for the corpus, not that the corpus
// has nothing nearby; the relaxed page is merged with the strict one rather
// than replacing it.
const SearchFallbackThreshold = 5

// PreviewRunes is the longest preview a search returns per hit. A model reads
// the preview first and asks for the body only when it needs one, which is what
// keeps a search answer cheap.
const PreviewRunes = textbudget.PreviewRunes

// withinSince reports whether a record belongs to a --since window. Every
// surface (search, context, list) compares the same instant: the record's
// content change time. Review, snooze, redaction and merge bookkeeping rewrite
// the frontmatter and move the revision, but not the text, so a window must not
// re-report the corpus after them. A record written before content_updated_at
// existed falls back to its revision and creation time, the conservative
// answer: reporting a record once too often is cheaper than hiding a change.
func withinSince(contentUpdatedAt, updatedAt, createdAt, since time.Time) bool {
	if since.IsZero() {
		return true
	}
	for _, at := range []time.Time{contentUpdatedAt, updatedAt, createdAt} {
		if !at.IsZero() {
			return !at.Before(since)
		}
	}
	return true
}

// Searcher runs a full-text search against a remote server. Server mode uses
// it for brain search through GET /v1/search.
type Searcher interface {
	Search(ctx context.Context, query ports.Query) ([]ports.Hit, error)
}

// termFrequencyIndex is implemented by a local index that can report how many
// documents of a project contain a query term. The support gate uses it to
// separate the rare, discriminative terms a supported hit must contain from the
// common ones it only has to partly contain. An index without it leaves the
// page untouched: support is never guessed.
type termFrequencyIndex interface {
	TermFrequencies(ctx context.Context, projectID string, terms []string) (map[string]int, int, error)
}

// KindDump is the index kind of connector dumps (mail, GitHub, GitLab).
const KindDump = "dump"

// SearchOptions selects what a search returns. By default connector dumps
// are left out so curated records are not drowned by mail and issue lists.
type SearchOptions struct {
	Text      string
	ProjectID string
	Limit     int
	// Kinds keeps only these kinds (note, decision, attempt, handoff, state,
	// dump); naming "dump" includes dumps.
	Kinds []string
	// IncludeDumps adds connector dumps and raw-zone items (kind "item") to
	// an unfiltered search.
	IncludeDumps bool
	// IncludeStubs keeps low-signal stub records (a title that is only the
	// record header, or a body that says nothing changed) that search hides
	// by default. Stubs stay in listings and counts; this flag only widens
	// retrieval.
	IncludeStubs bool
	// Since keeps only documents updated at or after this time; zero keeps
	// everything.
	Since time.Time
	// AsOf keeps documents valid at that instant: default is current records
	// only, an as-of instant selects the records that were current then.
	AsOf time.Time
	// IncludeSuperseded keeps records whose validity ended. An AsOf instant
	// selects the point in time and takes precedence.
	IncludeSuperseded bool
	// MatchMode combines the query terms: MatchModeAll requires every term,
	// MatchModeAny accepts any term, empty keeps the historical behaviour of
	// a strict pass followed by a relaxed one. Local and git mode only.
	MatchMode string
	// Relaxed allows the strict pass to fall back to the same terms joined
	// with OR when it finds nothing. It is on by default to keep the previous
	// behaviour; an explicit MatchModeAll turns it off unless Relaxed is set.
	Relaxed bool
	// MinScore drops hits below the composite relevance, in [0, 1]. Local and
	// git mode only.
	MinScore float64
	// Explain also returns the weighted contribution of every ranking signal.
	Explain bool
	// Full returns the whole body of each hit in addition to its preview.
	// Local and git mode only.
	Full bool
	// Subtype, Scope and TopicKey keep only records with that classification.
	// Empty keeps every record, so default search behaviour is unchanged.
	// Local and git mode only.
	Subtype  string
	Scope    string
	TopicKey string
}

// SearchReport is one search response: the ranked page and what the query had
// to do to produce it.
type SearchReport struct {
	Hits []ports.Hit
	// Relaxed reports that the strict pass found fewer than
	// SearchFallbackThreshold hits and the page was widened with an OR pass.
	Relaxed bool
	// Notice explains a withheld raw-zone result, empty when nothing was
	// withheld.
	Notice string
	// Support says whether the page answers the query or is only lexically
	// nearby. Tier is one of the Support* constants.
	Support Support
	// Excluded lists the records the response withheld and why. A reason is
	// one of the Excluded* constants.
	Excluded []ExcludedHit
}

// Support tiers of a search response.
const (
	// SupportSupported means the page answers the query: every hit (or the
	// first of them) contains the rare query terms and enough of the rest.
	SupportSupported = "supported"
	// SupportRelated means the hits only matched the relaxed pass; they are
	// nearby, not an answer.
	SupportRelated = "related"
	// SupportNone means nothing answered the query. The hit list is empty and
	// Reason says why; the relaxed matches, if any, are in Excluded with the
	// lower-rank reason.
	SupportNone = "none"
)

// Reasons a record is not in Hits. Every withheld record carries one.
const (
	// ExcludedDuplicate is a record whose content hash already appeared in
	// the same project: the same record written on another machine.
	ExcludedDuplicate = "duplicate"
	// ExcludedLowerRank is a relaxed match withheld because the response had
	// no supported hit; it is nearby, not an answer.
	ExcludedLowerRank = "lower-rank"
)

const (
	// supportRareShare is the document-frequency share below which a query
	// term counts as rare: a record that answers the query contains it.
	supportRareShare = 0.20
	// supportSubjectShare is the share of the common subject terms a
	// supported hit must contain.
	supportSubjectShare = 0.50
	// supportRelatedMinTerms is the number of distinct query terms a page must
	// overlap to stay visible as related when no hit is supported. One shared
	// word is a coincidence; two subject terms plus one rare term is topical
	// overlap.
	supportRelatedMinTerms = 2
	// supportPrefixRunes is the shortest term matched as a prefix when a hit
	// is checked for a term, mirroring the index's prefix rule.
	supportPrefixRunes = 4
)

// Support is why a search response is, or is not, an answer. Best is the
// largest share of the query terms any matched record contained, Threshold is
// the subject-term share a supported hit needs, and Reason is set only when the
// page could not answer the query.
type Support struct {
	Tier      string
	Best      float64
	Threshold float64
	Reason    string
}

// ExcludedHit is one record a search withheld, with the reason.
type ExcludedHit struct {
	ID        string
	ProjectID string
	Kind      string
	Title     string
	Score     float64
	Reason    string
}

// Search queries curated records (no dumps); see SearchWith.
func (a *App) Search(ctx context.Context, text, projectID string, limit int) ([]ports.Hit, error) {
	return a.SearchWith(ctx, SearchOptions{Text: text, ProjectID: projectID, Limit: limit})
}

// SearchWith queries the local FTS5 index in local and git mode, and the
// server search endpoint in server mode. Raw-zone hits are returned only when
// a context-allow.json grant authorizes them.
func (a *App) SearchWith(ctx context.Context, opts SearchOptions) ([]ports.Hit, error) {
	report, err := a.SearchWithReport(ctx, opts)
	return report.Hits, err
}

// SearchWithNotice is SearchWith plus a notice explaining a withheld raw
// result (empty when nothing was withheld). Curated records are never affected
// by the raw policy.
func (a *App) SearchWithNotice(ctx context.Context, opts SearchOptions) ([]ports.Hit, string, error) {
	report, err := a.SearchWithReport(ctx, opts)
	return report.Hits, report.Notice, err
}

// SearchWithReport runs a search and reports the page together with whether
// the query had to relax its terms.
func (a *App) SearchWithReport(ctx context.Context, opts SearchOptions) (SearchReport, error) {
	mode, err := normalizeMatchMode(opts.MatchMode)
	if err != nil {
		return SearchReport{}, err
	}
	opts.MatchMode = mode
	if subtype, scope, topic, err := validateClassificationFilters(opts.Subtype, opts.Scope, opts.TopicKey); err != nil {
		return SearchReport{}, err
	} else {
		opts.Subtype, opts.Scope, opts.TopicKey = subtype, scope, topic
	}
	if err := validateSearchScore(opts); err != nil {
		return SearchReport{}, err
	}
	if a.cfg.Mode == config.ModeServer {
		if a.deps.Searcher == nil {
			return SearchReport{}, fmt.Errorf("%w: server search client is not available", domain.ErrUnsupported)
		}
		if !opts.AsOf.IsZero() || opts.IncludeSuperseded {
			// The server API does not expose validity yet; silently ignoring
			// the flag would make a past-instant query look answered.
			return SearchReport{}, fmt.Errorf("%w: as-of and superseded search are local/git only", domain.ErrUnsupported)
		}
		if unsupported := localOnlySearchOption(opts); unsupported != "" {
			// The server keeps its own Postgres ranking; pretending to apply
			// a local ranking control would misreport the result.
			return SearchReport{}, fmt.Errorf("%w: %s is local/git only", domain.ErrUnsupported, unsupported)
		}
		// Servers before item support reject exclude_kind=item; a server
		// treats an excluded "dump" as excluding its items too.
		return a.search(ctx, opts, []string{KindDump}, a.deps.Searcher.Search, false)
	}
	if a.deps.Index == nil {
		return SearchReport{}, fmt.Errorf("%w: local search index is not available", domain.ErrUnsupported)
	}
	// A stale projection is still searchable; a failed repair keeps the
	// marker and is retried by the next search.
	_ = a.repairIndexIfDirty(ctx)
	return a.search(ctx, opts, []string{KindDump, KindItem}, a.deps.Index.Search, true)
}

// search runs the query; rawKinds are excluded from an unfiltered search
// unless opts.IncludeDumps is set. Raw hits are filtered by the grant policy
// after the query, so a store that ignores the kind filter cannot leak them.
//
// local selects the local/git behaviour: the ranking instant is the app clock,
// the match mode is explicit and the relaxed fallback is decided here. A server
// query leaves MatchMode and Now zero and sends exactly one request, because
// the server owns its ranking.
func (a *App) search(ctx context.Context, opts SearchOptions, rawKinds []string, run func(context.Context, ports.Query) ([]ports.Hit, error), local bool) (SearchReport, error) {
	text := strings.TrimSpace(opts.Text)
	if text == "" {
		return SearchReport{}, domain.NewValidationError("search.query", "is required")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if ctx == nil {
		ctx = context.Background()
	}
	query := ports.Query{
		Text: text, ProjectID: strings.TrimSpace(opts.ProjectID), Limit: limit, Since: opts.Since,
		AsOf: opts.AsOf, IncludeSuperseded: opts.IncludeSuperseded,
		MinScore: opts.MinScore, Explain: opts.Explain, IncludeBody: opts.Full,
		Subtype: strings.TrimSpace(opts.Subtype), Scope: strings.TrimSpace(opts.Scope),
		TopicKey: strings.TrimSpace(opts.TopicKey), IncludeStubs: opts.IncludeStubs,
	}
	if local {
		// The support gate reads the whole text to tell which query terms a
		// hit really contains; a snippet is a window, not the record. The
		// body is dropped before the response leaves the use case unless the
		// caller asked for it.
		query.IncludeBody = true
	}
	for _, kind := range opts.Kinds {
		if kind = strings.TrimSpace(kind); kind != "" {
			query.Kinds = append(query.Kinds, kind)
		}
	}
	if len(query.Kinds) == 0 && !opts.IncludeDumps {
		query.ExcludeKinds = rawKinds
	}
	raw := wantsRawText(query.Kinds, opts.IncludeDumps)

	// The strict pass goes first; the relaxed pass repeats the same terms
	// joined with OR. A query without an explicit mode keeps both passes, and
	// reports the fallback, so no caller loses results it used to get. The
	// strict pass is the precision pass, so it is always tried first even for a
	// broad query: strict hits outrank the records only the relaxed pass found.
	relaxed := false
	if local {
		query.Now = a.now()
		mode := opts.MatchMode
		if mode == "" {
			mode = ports.MatchModeAll
		}
		query.MatchMode = mode
		relaxed = opts.Relaxed || opts.MatchMode == ""
		if mode == ports.MatchModeAny {
			relaxed = false
		}
	}
	hits, err := run(ctx, query)
	if err != nil {
		return SearchReport{}, err
	}
	report := SearchReport{}
	strict := hits
	if relaxed && searchTermCount(text) > 1 && len(hits) < SearchFallbackThreshold {
		relaxedQuery := query
		relaxedQuery.MatchMode = ports.MatchModeAny
		relaxedHits, runErr := run(ctx, relaxedQuery)
		if runErr != nil {
			return SearchReport{}, runErr
		}
		if merged, used := mergeRelaxedHits(hits, relaxedHits, limit); used {
			hits = merged
			report.Relaxed = true
		}
	}
	// The same record written on two machines is one answer, not two: the
	// first occurrence by creation time keeps the slot and the copies are
	// reported once, with the duplicate reason.
	hits, duplicates := collapseDuplicateHits(hits)
	report.Excluded = append(report.Excluded, duplicates...)
	if local {
		var withheld []ExcludedHit
		// Only an AND pass is a strict pass. Under MatchModeAny the first
		// page is the OR page, so nothing may be promoted to supported just
		// because it matched.
		var strictSet map[string]bool
		if query.MatchMode == ports.MatchModeAll {
			strictSet = hitKeys(strict)
		}
		report.Support, hits, withheld = a.supportGate(ctx, opts, hits, strictSet)
		report.Excluded = append(report.Excluded, withheld...)
	}
	// The preview stays a preview: --full adds the body next to it instead of
	// turning the snippet into one.
	for index := range hits {
		hits[index].Snippet = textbudget.Preview(hits[index].Snippet, PreviewRunes)
		if !opts.Full {
			hits[index].Body = ""
		}
	}
	if !raw {
		// A curated search must never return raw text even if the store
		// ignored the exclude filter.
		report.Hits = dropRawKinds(hits)
		return report, nil
	}
	access, err := a.loadRawAccess(ctx)
	if err != nil {
		return SearchReport{}, err
	}
	kept, dropped := a.filterRawHits(ctx, access, hits)
	if dropped > 0 || (!access.available && len(hits) > 0) {
		report.Notice = access.hint()
	}
	report.Hits = kept
	return report, nil
}

// normalizeMatchMode validates the match mode and lowercases it.
func normalizeMatchMode(mode string) (string, error) {
	switch value := strings.ToLower(strings.TrimSpace(mode)); value {
	case "", ports.MatchModeAll, ports.MatchModeAny:
		return value, nil
	default:
		return "", domain.NewValidationError("search.matchMode", fmt.Sprintf("must be %s or %s, got %q", ports.MatchModeAll, ports.MatchModeAny, mode))
	}
}

// validateSearchScore rejects a score threshold outside the composite range.
func validateSearchScore(opts SearchOptions) error {
	if opts.MinScore < 0 || opts.MinScore > 1 {
		return domain.NewValidationError("search.minScore", fmt.Sprintf("must be between 0 and 1, got %v", opts.MinScore))
	}
	return nil
}

// localOnlySearchOption names the first ranking control the server cannot
// honour, so the caller gets one clear refusal instead of a silently ignored
// flag.
func localOnlySearchOption(opts SearchOptions) string {
	switch {
	case opts.IncludeStubs:
		return "including stub records"
	case opts.MatchMode != "":
		return "match mode"
	case opts.Relaxed:
		return "a relaxed match"
	case opts.MinScore > 0:
		return "a minimum score"
	case opts.Explain:
		return "the ranking explanation"
	case opts.Full:
		return "full bodies"
	case opts.Subtype != "":
		return "the subtype filter"
	case opts.Scope != "":
		return "the scope filter"
	case opts.TopicKey != "":
		return "the topic filter"
	default:
		return ""
	}
}

// mergeRelaxedHits merges the strict page with the OR page: strict hits always
// hold the top of the page, OR-only hits follow, and each group is ordered by
// score (then id) inside itself. The partition is what makes the merge safe:
// the two passes normalize relevance over different match sets, so a raw score
// from the OR page is not comparable with a strict score and must never
// outrank a record that matched every term.
//
// Records are deduped by project and id; a record in both pages keeps its
// strict position and score. used reports whether the relaxed page added
// anything; an empty OR page leaves the strict page untouched.
func mergeRelaxedHits(strict, relaxed []ports.Hit, limit int) ([]ports.Hit, bool) {
	if len(relaxed) == 0 {
		return strict, false
	}
	key := func(hit ports.Hit) string { return hit.ProjectID + "\x00" + hit.ID }
	seen := make(map[string]struct{}, len(relaxed)+len(strict))
	merged := make([]ports.Hit, 0, len(relaxed)+len(strict))
	for _, hit := range strict {
		id := key(hit)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		merged = append(merged, hit)
	}
	strictCount := len(merged)
	for _, hit := range relaxed {
		id := key(hit)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		merged = append(merged, hit)
	}
	byScore := func(hits []ports.Hit) {
		sort.SliceStable(hits, func(left, right int) bool {
			if hits[left].Score != hits[right].Score {
				return hits[left].Score > hits[right].Score
			}
			return hits[left].ID < hits[right].ID
		})
	}
	byScore(merged[:strictCount])
	byScore(merged[strictCount:])
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, true
}

// dropRawKinds removes raw-zone kinds from a response that did not request
// them.
func dropRawKinds(hits []ports.Hit) []ports.Hit {
	kept := make([]ports.Hit, 0, len(hits))
	for _, hit := range hits {
		if hit.Kind == KindItem || hit.Kind == KindDump {
			continue
		}
		kept = append(kept, hit)
	}
	return kept
}

// searchTermCount reports how many whitespace-separated terms the query has.
// The relaxed pass only makes sense with two or more terms.
func searchTermCount(text string) int {
	count := 0
	for _, field := range strings.Fields(text) {
		for _, r := range field {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				count++
				break
			}
		}
	}
	return count
}

// hitKey identifies a hit inside one response.
func hitKey(hit ports.Hit) string {
	return hit.ProjectID + "\x00" + hit.ID
}

// hitKeys is the key set of a page, used to remember which hits came from the
// strict pass.
func hitKeys(hits []ports.Hit) map[string]bool {
	keys := make(map[string]bool, len(hits))
	for _, hit := range hits {
		keys[hitKey(hit)] = true
	}
	return keys
}

// collapseDuplicateHits keeps one row per non-empty content hash. The oldest
// record of a group is the canonical one: the same record written twice on one
// machine or once on two clones is one answer. The canonical row takes the best
// rank its group reached, and the other copies are returned with the duplicate
// reason instead of silently disappearing.
//
// Project scope is part of the key: the indexed hash covers kind and content,
// so the same sentence can legitimately appear in two projects.
func collapseDuplicateHits(hits []ports.Hit) ([]ports.Hit, []ExcludedHit) {
	if len(hits) < 2 {
		return hits, nil
	}
	type group struct {
		first     int
		canonical int
	}
	groups := make(map[string]*group, len(hits))
	keys := make([]string, len(hits))
	for index := range hits {
		hash := strings.TrimSpace(hits[index].ContentHash)
		if hash == "" {
			continue
		}
		key := hits[index].ProjectID + "\x00" + hash
		keys[index] = key
		existing, seen := groups[key]
		if !seen {
			groups[key] = &group{first: index, canonical: index}
			continue
		}
		if olderHit(hits[index], hits[existing.canonical]) {
			existing.canonical = index
		}
	}
	kept := make([]ports.Hit, 0, len(hits))
	var excluded []ExcludedHit
	for index := range hits {
		group, grouped := groups[keys[index]]
		switch {
		case keys[index] == "" || !grouped:
			kept = append(kept, hits[index])
		case index == group.first:
			kept = append(kept, hits[group.canonical])
			if group.canonical != group.first {
				// The best-ranked copy is not the canonical one: it gives its
				// slot to the oldest row and is reported as the duplicate.
				excluded = append(excluded, excludedHit(hits[group.first], ExcludedDuplicate))
			}
		case index == group.canonical:
			// Emitted at the first (best-ranked) position of its group.
		default:
			excluded = append(excluded, excludedHit(hits[index], ExcludedDuplicate))
		}
	}
	return kept, excluded
}

// olderHit reports whether left was created before right. The index projects
// record validity from the creation instant; a document without validity falls
// back to its ULID id, which is time ordered across machines.
func olderHit(left, right ports.Hit) bool {
	if !left.ValidFrom.IsZero() && !right.ValidFrom.IsZero() && !left.ValidFrom.Equal(right.ValidFrom) {
		return left.ValidFrom.Before(right.ValidFrom)
	}
	return left.ID < right.ID
}

// excludedHit copies the visible fields of a withheld hit.
func excludedHit(hit ports.Hit, reason string) ExcludedHit {
	return ExcludedHit{
		ID: hit.ID, ProjectID: hit.ProjectID, Kind: hit.Kind,
		Title: hit.Title, Score: hit.Score, Reason: reason,
	}
}

// supportGate classifies a matched page as an answer, as nearby text or as
// nothing.
//
// A hit is supported when it contains every rare query term (a term whose
// document frequency is at most a fifth of the project) and at least half of
// the remaining subject terms. A strict-pass hit always qualifies: the strict
// pass already required every term. A hit that only matched the relaxed pass
// and fails the test is related. A page with no supported hit stays visible as
// related while its best record covers at least one rare query term and two
// subject terms; a page below that is not nearby text either. In that case the
// hits are returned as excluded with the lower-rank reason and the reason says
// what the query lacked, so a caller can never mistake relaxed noise for a
// result.
//
// strict is the key set of the strict pass; nil or empty means the page came
// from a single OR pass (MatchModeAny).
func (a *App) supportGate(ctx context.Context, opts SearchOptions, hits []ports.Hit, strict map[string]bool) (Support, []ports.Hit, []ExcludedHit) {
	support := Support{Threshold: supportSubjectShare}
	// An explicit --relaxed or --match-mode any is the caller asking for
	// partial matches; keep them visible as related instead of hiding them.
	if opts.Relaxed || opts.MatchMode == ports.MatchModeAny {
		support.Tier = SupportRelated
		support.Reason = "partial matching was requested explicitly"
		for index := range hits {
			hits[index].Tier = SupportRelated
		}
		return support, hits, nil
	}
	terms := supportTerms(opts.Text)
	if len(terms) == 0 {
		if len(hits) == 0 {
			support.Tier = SupportNone
			support.Reason = "the query has no searchable subject terms"
			return support, hits, nil
		}
		for index := range hits {
			hits[index].Tier = SupportRelated
		}
		support.Tier = SupportRelated
		support.Reason = "the query has no subject terms; the page is a lexical match only"
		return support, hits, nil
	}
	index, ok := a.deps.Index.(termFrequencyIndex)
	if !ok {
		support.Reason = "the index cannot report document frequency; support was not evaluated"
		return support, hits, nil
	}
	frequencies, total, err := index.TermFrequencies(ctx, strings.TrimSpace(opts.ProjectID), terms)
	if err != nil || total == 0 {
		support.Reason = "the index could not report document frequency; support was not evaluated"
		return support, hits, nil
	}
	// At least one document makes a term rare: a term that occurs once in a
	// five-document project is still the term the query is about. Without the
	// floor a small corpus would call every term common and accept a partial
	// match as an answer.
	rareLimit := int(math.Floor(supportRareShare * float64(total)))
	if rareLimit < 1 {
		rareLimit = 1
	}
	rare := make([]string, 0, len(terms))
	common := make([]string, 0, len(terms))
	for _, term := range terms {
		if frequencies[term] <= rareLimit {
			rare = append(rare, term)
			continue
		}
		common = append(common, term)
	}
	commonNeeded := int(math.Ceil(supportSubjectShare * float64(len(common))))
	best := 0.0
	bestRare := 0
	bestCovered := 0
	supported := 0
	passes := make([]bool, len(hits))
	for index := range hits {
		tokens := newHitTokens(hits[index].Title + "\n" + hits[index].Body)
		covered, rareCovered, commonCovered := 0, 0, 0
		for _, term := range rare {
			if tokens.covers(term) {
				covered++
				rareCovered++
			}
		}
		for _, term := range common {
			if tokens.covers(term) {
				covered++
				commonCovered++
			}
		}
		ratio := float64(covered) / float64(len(terms))
		passes[index] = rareCovered == len(rare) && commonCovered >= commonNeeded
		if strict[hitKey(hits[index])] {
			passes[index] = true
			ratio = 1
		}
		if passes[index] {
			supported++
		}
		if ratio > best {
			best = ratio
		}
		if rareCovered > bestRare {
			bestRare = rareCovered
		}
		if covered > bestCovered {
			bestCovered = covered
		}
	}
	support.Best = best
	if supported > 0 {
		support.Tier = SupportSupported
		for index := range hits {
			if passes[index] {
				hits[index].Tier = SupportSupported
				continue
			}
			hits[index].Tier = SupportRelated
		}
		return support, hits, nil
	}
	// A page that overlaps the topic but does not answer it stays visible as
	// related: a caller can see that the records are nearby. A page with no
	// rare term at all is not nearby text either - that is the absent-topic
	// case, and it must not be presented as an answer.
	if len(hits) > 0 && bestRare > 0 && bestCovered >= supportRelatedMinTerms {
		support.Tier = SupportRelated
		support.Reason = "the page only matched the relaxed pass; no record contains every rare query term"
		for index := range hits {
			hits[index].Tier = SupportRelated
		}
		return support, hits, nil
	}
	if len(hits) == 0 {
		support.Tier = SupportNone
		support.Reason = "no record matches the query terms"
		return support, hits, nil
	}
	support.Tier = SupportNone
	support.Reason = fmt.Sprintf(
		"no record contains every rare query term (document frequency <= %d%%) and at least %d%% of the remaining subject terms",
		int(math.Round(supportRareShare*100)), int(math.Round(supportSubjectShare*100)))
	excluded := make([]ExcludedHit, 0, len(hits))
	for index := range hits {
		excluded = append(excluded, excludedHit(hits[index], ExcludedLowerRank))
	}
	return support, []ports.Hit{}, excluded
}

// supportStopWords are the question and function words the support gate never
// requires a hit to contain. internal/app/findings.go holds the domain
// vocabulary stopwords; this set adds the question frame, which a topical
// decision answers without repeating.
var supportStopWords = map[string]bool{
	"about": true, "after": true, "all": true, "also": true, "an": true,
	"and": true, "any": true, "are": true, "as": true, "at": true,
	"be": true, "been": true, "before": true, "being": true, "between": true,
	"both": true, "but": true, "by": true, "can": true, "could": true,
	"did": true, "do": true, "does": true, "down": true, "each": true,
	"every": true, "for": true, "from": true, "had": true, "has": true,
	"have": true, "how": true, "if": true, "in": true, "instead": true, "into": true,
	"is": true, "it": true, "its": true, "just": true, "may": true,
	"might": true, "more": true, "most": true, "must": true, "no": true,
	"not": true, "of": true, "off": true, "on": true, "one": true, "only": true,
	"or": true, "other": true, "our": true, "out": true, "over": true,
	"own": true, "per": true, "same": true, "should": true, "so": true,
	"some": true, "than": true, "that": true, "the": true, "their": true,
	"them": true, "then": true, "there": true, "these": true, "they": true,
	"this": true, "those": true, "to": true, "too": true, "under": true,
	"up": true, "very": true, "via": true, "was": true, "we": true,
	"were": true, "what": true, "when": true, "where": true, "which": true,
	"who": true, "whom": true, "whose": true, "why": true, "will": true,
	"with": true, "without": true, "would": true, "you": true, "your": true,
}

// supportTerms returns the distinct subject terms of a query, in query order.
// The gate never requires a function word: a natural-language question is
// answered by a record that does not repeat "how" or "the".
func supportTerms(text string) []string {
	seen := make(map[string]struct{}, 8)
	terms := make([]string, 0, 8)
	for _, token := range lowerTokens(text) {
		if len(token) < 2 || stopWords[token] || supportStopWords[token] {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		terms = append(terms, token)
	}
	return terms
}

// lowerTokens splits text into lowercase letter and digit runs.
func lowerTokens(text string) []string {
	tokens := make([]string, 0, 16)
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		tokens = append(tokens, current.String())
		current.Reset()
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return tokens
}

// hitTokens is the lowercase token set of one document.
type hitTokens map[string]struct{}

// newHitTokens builds the token set a support check reads.
func newHitTokens(text string) hitTokens {
	tokens := make(hitTokens, 16)
	for _, token := range lowerTokens(text) {
		tokens[token] = struct{}{}
	}
	return tokens
}

// covers reports whether the document contains term. It mirrors the index's
// match rule: exact, prefix for long terms, and the word-form stem, so "signed"
// is covered by "sign" and "releases" by "release".
func (tokens hitTokens) covers(term string) bool {
	if _, ok := tokens[term]; ok {
		return true
	}
	if len(term) < supportPrefixRunes {
		return false
	}
	stem, hasStem := searchnorm.Stem(term)
	for token := range tokens {
		if strings.HasPrefix(token, term) {
			return true
		}
		if len(token) >= supportPrefixRunes && strings.HasPrefix(term, token) {
			return true
		}
		if !hasStem {
			continue
		}
		if len(stem) >= supportPrefixRunes && strings.HasPrefix(token, stem) {
			return true
		}
		if len(token) >= supportPrefixRunes && strings.HasPrefix(stem, token) {
			return true
		}
	}
	return false
}
