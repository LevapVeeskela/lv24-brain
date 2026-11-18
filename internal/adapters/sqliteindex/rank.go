package sqliteindex

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Ranking v2 weights. Every signal below is normalized to [0, 1] before it is
// weighted, so the weights are the share each signal owns and the composite
// score is the weighted sum: a score in [0, 1] whose parts add up to the score
// itself, which is what `--explain` prints as the absolute contribution.
//
// The relevance term used to be measured on the raw bm25 scale, which is
// negative, corpus dependent and unbounded. It is normalized per query instead
// (see relevanceExpr), so a change here changes both the order and the
// explained contributions.
const (
	// WeightBM25 is the share of the normalized lexical relevance.
	WeightBM25 = 0.55
	// WeightRecency is the share of the 30-day half-life freshness.
	WeightRecency = 0.20
	// WeightPin is the share of the pin/confidence signal: an explicit pin, an
	// accepted decision or a stated author confidence.
	WeightPin = 0.15
	// WeightImportance is the share of the kind's authority (see
	// importanceByKind).
	WeightImportance = 0.05
	// WeightTrust is the share of the provenance trust of a record: verified
	// 1.0, stated 0.6, inferred 0.3. It stays a small additive term on purpose:
	// auto-captured, inferred records are primary content, so trust must break
	// ties without burying them.
	WeightTrust = 0.05

	// bm25TitleWeight and bm25BodyWeight are the FTS5 field weights: a query
	// term in the title is worth three times the same term in the body.
	bm25TitleWeight = 3.0
	bm25BodyWeight  = 1.0

	// recencyHalfLifeDays is the freshness half-life: a record 30 days old
	// scores half of a record written now.
	recencyHalfLifeDays = 30.0
	// secondsPerDay converts the Unix-second age into days.
	secondsPerDay = 86400.0
	// ln2 is the natural log of 2, used by exp(-ln2 * age / halfLife).
	ln2 = 0.6931471805599453

	// smallMatchCount is the match count below which relevance is top-anchored:
	// with one or two matches there is no distribution to scale, so the best
	// match scores 1.0 and a second match keeps its ratio to it. That is
	// rank-based (rank 1 always scores 1.0) without stretching a small quality
	// gap into a full [0, 1] swing.
	smallMatchCount = 3

	// trustVerified, trustStated and trustInferred are the strength a projected
	// provenance trust level scores. They are the values the ranking trusts:
	// an unsupported claim must never rank like a confirmed one.
	trustVerified = 1.0
	trustStated   = 0.6
	trustInferred = 0.3
	// defaultTrust is the trust a record scores when the projection has no
	// provenance value: a record written before schema v9, or a document with
	// no curated provenance (state, dump, item). An unknown value is not
	// guessed down, it stays neutral, so an upgrade does not reorder the
	// corpus.
	defaultTrust = 1.0

	// reasonFreshDays is the age within one half-life at which the recency
	// signal is reported as one that fired.
	reasonFreshDays = recencyHalfLifeDays
	// reasonImportanceMin is the kind importance from which the importance
	// signal is reported as one that fired.
	reasonImportanceMin = 0.7
)

// importanceByKind is the authority of a record kind. A decision governs code,
// so it outweighs the narrative kinds; raw dumps and items rank lowest because
// they are noise unless explicitly requested.
var importanceByKind = map[string]float64{
	ports.RecordDecision: 1.0,
	ports.RecordAttempt:  0.8,
	ports.RecordHandoff:  0.7,
	ports.RecordNote:     0.5,
}

// defaultImportance is used for kinds without an entry (state, dump, item).
const defaultImportance = 0.3

// signalColumns are the weighted contribution columns, in the order
// ports.Explain lists them. signal is the level-1 strength column; the c_link
// column stays in the projection for the API shape but carries no signal since
// the link bonus was replaced by the provenance trust.
//
// The provenance trust travels in the importance column: ports.Explain has
// five slots and trust, like kind authority, is an authority signal. The two
// weighted terms are added into c_imp, so the five explained contributions
// still add up to the score and their shares add up to 1.
var signalColumns = []struct {
	name   string
	signal string
	weight float64
}{
	{"c_bm25", "bm_norm", WeightBM25},
	{"c_recency", "rec", WeightRecency},
	{"c_importance", "authority", WeightImportance},
	{"c_pin", "pin", WeightPin},
	{"c_link", "", 0},
}

// contributionExpr is the weighted contribution of one level-1 signal.
func contributionExpr(signal string, weight float64) string {
	return floatLiteral(weight) + " * " + signal
}

// trustExpr is the provenance trust strength the ranking reads from the
// projection: verified 1.0, stated 0.6, inferred 0.3. A row without a trust
// value (written before schema v9, or a document with no curated provenance)
// stays neutral (defaultTrust) instead of being guessed down.
func trustExpr() string {
	return "CASE d.trust\n" +
		"\t\t\t\tWHEN '" + string(domain.TrustVerified) + "' THEN " + floatLiteral(trustVerified) + "\n" +
		"\t\t\t\tWHEN '" + string(domain.TrustStated) + "' THEN " + floatLiteral(trustStated) + "\n" +
		"\t\t\t\tWHEN '" + string(domain.TrustInferred) + "' THEN " + floatLiteral(trustInferred) + "\n" +
		"\t\t\t\tELSE " + floatLiteral(defaultTrust) + "\n" +
		"\t\t\tEND"
}

// relevanceExpr normalizes the bm25 of one match to [0, 1].
//
// bm25() is negative (a better match is more negative), so the level-1 signal
// is bm = -bm25, already clamped at 0. Above smallMatchCount matches the scale
// is min-max over the matched set: the weakest match scores 0, the strongest
// 1.0 and every match keeps its relative position, so a strong match still
// outranks a merely fresh one. Below it the scale is top-anchored (rank 1
// scores 1.0, a second match keeps its ratio to the best) because one or two
// samples say nothing about the shape of the distribution. A flat set (all
// matches equally strong) scores 1.0 instead of dividing by zero, and an empty
// set never reaches this CASE.
func relevanceExpr() string {
	return "CASE\n" +
		"\t\t\t\tWHEN n < 1 THEN 0.0\n" +
		"\t\t\t\tWHEN n < 2 THEN 1.0\n" +
		"\t\t\t\tWHEN n < " + strconv.Itoa(smallMatchCount) + " THEN CASE WHEN bm_max <= 0.0 THEN 0.0 ELSE MIN(1.0, bm / bm_max) END\n" +
		"\t\t\t\tWHEN bm_max - bm_min <= 0.0 THEN 1.0\n" +
		"\t\t\t\tELSE MIN(1.0, (bm - bm_min) / (bm_max - bm_min))\n" +
		"\t\t\tEND"
}

// rankedSelectPrefix returns the head of the ranking query. The exposed
// subqueries are:
//
//	matched:        the FTS5 MATCH, the filters and the raw signal strengths,
//	                one bm25() evaluation per row;
//	ranked:         the match count and the bm25 range (min, max) the
//	                relevance of this pass is normalized over;
//	ranked_scored:  the normalized relevance and the authority term;
//	ranked_contrib: the weighted contributions and their sum as score.
//
// The four snippet placeholders and the ranking instant come first, in that
// order, followed by the MATCH placeholder; the filter placeholders are
// appended by buildSearchStatement between the prefix and the suffix.
func rankedSelectPrefix(includeBody bool) string {
	body := "''"
	if includeBody {
		body = "d.body"
	}
	var builder strings.Builder
	builder.WriteString("WITH matched AS (\n")
	builder.WriteString("\t\tSELECT d.id AS id, d.project_id AS project_id, d.kind AS kind, d.title AS title,\n")
	builder.WriteString("\t\t\tsnippet(documents_fts, -1, ?, ?, ?, ?) AS snippet,\n")
	builder.WriteString("\t\t\t" + body + " AS body,\n")
	builder.WriteString("\t\t\tMAX(0.0, -bm25(documents_fts, " + floatLiteral(bm25TitleWeight) + ", " + floatLiteral(bm25BodyWeight) + ")) AS bm,\n")
	builder.WriteString("\t\t\texp(-" + floatLiteral(ln2) + " * MAX(0.0, ? - d.updated_at / 1000000000.0)\n")
	builder.WriteString("\t\t\t\t/ (" + floatLiteral(secondsPerDay) + " * " + floatLiteral(recencyHalfLifeDays) + ")) AS rec,\n")
	builder.WriteString("\t\t\tCASE d.kind\n")
	for _, kind := range []string{ports.RecordDecision, ports.RecordAttempt, ports.RecordHandoff, ports.RecordNote} {
		builder.WriteString("\t\t\t\tWHEN '" + kind + "' THEN " + floatLiteral(importanceByKind[kind]) + "\n")
	}
	builder.WriteString("\t\t\t\tELSE " + floatLiteral(defaultImportance) + "\n\t\t\tEND AS imp,\n")
	builder.WriteString("\t\t\t" + trustExpr() + " AS trs,\n")
	builder.WriteString("\t\t\tMAX(\n")
	builder.WriteString("\t\t\t\tCASE WHEN d.pinned = 1 THEN 1.0 ELSE 0.0 END,\n")
	builder.WriteString("\t\t\t\tCASE WHEN d.kind = 'decision' AND d.status = 'accepted' THEN 1.0 ELSE 0.0 END,\n")
	builder.WriteString("\t\t\t\tMIN(1.0, COALESCE(d.confidence, 0.0))\n")
	builder.WriteString("\t\t\t) AS pin,\n")
	builder.WriteString("\t\t\td.valid_from AS valid_from, d.valid_to AS valid_to, d.superseded_by AS superseded_by,\n")
	builder.WriteString("\t\t\td.status AS status, d.confidence AS confidence,\n")
	builder.WriteString("\t\t\td.subtype AS subtype, d.scope AS scope, d.topic_key AS topic_key, d.occurrences AS occurrences,\n")
	builder.WriteString("\t\t\td.pinned AS pinned,\n")
	builder.WriteString("\t\t\td.content_hash AS content_hash,\n")
	builder.WriteString("\t\t\td.updated_at AS updated_at\n")
	builder.WriteString("\t\tFROM documents_fts\n")
	builder.WriteString("\t\tJOIN documents d ON d.rowid = documents_fts.rowid\n")
	builder.WriteString("\t\tWHERE documents_fts MATCH ?")
	return builder.String()
}

// rankedSelectSuffix closes the CTEs and returns the ranked page. The score
// threshold, the deterministic order and the page limit are applied after the
// normalization, so a MinScore filters the same composite score the caller
// sees.
func rankedSelectSuffix() string {
	var builder strings.Builder
	builder.WriteString(`
	),
	ranked AS (
		SELECT matched.*,
			COUNT(*) OVER () AS n,
			MIN(bm) OVER () AS bm_min,
			MAX(bm) OVER () AS bm_max
		FROM matched
	),
	ranked_scored AS (
		SELECT ranked.*,
			`)
	builder.WriteString(relevanceExpr())
	builder.WriteString(` AS bm_norm,
			imp * ` + floatLiteral(WeightImportance) + ` + trs * ` + floatLiteral(WeightTrust) + ` AS authority
		FROM ranked
	),
	ranked_contrib AS (
		SELECT ranked_scored.*,
			`)
	builder.WriteString(contributionExpr("bm_norm", WeightBM25) + " AS c_bm25,\n\t\t\t")
	builder.WriteString(contributionExpr("rec", WeightRecency) + " AS c_recency,\n\t\t\t")
	builder.WriteString("authority AS c_importance,\n\t\t\t")
	builder.WriteString(contributionExpr("pin", WeightPin) + " AS c_pin,\n\t\t\t")
	builder.WriteString("0.0 AS c_link,\n\t\t\t")
	builder.WriteString(contributionExpr("bm_norm", WeightBM25) + " + " + contributionExpr("rec", WeightRecency) + " + authority + " + contributionExpr("pin", WeightPin) + " AS score\n")
	builder.WriteString(`		FROM ranked_scored
	)
	SELECT id, project_id, kind, title, snippet, body, score,
		c_bm25, c_recency, c_importance, c_pin, c_link,
		valid_from, valid_to, superseded_by, status, confidence, subtype, scope, topic_key, occurrences, pinned, content_hash, updated_at
	FROM ranked_contrib
	WHERE score >= ?
	ORDER BY score DESC, bm DESC, updated_at DESC, id ASC
	LIMIT ?`)
	return builder.String()
}

// floatLiteral renders a constant for the SQL text. Only package constants are
// passed here; query values are always bound as parameters.
func floatLiteral(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// TermFrequencies reports the document frequency of each query term over the
// project index and the number of documents in it. The retrieval support gate
// uses it to separate a rare, discriminative term from a common one: a term
// that appears in more than a fifth of the project cannot be required in every
// supported hit.
//
// The frequency uses the same FTS expression as search (quoted, prefix and
// word-form stem), so a count is the number of documents search can return for
// that term. An empty projectID counts the whole index. Stubs are counted:
// they stay in the index and in counts, only retrieval hides them.
func (i *Index) TermFrequencies(ctx context.Context, projectID string, terms []string) (map[string]int, int, error) {
	frequencies := make(map[string]int, len(terms))
	projectID = strings.TrimSpace(projectID)
	totalQuery := `SELECT COUNT(*) FROM documents`
	var totalArgs []any
	if projectID != "" {
		totalQuery += ` WHERE project_id = ?`
		totalArgs = append(totalArgs, projectID)
	}
	var total int
	if err := i.db.QueryRowContext(ctx, totalQuery, totalArgs...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("sqliteindex: count documents: %w", err)
	}
	dfQuery := `SELECT COUNT(*) FROM documents_fts
		JOIN documents d ON d.rowid = documents_fts.rowid
		WHERE documents_fts MATCH ?`
	if projectID != "" {
		dfQuery += ` AND d.project_id = ?`
	}
	for _, term := range terms {
		match, ok := matchExpression(term, joinAll)
		if !ok {
			continue
		}
		args := []any{match}
		if projectID != "" {
			args = append(args, projectID)
		}
		var count int
		if err := i.db.QueryRowContext(ctx, dfQuery, args...).Scan(&count); err != nil {
			return nil, 0, fmt.Errorf("sqliteindex: count %q: %w", term, err)
		}
		frequencies[term] = count
	}
	return frequencies, total, nil
}

// importanceOf returns the kind importance a ranked hit received.
func importanceOf(kind string) float64 {
	if value, ok := importanceByKind[kind]; ok {
		return value
	}
	return defaultImportance
}

// isFresh reports whether updatedAt is younger than one recency half-life at
// the ranking instant, the age at which the recency reason fires.
func isFresh(updatedAt, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	return now.Sub(updatedAt) <= time.Duration(reasonFreshDays*24)*time.Hour
}

// pinned reports whether the pin signal fired: an explicitly pinned record, an
// accepted decision or a record with a stated confidence.
func pinned(kind, status string, confidence float64, isPinned bool) bool {
	return isPinned || (kind == ports.RecordDecision && status == "accepted") || confidence > 0
}

// matchReasons names the signals that fired for a hit, in the documented
// order: bm25, recency, importance, pin. bm25 always fires because the
// document matched at least one query term. Trust shares the importance column
// with the kind authority, so it never adds a reason of its own.
func matchReasons(kind string, contributions []float64, fresh, isPinned bool) []string {
	reasons := make([]string, 0, len(signalColumns))
	if len(contributions) != len(signalColumns) {
		return reasons
	}
	if contributions[0] > 0 {
		reasons = append(reasons, ports.ReasonBM25)
	}
	if fresh {
		reasons = append(reasons, ports.ReasonRecency)
	}
	if importanceOf(kind) >= reasonImportanceMin {
		reasons = append(reasons, ports.ReasonImportance)
	}
	if isPinned {
		reasons = append(reasons, ports.ReasonPin)
	}
	return reasons
}
