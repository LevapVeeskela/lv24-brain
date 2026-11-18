package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
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
	// SearchFallbackThreshold hits and the terms were retried joined with OR.
	Relaxed bool
	// Notice explains a withheld raw-zone result, empty when nothing was
	// withheld.
	Notice string
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
		TopicKey: strings.TrimSpace(opts.TopicKey),
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
	// The preview stays a preview: --full adds the body next to it instead of
	// turning the snippet into one.
	for index := range hits {
		hits[index].Snippet = textbudget.Preview(hits[index].Snippet, PreviewRunes)
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
