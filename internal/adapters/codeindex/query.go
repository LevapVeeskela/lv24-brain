package codeindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// symbolColumns is the column list every symbol query scans.
const symbolColumns = `id, kind, name, qualname, path, start_line, end_line, signature, doc, exported`

// traversalEdgeKinds are the edges that mean "one definition depends on
// another". contains/file ownership does not exist in this store.
var traversalEdgeKinds = []string{"calls", "references", "imports", "implements", "embeds"}

// defaultKinds are the definitions an agent navigates. The native store has no
// file or import nodes, so every indexed kind is a useful answer.
var defaultKinds = []string{"package", "type", "func", "method", "const", "var"}

// ErrAmbiguous marks a symbol name that resolves to more than one definition.
var ErrAmbiguous = errors.New("codeindex: ambiguous symbol")

// AmbiguousError lists the definitions a name matched.
type AmbiguousError struct {
	Symbol     string
	File       string
	Candidates []ports.GraphSymbol
}

func (e *AmbiguousError) Error() string {
	var detail strings.Builder
	for index, candidate := range e.Candidates {
		if index == 5 {
			fmt.Fprintf(&detail, ", and %d more", len(e.Candidates)-index)
			break
		}
		if index > 0 {
			detail.WriteString(", ")
		}
		fmt.Fprintf(&detail, "%s %s (%s:%d)", candidate.Kind, candidate.QualName, candidate.File, candidate.StartLine)
	}
	scope := e.Symbol
	if e.File != "" {
		scope += " in " + e.File
	}
	return fmt.Sprintf("codeindex: symbol %q is ambiguous: %s; pass a file to choose one", scope, detail.String())
}

func (e *AmbiguousError) Unwrap() error { return ErrAmbiguous }

// Search resolves symbols in three tiers: an exact indexed name, full text and
// a substring fallback. Every hit reports the tier and the confidence of that
// tier.
func (i *Index) Search(ctx context.Context, query ports.GraphQuery) ([]ports.SymbolHit, error) {
	text := strings.TrimSpace(query.Text)
	if text == "" {
		return nil, domain.NewValidationError("graph.query", "is required")
	}
	if err := i.ensureReadable(ctx); err != nil {
		return nil, err
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

// searchExact matches an indexed name or qualified name case-insensitively.
func (i *Index) searchExact(ctx context.Context, text, file string, kinds []string, limit int) ([]ports.SymbolHit, error) {
	variants := qualNameVariants(text)
	arguments := []any{text}
	arguments = append(arguments, stringsToAny(lowerAll(variants))...)
	where := `(lower(name) = lower(?) OR lower(qualname) IN (` + placeholders(len(variants)) + `))`
	clause, filterArguments := kindFileClause(kinds, file)
	arguments = append(arguments, filterArguments...)
	arguments = append(arguments, text)
	arguments = append(arguments, limit)
	query := `SELECT ` + symbolColumns + ` FROM symbols WHERE ` + where + clause + `
		ORDER BY CASE WHEN lower(name) = lower(?) THEN 0 ELSE 1 END,
		` + kindRank + `, path, start_line LIMIT ?`
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("codeindex: exact search: %w", err)
	}
	return collectHits(rows, ports.GraphMatchExact, 1.0, func(int) float64 { return 100 })
}

// searchFTS matches the full-text mirror. bm25 weights the name above the
// qualified name, signature and doc.
func (i *Index) searchFTS(ctx context.Context, expression, file string, kinds []string, limit int) ([]ports.SymbolHit, error) {
	clause, filterArguments := kindFileClause(kinds, file)
	arguments := append([]any{expression}, filterArguments...)
	arguments = append(arguments, limit)
	query := `SELECT ` + prefixedSymbolColumns("symbols") + `, bm25(symbols_fts, 1.0, 10.0, 5.0, 1.0) AS rank
		FROM symbols_fts JOIN symbols ON symbols.rowid = symbols_fts.rowid
		WHERE symbols_fts MATCH ?` + clause + `
		ORDER BY rank, path, start_line LIMIT ?`
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("codeindex: full-text search %q: %w", expression, err)
	}
	return collectHits(rows, ports.GraphMatchFTS, 0.8, nil)
}

// searchLike is the substring fallback. It searches the longest token only.
func (i *Index) searchLike(ctx context.Context, text, file string, kinds []string, limit int) ([]ports.SymbolHit, error) {
	token := longestToken(text)
	if len([]rune(token)) < 3 {
		return nil, nil
	}
	pattern := "%" + escapeLike(strings.ToLower(token)) + "%"
	clause, filterArguments := kindFileClause(kinds, file)
	arguments := []any{pattern, pattern}
	arguments = append(arguments, filterArguments...)
	arguments = append(arguments, limit)
	query := `SELECT ` + symbolColumns + ` FROM symbols
		WHERE (lower(name) LIKE ? ESCAPE '\' OR lower(qualname) LIKE ? ESCAPE '\')` + clause + `
		ORDER BY ` + kindRank + `, length(name), path, start_line LIMIT ?`
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("codeindex: substring search: %w", err)
	}
	return collectHits(rows, ports.GraphMatchLike, 0.5, func(index int) float64 { return 10 - float64(index) })
}

// collectHits scans symbol rows from one tier.
func collectHits(rows *sql.Rows, match string, confidence float64, score func(index int) float64) ([]ports.SymbolHit, error) {
	defer func() { _ = rows.Close() }()
	hits := make([]ports.SymbolHit, 0, 16)
	for rows.Next() {
		var hit ports.SymbolHit
		var rank float64
		dest := []any{&hit.ID, &hit.Kind, &hit.Name, &hit.QualName, &hit.File,
			&hit.StartLine, &hit.EndLine, &hit.Signature, &hit.Docstring, &hit.Exported}
		withRank := score == nil
		if withRank {
			dest = append(dest, &rank)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("codeindex: scan symbol: %w", err)
		}
		hit.Language = Language
		hit.Visibility = visibility(hit.Exported)
		hit.Match = match
		hit.Confidence = confidence
		hit.Provenance = "indexed"
		if withRank {
			hit.Score = -rank
		} else {
			hit.Score = score(len(hits))
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// symbolsInFile returns the definitions of one path in source order.
func (i *Index) symbolsInFile(ctx context.Context, path string, kinds []string, limit int) ([]ports.SymbolHit, error) {
	clause, arguments := kindFileClause(kinds, path)
	arguments = append(arguments, limit)
	query := `SELECT ` + symbolColumns + ` FROM symbols WHERE 1 = 1` + clause + `
		ORDER BY start_line, name LIMIT ?`
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("codeindex: list symbols in %s: %w", path, err)
	}
	return collectHits(rows, ports.GraphMatchPath, 1.0, func(int) float64 { return 90 })
}

// kindFileClause builds the shared filters and their arguments.
func kindFileClause(kinds []string, file string) (string, []any) {
	clause := ""
	var arguments []any
	if len(kinds) > 0 {
		clause += ` AND kind IN (` + placeholders(len(kinds)) + `)`
		arguments = append(arguments, stringsToAny(kinds)...)
	}
	if strings.TrimSpace(file) != "" {
		clause += ` AND path = ?`
		arguments = append(arguments, strings.TrimSpace(file))
	}
	return clause, arguments
}

// kindRank orders callables before types and data with the same name.
const kindRank = `CASE kind
	WHEN 'func' THEN 0 WHEN 'method' THEN 0
	WHEN 'type' THEN 1 WHEN 'const' THEN 3 WHEN 'var' THEN 3
	WHEN 'package' THEN 5 ELSE 6 END`

// edgeKindRank orders direct neighbours so a call is listed before a type
// reference at the same distance.
const edgeKindRank = `CASE e.kind
	WHEN 'calls' THEN 0 WHEN 'implements' THEN 1 WHEN 'embeds' THEN 2
	WHEN 'references' THEN 3 WHEN 'imports' THEN 4 ELSE 5 END`

// normalizeKinds resolves an explicit kind filter or the default kinds.
func normalizeKinds(kinds []string) []string {
	if len(kinds) == 0 {
		return defaultKinds
	}
	out := make([]string, 0, len(kinds))
	seen := map[string]bool{}
	for _, kind := range kinds {
		kind = strings.ToLower(strings.TrimSpace(kind))
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		out = append(out, kind)
	}
	if len(out) == 0 {
		return defaultKinds
	}
	return out
}

// Node returns one definition with optional source and direct neighbours.
func (i *Index) Node(ctx context.Context, project, symbol, file string, opts ports.NodeOpts) (ports.NodeView, error) {
	if err := i.ensureReadable(ctx); err != nil {
		return ports.NodeView{}, err
	}
	resolved, err := i.resolveSymbol(ctx, symbol, file)
	if err != nil {
		return ports.NodeView{}, err
	}
	view := ports.NodeView{Symbol: resolved}
	limit := clampLimit(opts.Limit, defaultNeighborLimit, ports.MaxGraphSearchLimit)
	reader, err := i.newSourceReader(ctx, []string{resolved.File})
	if err != nil {
		return ports.NodeView{}, err
	}
	if changed, note := reader.changed(resolved.File); changed {
		view.Notes = append(view.Notes, note)
	}
	if opts.IncludeSource {
		i.attachSource(reader, &view)
	}
	if opts.IncludeCallers {
		callers, truncated, err := i.neighbors(ctx, resolved.ID, "callers", limit)
		if err != nil {
			return ports.NodeView{}, err
		}
		view.Callers = callers
		if truncated {
			view.Notes = append(view.Notes, fmt.Sprintf("callers truncated at %d; raise --limit to see more", limit))
		}
	}
	if opts.IncludeCallees {
		callees, truncated, err := i.neighbors(ctx, resolved.ID, "callees", limit)
		if err != nil {
			return ports.NodeView{}, err
		}
		view.Callees = callees
		if truncated {
			view.Notes = append(view.Notes, fmt.Sprintf("callees truncated at %d; raise --limit to see more", limit))
		}
	}
	return view, nil
}

// attachSource renders the declaration lines with a cap and a stale warning.
func (i *Index) attachSource(reader *sourceReader, view *ports.NodeView) {
	start, end := view.Symbol.StartLine, view.Symbol.EndLine
	if end < start {
		end = start
	}
	capped := false
	if end-start+1 > maxSymbolSourceLines {
		end = start + maxSymbolSourceLines - 1
		view.SourceTruncated = true
		capped = true
	}
	lines, clipped, err := reader.lines(view.Symbol.File, start, end)
	if err != nil {
		view.Notes = append(view.Notes, "source unavailable: "+err.Error())
		return
	}
	view.Source = lines
	if clipped {
		view.SourceTruncated = true
	}
	if capped {
		view.Notes = append(view.Notes, fmt.Sprintf("source truncated at %d lines; read the file for the rest", maxSymbolSourceLines))
	} else if clipped {
		view.Notes = append(view.Notes, "the file is shorter than the indexed range; the index may be stale")
	}
}

// resolveSymbol maps a name to exactly one definition.
func (i *Index) resolveSymbol(ctx context.Context, symbol, file string) (ports.GraphSymbol, error) {
	name := strings.TrimSpace(symbol)
	if name == "" {
		return ports.GraphSymbol{}, domain.NewValidationError("graph.symbol", "is required")
	}
	file = strings.TrimSpace(file)
	variants := qualNameVariants(name)
	arguments := []any{name, name}
	arguments = append(arguments, stringsToAny(lowerAll(variants))...)
	where := `(id = ? OR lower(name) = lower(?) OR lower(qualname) IN (` + placeholders(len(variants)) + `))`
	query := `SELECT ` + symbolColumns + ` FROM symbols WHERE ` + where
	if file != "" {
		query += ` AND path = ?`
		arguments = append(arguments, file)
	}
	query += ` ORDER BY ` + kindRank + `, path, start_line LIMIT 20`
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return ports.GraphSymbol{}, fmt.Errorf("codeindex: resolve %q: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]ports.GraphSymbol, 0, 8)
	for rows.Next() {
		candidate, err := scanSymbol(rows, false)
		if err != nil {
			return ports.GraphSymbol{}, fmt.Errorf("codeindex: scan symbol: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return ports.GraphSymbol{}, fmt.Errorf("codeindex: resolve %q: %w", name, err)
	}
	if len(candidates) == 0 {
		return ports.GraphSymbol{}, fmt.Errorf("%w: symbol %q is not in the index", domain.ErrNotFound, name)
	}
	if strings.ContainsAny(name, ".:/") {
		qualified := make([]ports.GraphSymbol, 0, 2)
		for _, candidate := range candidates {
			for _, variant := range variants {
				if strings.EqualFold(candidate.QualName, variant) {
					qualified = append(qualified, candidate)
					break
				}
			}
		}
		if len(qualified) == 1 {
			return qualified[0], nil
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	return ports.GraphSymbol{}, &AmbiguousError{Symbol: name, File: file, Candidates: candidates}
}

// neighbors loads one side of a symbol's edges, collapsing repeated sites.
func (i *Index) neighbors(ctx context.Context, id, side string, limit int) ([]ports.GraphNeighbor, bool, error) {
	position, joined := "e.source", "s.id = e.target"
	if side == "callers" {
		position, joined = "e.target", "s.id = e.source"
	}
	query := `SELECT ` + prefixedSymbolColumns("s") + `, e.kind, e.line, e.confidence, e.provenance
		FROM edges e JOIN symbols s ON ` + joined + `
		WHERE ` + position + ` = ? AND e.kind IN (` + placeholders(len(traversalEdgeKinds)) + `)
		ORDER BY ` + edgeKindRank + `, s.path, s.start_line, e.line LIMIT ?`
	arguments := []any{id}
	arguments = append(arguments, stringsToAny(traversalEdgeKinds)...)
	arguments = append(arguments, limit+2)
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, false, fmt.Errorf("codeindex: read %s: %w", side, err)
	}
	defer func() { _ = rows.Close() }()
	order := make([]string, 0, limit)
	grouped := map[string]*ports.GraphNeighbor{}
	for rows.Next() {
		var symbol ports.GraphSymbol
		var exported int
		var edge, provenance string
		var line int
		var confidence float64
		if err := rows.Scan(&symbol.ID, &symbol.Kind, &symbol.Name, &symbol.QualName, &symbol.File,
			&symbol.StartLine, &symbol.EndLine, &symbol.Signature, &symbol.Docstring, &exported,
			&edge, &line, &confidence, &provenance); err != nil {
			return nil, false, fmt.Errorf("codeindex: scan %s: %w", side, err)
		}
		symbol.Exported = exported != 0
		symbol.Language = Language
		symbol.Visibility = visibility(symbol.Exported)
		key := symbol.ID + "\x00" + edge
		if existing, ok := grouped[key]; ok {
			existing.Count++
			continue
		}
		neighbor := &ports.GraphNeighbor{
			GraphSymbol: symbol,
			Edge:        edge,
			Line:        line,
			Count:       1,
			Confidence:  confidence,
			Provenance:  provenance,
		}
		grouped[key] = neighbor
		order = append(order, key)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("codeindex: read %s: %w", side, err)
	}
	truncated := len(order) > limit
	if truncated {
		order = order[:limit]
	}
	out := make([]ports.GraphNeighbor, 0, len(order))
	for _, key := range order {
		out = append(out, *grouped[key])
	}
	return out, truncated, nil
}

// scanSymbol reads one symbol row.
func scanSymbol(row interface{ Scan(dest ...any) error }, withRank bool) (ports.GraphSymbol, error) {
	var symbol ports.GraphSymbol
	var exported int
	dest := []any{&symbol.ID, &symbol.Kind, &symbol.Name, &symbol.QualName, &symbol.File,
		&symbol.StartLine, &symbol.EndLine, &symbol.Signature, &symbol.Docstring, &exported}
	var rank float64
	if withRank {
		dest = append(dest, &rank)
	}
	if err := row.Scan(dest...); err != nil {
		return ports.GraphSymbol{}, err
	}
	symbol.Exported = exported != 0
	symbol.Language = Language
	symbol.Visibility = visibility(symbol.Exported)
	return symbol, nil
}

// prefixedSymbolColumns renders the shared column list with a table alias.
func prefixedSymbolColumns(alias string) string {
	columns := strings.Split(symbolColumns, ",")
	for index, column := range columns {
		columns[index] = alias + "." + strings.TrimSpace(column)
	}
	return strings.Join(columns, ", ")
}

func visibility(exported bool) string {
	if exported {
		return "public"
	}
	return "private"
}

// qualNameVariants are the qualified spellings a dotted or slashed query may
// use.
func qualNameVariants(text string) []string {
	variants := []string{text}
	if strings.Contains(text, "::") {
		variants = append(variants, strings.ReplaceAll(text, "::", "."))
	}
	if strings.Contains(text, ".") {
		variants = append(variants, strings.ReplaceAll(text, ".", "::"))
	}
	if strings.Contains(text, "/") {
		variants = append(variants, strings.ReplaceAll(text, "/", "."))
	}
	return uniqueStrings(variants)
}

// ftsExpression turns free text into a safe MATCH expression: quoted prefix
// terms joined with AND.
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

// escapeLike escapes LIKE wildcards so a query is matched literally.
func escapeLike(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(text)
}

// lowerAll lowercases qualified-name variants for a case-insensitive IN list.
func lowerAll(values []string) []string {
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = strings.ToLower(value)
	}
	return out
}
