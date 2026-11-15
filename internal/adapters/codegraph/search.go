package codegraph

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Search resolves symbols in three tiers. An exact indexed name wins, because
// it is the only tier that proves identity. Full text is a candidate tier and a
// substring is the last resort; every hit reports which tier produced it so a
// caller never presents a fuzzy candidate as an exact match.
func (i *Index) Search(ctx context.Context, query ports.GraphQuery) ([]ports.SymbolHit, error) {
	text := strings.TrimSpace(query.Text)
	if text == "" {
		return nil, domain.NewValidationError("graph.query", "is required")
	}
	limit := clampLimit(query.Limit, ports.DefaultGraphSearchLimit, ports.MaxGraphSearchLimit)
	kinds := normalizeKinds(query.Kinds)
	file := strings.TrimSpace(query.File)

	hits, err := i.searchExact(ctx, text, file, kinds, limit)
	if err != nil {
		return nil, err
	}
	if len(hits) > 0 {
		return hits, nil
	}
	if expression := ftsExpression(text); expression != "" {
		hits, err = i.searchFTS(ctx, expression, file, kinds, limit)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			return hits, nil
		}
	}
	return i.searchLike(ctx, text, file, kinds, limit)
}

// searchExact matches a name or fully qualified name, case-insensitively. A
// dotted query also matches the "::" qualified spelling CodeGraph uses.
func (i *Index) searchExact(ctx context.Context, text, file string, kinds []string, limit int) ([]ports.SymbolHit, error) {
	names := qualNameVariants(text)
	arguments := []any{text}
	arguments = append(arguments, stringsToAny(names)...)
	where := `(lower(n.name) = lower(?) OR lower(n.qualified_name) IN (` + placeholders(len(names)) + `))`
	arguments = append(arguments, text)
	clause, filterArguments := kindFileClause(kinds, file, "n")
	arguments = append(arguments, filterArguments...)
	arguments = append(arguments, limit)
	query := `SELECT ` + prefixedNodeColumns("n") + ` FROM nodes n WHERE ` + where + clause + `
		ORDER BY CASE WHEN lower(n.name) = lower(?) THEN 0 ELSE 1 END, ` + kindRankFor("n") + `, n.file_path, n.start_line
		LIMIT ?`
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("codegraph: exact search: %w", err)
	}
	return i.collectHits(rows, ports.GraphMatchExact, 1.0, func(int) float64 { return 100 })
}

// searchFTS matches the nodes_fts mirror. bm25 weights name above qualified
// name, signature and docstring, so a name hit does not lose to a docstring
// that repeats a word.
func (i *Index) searchFTS(ctx context.Context, expression, file string, kinds []string, limit int) ([]ports.SymbolHit, error) {
	clause, filterArguments := kindFileClause(kinds, file, "n")
	arguments := append([]any{expression}, filterArguments...)
	arguments = append(arguments, limit)
	query := `SELECT ` + prefixedNodeColumns("n") + `, bm25(nodes_fts, 1.0, 10.0, 5.0, 1.0, 2.0) AS rank
		FROM nodes_fts JOIN nodes n ON n.id = nodes_fts.id
		WHERE nodes_fts MATCH ?` + clause + `
		ORDER BY rank, n.file_path, n.start_line
		LIMIT ?`
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		// A malformed MATCH expression is a query problem, not an index
		// problem: report it instead of silently degrading.
		return nil, fmt.Errorf("codegraph: full-text search %q: %w", expression, err)
	}
	return i.collectHitsWithRank(rows, ports.GraphMatchFTS, 0.8)
}

// searchLike is the substring fallback. It searches the longest token only: a
// short token would match most of the index and answer nothing.
func (i *Index) searchLike(ctx context.Context, text, file string, kinds []string, limit int) ([]ports.SymbolHit, error) {
	token := longestToken(text)
	if len([]rune(token)) < 3 {
		return nil, nil
	}
	pattern := "%" + escapeLike(strings.ToLower(token)) + "%"
	clause, filterArguments := kindFileClause(kinds, file, "n")
	arguments := []any{pattern, pattern}
	arguments = append(arguments, filterArguments...)
	arguments = append(arguments, limit)
	query := `SELECT ` + prefixedNodeColumns("n") + ` FROM nodes n
		WHERE (lower(n.name) LIKE ? ESCAPE '\' OR lower(n.qualified_name) LIKE ? ESCAPE '\')` + clause + `
		ORDER BY ` + kindRankFor("n") + `, length(n.name), n.file_path, n.start_line
		LIMIT ?`
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("codegraph: substring search: %w", err)
	}
	return i.collectHits(rows, ports.GraphMatchLike, 0.5, func(index int) float64 { return 10 - float64(index) })
}

// collectHits scans node rows and applies a fixed tier score.
func (i *Index) collectHits(rows *sql.Rows, match string, confidence float64, score func(index int) float64) ([]ports.SymbolHit, error) {
	return i.collectHitsWithScore(rows, match, confidence, score)
}

// collectHitsWithRank scans rows whose last column is a bm25 rank and flips
// its sign: bm25 is negative and lower is better, so a higher Score is better.
func (i *Index) collectHitsWithRank(rows *sql.Rows, match string, confidence float64) ([]ports.SymbolHit, error) {
	return i.collectHitsWithScore(rows, match, confidence, nil)
}

func (i *Index) collectHitsWithScore(rows *sql.Rows, match string, confidence float64, score func(index int) float64) ([]ports.SymbolHit, error) {
	defer func() { _ = rows.Close() }()
	hits := make([]ports.SymbolHit, 0, 16)
	for rows.Next() {
		var symbol ports.GraphSymbol
		var signature, docstring, visibility sql.NullString
		var exported int
		dest := []any{&symbol.ID, &symbol.Kind, &symbol.Name, &symbol.QualName, &symbol.File,
			&symbol.Language, &symbol.StartLine, &symbol.EndLine, &signature, &docstring, &visibility, &exported}
		withRank := score == nil
		var rank float64
		if withRank {
			dest = append(dest, &rank)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("codegraph: scan symbol: %w", err)
		}
		symbol.Signature = signature.String
		symbol.Docstring = docstring.String
		symbol.Visibility = visibility.String
		symbol.Exported = exported != 0
		hit := ports.SymbolHit{
			GraphSymbol: symbol,
			Match:       match,
			Confidence:  confidence,
			Provenance:  "indexed",
		}
		if withRank {
			hit.Score = -rank
		} else {
			hit.Score = score(len(hits))
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// symbolsInFile returns the definitions of one path in source order. It backs
// path-pinned explore queries, where the path proves identity even when the
// name does not.
func (i *Index) symbolsInFile(ctx context.Context, path string, kinds []string, limit int) ([]ports.SymbolHit, error) {
	clause, arguments := kindFileClause(kinds, path, "n")
	arguments = append(arguments, limit)
	query := `SELECT ` + prefixedNodeColumns("n") + ` FROM nodes n WHERE 1 = 1` + clause + `
		ORDER BY n.start_line LIMIT ?`
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("codegraph: list symbols in %s: %w", path, err)
	}
	return i.collectHits(rows, ports.GraphMatchPath, 1.0, func(int) float64 { return 90 })
}

// kindFileClause builds the shared AND clauses and their arguments. An empty
// kind list keeps every kind: Node resolves file and import nodes too.
func kindFileClause(kinds []string, file, alias string) (string, []any) {
	column := func(name string) string {
		if alias == "" {
			return name
		}
		return alias + "." + name
	}
	clause := ""
	var arguments []any
	if len(kinds) > 0 {
		clause += ` AND ` + column("kind") + ` IN (` + placeholders(len(kinds)) + `)`
		arguments = append(arguments, stringsToAny(kinds)...)
	}
	if strings.TrimSpace(file) != "" {
		clause += ` AND ` + column("file_path") + ` = ?`
		arguments = append(arguments, strings.TrimSpace(file))
	}
	return clause, arguments
}

// ftsExpression turns free text into a safe FTS5 MATCH expression: quoted
// prefix terms joined with AND. Every character that FTS5 treats as syntax is
// dropped by the tokenizer, so a crafted query cannot alter the expression.
func ftsExpression(text string) string {
	tokens := queryTokens(text)
	if len(tokens) == 0 {
		return ""
	}
	parts := make([]string, 0, len(tokens))
	for _, token := range tokens {
		parts = append(parts, `"`+token+`"*`)
	}
	return strings.Join(parts, " AND ")
}

// queryTokens splits text into identifier tokens of at least two runes.
func queryTokens(text string) []string {
	var tokens []string
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		token := current.String()
		current.Reset()
		if len([]rune(token)) >= 2 {
			tokens = append(tokens, token)
		}
	}
	for _, char := range text {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || char == '_' {
			current.WriteRune(char)
			continue
		}
		flush()
	}
	flush()
	return tokens
}

// longestToken returns the longest identifier token of text.
func longestToken(text string) string {
	longest := ""
	for _, token := range queryTokens(text) {
		if len([]rune(token)) > len([]rune(longest)) {
			longest = token
		}
	}
	return longest
}

// qualNameVariants are the qualified spellings a dotted or slashed query may
// use. CodeGraph stores methods as "Type::method".
func qualNameVariants(text string) []string {
	variants := []string{text}
	if strings.Contains(text, "::") {
		variants = append(variants, strings.ReplaceAll(text, "::", "."))
	}
	if strings.Contains(text, ".") {
		variants = append(variants, strings.ReplaceAll(text, ".", "::"))
	}
	return uniqueStrings(variants)
}

// escapeLike escapes LIKE wildcards so a query is matched literally.
func escapeLike(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(text)
}

// uniqueStrings drops duplicates while keeping the first occurrence.
func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func clampLimit(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}
