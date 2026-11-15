package codegraph

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ErrAmbiguous marks a symbol name that resolves to more than one definition.
// The caller must disambiguate with a file; the adapter never picks the first
// candidate of an ambiguous name.
var ErrAmbiguous = errors.New("codegraph: ambiguous symbol")

// AmbiguousError lists the definitions a name matched, so the caller can name
// the file instead of guessing.
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
	return fmt.Sprintf("codegraph: symbol %q is ambiguous: %s; pass a file to choose one", scope, detail.String())
}

func (e *AmbiguousError) Unwrap() error { return ErrAmbiguous }

// Node returns one definition with optional source and its direct callers and
// callees.
func (i *Index) Node(ctx context.Context, project, symbol, file string, opts ports.NodeOpts) (ports.NodeView, error) {
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

// attachSource renders the declaration with line numbers. Source may be
// missing when the checkout is not present; the graph answer stays useful and
// the omission is stated.
func (i *Index) attachSource(reader *sourceReader, view *ports.NodeView) {
	start := view.Symbol.StartLine
	end := view.Symbol.EndLine
	if end < start {
		end = start
	}
	capped := false
	if end-start+1 > maxNodeSourceLines {
		end = start + maxNodeSourceLines - 1
		view.SourceTruncated = true
		capped = true
	}
	lines, clipped, err := reader.lines(view.Symbol.File, start, end)
	if err != nil {
		view.Notes = append(view.Notes, "source unavailable: "+err.Error())
		return
	}
	view.Source = lines
	if clipped && !view.SourceTruncated {
		view.SourceTruncated = true
	}
	if capped {
		view.Notes = append(view.Notes, fmt.Sprintf("source truncated at %d lines; read the file for the rest", maxNodeSourceLines))
	} else if clipped {
		view.Notes = append(view.Notes, "the file is shorter than the indexed line range; the index may be stale")
	}
}

// resolveSymbol maps a name to exactly one definition. Qualified names win,
// then a unique name, then a name narrowed by file. Anything else is reported
// as ambiguous instead of guessed.
func (i *Index) resolveSymbol(ctx context.Context, symbol, file string) (ports.GraphSymbol, error) {
	name := strings.TrimSpace(symbol)
	if name == "" {
		return ports.GraphSymbol{}, domain.NewValidationError("graph.symbol", "is required")
	}
	file = strings.TrimSpace(file)
	variants := qualNameVariants(name)
	arguments := []any{name, name}
	arguments = append(arguments, stringsToAny(variants)...)
	where := `(id = ? OR lower(name) = lower(?) OR lower(qualified_name) IN (` + placeholders(len(variants)) + `))`
	clause, filterArguments := kindFileClause(nil, file, "")
	arguments = append(arguments, filterArguments...)
	query := `SELECT ` + nodeColumns + ` FROM nodes WHERE ` + where + clause + `
		ORDER BY ` + kindRankFor("") + `, file_path, start_line LIMIT 20`
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return ports.GraphSymbol{}, fmt.Errorf("codegraph: resolve %q: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]ports.GraphSymbol, 0, 8)
	for rows.Next() {
		candidate, err := scanNode(rows)
		if err != nil {
			return ports.GraphSymbol{}, fmt.Errorf("codegraph: scan symbol: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return ports.GraphSymbol{}, fmt.Errorf("codegraph: resolve %q: %w", name, err)
	}
	if len(candidates) == 0 {
		return ports.GraphSymbol{}, fmt.Errorf("%w: symbol %q is not in the index", domain.ErrNotFound, name)
	}
	// A qualified query ("Type::method", "pkg.Symbol") proves identity only
	// when exactly one candidate carries that qualified name. A bare name is
	// never promoted to a qualified hit.
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

// neighbors loads one side of a symbol's edges, collapsing repeated call sites
// into one neighbour with a count.
func (i *Index) neighbors(ctx context.Context, id, side string, limit int) ([]ports.GraphNeighbor, bool, error) {
	position, joined := "e.source", "n.id = e.target"
	if side == "callers" {
		position, joined = "e.target", "n.id = e.source"
	}
	query := `SELECT ` + prefixedNodeColumns("n") + `, e.kind, COALESCE(e.line, 0), COALESCE(e.provenance, ''), e.metadata
		FROM edges e JOIN nodes n ON ` + joined + `
		WHERE ` + position + ` = ? AND e.kind IN (` + placeholders(len(traversalEdgeKinds)) + `)
		ORDER BY n.file_path, n.start_line, e.kind, e.line LIMIT ?`
	arguments := []any{id}
	arguments = append(arguments, stringsToAny(traversalEdgeKinds)...)
	arguments = append(arguments, limit+2)
	rows, err := i.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, false, fmt.Errorf("codegraph: read %s: %w", side, err)
	}
	defer func() { _ = rows.Close() }()
	order := make([]string, 0, limit)
	grouped := map[string]*ports.GraphNeighbor{}
	for rows.Next() {
		var symbol ports.GraphSymbol
		var signature, docstring, visibility sql.NullString
		var exported int
		var edge string
		var line int
		var provenance string
		var metadata sql.NullString
		dest := []any{&symbol.ID, &symbol.Kind, &symbol.Name, &symbol.QualName, &symbol.File,
			&symbol.Language, &symbol.StartLine, &symbol.EndLine, &signature, &docstring, &visibility, &exported,
			&edge, &line, &provenance, &metadata}
		if err := rows.Scan(dest...); err != nil {
			return nil, false, fmt.Errorf("codegraph: scan %s: %w", side, err)
		}
		symbol.Signature = signature.String
		symbol.Docstring = docstring.String
		symbol.Visibility = visibility.String
		symbol.Exported = exported != 0
		confidence, resolvedBy := edgeMetadata(metadata)
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
			Provenance:  provenanceOf(provenance, resolvedBy),
		}
		grouped[key] = neighbor
		order = append(order, key)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("codegraph: read %s: %w", side, err)
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

// prefixedNodeColumns renders the shared node column list with a table alias.
func prefixedNodeColumns(alias string) string {
	columns := strings.Split(nodeColumns, ",")
	for index, column := range columns {
		columns[index] = alias + "." + strings.TrimSpace(column)
	}
	return strings.Join(columns, ", ")
}

// edgeMetadata reads confidence and resolvedBy from an edge metadata object.
func edgeMetadata(raw sql.NullString) (float64, string) {
	confidence := 1.0
	resolvedBy := ""
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return confidence, resolvedBy
	}
	var meta struct {
		Confidence *float64 `json:"confidence"`
		ResolvedBy string   `json:"resolvedBy"`
	}
	if err := json.Unmarshal([]byte(raw.String), &meta); err != nil {
		return confidence, ""
	}
	if meta.Confidence != nil && *meta.Confidence >= 0 && *meta.Confidence <= 1 {
		confidence = *meta.Confidence
	}
	return confidence, meta.ResolvedBy
}

// provenanceOf names where an edge came from. The resolver stores how it bound
// the edge; an edge without either column is structural and reported as such.
func provenanceOf(column, resolvedBy string) string {
	if value := strings.TrimSpace(column); value != "" {
		return value
	}
	if value := strings.TrimSpace(resolvedBy); value != "" {
		return value
	}
	return "index"
}
