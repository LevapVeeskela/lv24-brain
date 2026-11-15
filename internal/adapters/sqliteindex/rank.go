package sqliteindex

import (
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Ranking weights of the composite score. They are the only place these
// numbers live: the SQL is rendered from them, so a change here changes both
// the order and the --explain contributions. The weights sum to 1, which keeps
// the composite score in [0, 1] and makes a score threshold meaningful.
const (
	// WeightBM25 is the share of the lexical FTS5 relevance.
	WeightBM25 = 0.55
	// WeightRecency is the share of the 30-day half-life freshness.
	WeightRecency = 0.20
	// WeightImportance is the share of the kind's authority (see
	// importanceByKind).
	WeightImportance = 0.15
	// WeightPin is the share of the pin/confidence signal: an accepted
	// decision or a record with a stated author confidence.
	WeightPin = 0.05
	// WeightLink is the share of the link-graph signal: a record whose body
	// references another record with [[id]].
	WeightLink = 0.05

	// bm25TitleWeight and bm25BodyWeight are the FTS5 field weights: a query
	// term in the title is worth three times the same term in the body.
	bm25TitleWeight = 3.0
	bm25BodyWeight  = 1.0
	// scoreOffset turns the weighted sum into the reported score:
	// score = total / (total + scoreOffset). The map is strictly increasing
	// and bounded, so it preserves the ranking order while keeping the score
	// in [0, 1).
	scoreOffset = 1.0

	// recencyHalfLifeDays is the freshness half-life: a record 30 days old
	// scores half of a record written now.
	recencyHalfLifeDays = 30.0
	// secondsPerDay converts the Unix-second age into days.
	secondsPerDay = 86400.0
	// ln2 is the natural log of 2, used by exp(-ln2 * age / halfLife).
	ln2 = 0.6931471805599453

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
// ports.Explain lists them. signal is the level-1 strength column.
var signalColumns = []struct {
	name   string
	signal string
	weight float64
}{
	{"c_bm25", "bm", WeightBM25},
	{"c_recency", "rec", WeightRecency},
	{"c_importance", "imp", WeightImportance},
	{"c_pin", "pin", WeightPin},
	{"c_link", "lnk", WeightLink},
}

// contributionExpr is the unnormalized weighted contribution of one signal.
func contributionExpr(signal string, weight float64) string {
	return floatLiteral(weight) + " * " + signal
}

// totalExpr is the weighted sum the ranking orders by. It must stay the sum of
// the same terms contributionExpr produces: the score divides every term by
// the same denominator, so the contributions still add up to the score.
func totalExpr() string {
	parts := make([]string, 0, len(signalColumns))
	for _, column := range signalColumns {
		parts = append(parts, contributionExpr(column.signal, column.weight))
	}
	return "(" + strings.Join(parts, " + ") + ")"
}

// scoreExpr maps the weighted sum into [0, 1) without changing the order.
func scoreExpr() string {
	return "total / (total + " + floatLiteral(scoreOffset) + ")"
}

// rankedSelectPrefix returns the head of the ranking query:
//
//	level 1 (innermost): MATCH and the filters; the raw signal strengths and
//	                     one bm25() evaluation per row;
//	level 2:             the weighted contributions w_* and their sum as total;
//	level 3:             the bounded score, the per-signal shares, the score
//	                     threshold, the deterministic order and the page limit.
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
	builder.WriteString("SELECT id, project_id, kind, title, snippet, body,\n")
	builder.WriteString("\t\t" + scoreExpr() + " AS score,\n")
	for _, column := range signalColumns {
		builder.WriteString("\t\tw_" + column.signal + " / (total + " + floatLiteral(scoreOffset) + ") AS " + column.name + ",\n")
	}
	builder.WriteString("\t\tvalid_from, valid_to, superseded_by, status, confidence, updated_at\n\tFROM (\n")
	builder.WriteString("\t\tSELECT id, project_id, kind, title, snippet, body,\n")
	for _, column := range signalColumns {
		builder.WriteString("\t\t\t" + contributionExpr(column.signal, column.weight) + " AS w_" + column.signal + ",\n")
	}
	builder.WriteString("\t\t\t" + totalExpr() + " AS total,\n")
	builder.WriteString("\t\t\tvalid_from, valid_to, superseded_by, status, confidence, updated_at\n\t\tFROM (\n")
	builder.WriteString("\t\t\tSELECT d.id AS id, d.project_id AS project_id, d.kind AS kind, d.title AS title,\n")
	builder.WriteString("\t\t\t\tsnippet(documents_fts, -1, ?, ?, ?, ?) AS snippet,\n")
	builder.WriteString("\t\t\t\t" + body + " AS body,\n")
	builder.WriteString("\t\t\t\tMAX(0.0, -bm25(documents_fts, " + floatLiteral(bm25TitleWeight) + ", " + floatLiteral(bm25BodyWeight) + ")) AS bm,\n")
	builder.WriteString("\t\t\t\texp(-" + floatLiteral(ln2) + " * MAX(0.0, ? - d.updated_at / 1000000000.0)\n")
	builder.WriteString("\t\t\t\t\t/ (" + floatLiteral(secondsPerDay) + " * " + floatLiteral(recencyHalfLifeDays) + ")) AS rec,\n")
	builder.WriteString("\t\t\t\tCASE d.kind\n")
	for _, kind := range []string{ports.RecordDecision, ports.RecordAttempt, ports.RecordHandoff, ports.RecordNote} {
		builder.WriteString("\t\t\t\t\tWHEN '" + kind + "' THEN " + floatLiteral(importanceByKind[kind]) + "\n")
	}
	builder.WriteString("\t\t\t\t\tELSE " + floatLiteral(defaultImportance) + "\n\t\t\t\tEND AS imp,\n")
	builder.WriteString("\t\t\t\tMAX(\n")
	builder.WriteString("\t\t\t\t\tCASE WHEN d.kind = 'decision' AND d.status = 'accepted' THEN 1.0 ELSE 0.0 END,\n")
	builder.WriteString("\t\t\t\t\tMIN(1.0, COALESCE(d.confidence, 0.0))\n")
	builder.WriteString("\t\t\t\t) AS pin,\n")
	builder.WriteString("\t\t\t\tCASE WHEN instr(d.body, '[[') > 0 AND instr(d.body, ']]') > 0 THEN 1.0 ELSE 0.0 END AS lnk,\n")
	builder.WriteString("\t\t\t\td.valid_from AS valid_from, d.valid_to AS valid_to, d.superseded_by AS superseded_by,\n")
	builder.WriteString("\t\t\t\td.status AS status, d.confidence AS confidence, d.updated_at AS updated_at\n")
	builder.WriteString("\t\t\tFROM documents_fts\n")
	builder.WriteString("\t\t\tJOIN documents d ON d.rowid = documents_fts.rowid\n")
	builder.WriteString("\t\t\tWHERE documents_fts MATCH ?")
	return builder.String()
}

// rankedSelectSuffix closes the subqueries, drops hits below the score
// threshold and orders the result deterministically: score first, then newest
// update, then id, so paging never repeats or skips a row.
func rankedSelectSuffix() string {
	return `
	)
)
WHERE ` + scoreExpr() + ` >= ?
ORDER BY score DESC, updated_at DESC, id ASC
LIMIT ?`
}

// floatLiteral renders a constant for the SQL text. Only package constants are
// passed here; query values are always bound as parameters.
func floatLiteral(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
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

// pinned reports whether the pin signal fired: an accepted decision or a
// record with a stated confidence.
func pinned(kind, status string, confidence float64) bool {
	return (kind == ports.RecordDecision && status == "accepted") || confidence > 0
}

// matchReasons names the signals that fired for a hit, in the documented
// order: bm25, recency, importance, pin, link. bm25 always fires because the
// document matched at least one query term.
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
	if contributions[4] > 0 {
		reasons = append(reasons, ports.ReasonLink)
	}
	return reasons
}
