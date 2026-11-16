package codegraph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/graphalgo"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Diff compares this read-only index with another CodeGraph index of the same
// schema. Both sides are opened read-only; nothing is written to either index.
//
// A symbol id is stable in a CodeGraph index and a file path is stable across
// line shifts, so the diff is a set difference keyed by node id, file path and
// (source, target, kind) over the dependency edges. Two checkouts of the same
// repository at different commits are the intended input.
func (i *Index) Diff(ctx context.Context, project string, opts ports.GraphDiffQuery) (ports.GraphDiffView, error) {
	view := ports.GraphDiffView{
		BaseProject: strings.TrimSpace(opts.BaseProject),
		HeadProject: i.label(project),
		HeadIndex:   i.dbPath,
		HeadCommit:  i.info.Commit,
	}
	basePath, err := graphalgo.ValidateIndexPath(i.dbPath, opts.BaseIndexPath)
	if err != nil {
		return view, err
	}
	base, err := Open(ctx, Options{
		DBPath: basePath, Root: i.root,
		Project: view.BaseProject, Now: i.now,
	})
	if err != nil {
		return view, err
	}
	defer func() { _ = base.Close() }()
	view.BaseIndex = basePath
	view.BaseCommit = base.info.Commit
	if view.BaseProject == "" {
		view.BaseProject = base.project
	}
	limit := clampLimit(opts.Limit, ports.DefaultGraphDiffSamples, ports.MaxGraphDiffSamples)

	baseFiles, err := queryTextKeys(ctx, base.db, `SELECT path FROM files ORDER BY path`)
	if err != nil {
		return view, fmt.Errorf("codegraph: diff base files: %w", err)
	}
	defer baseFiles.Close()
	headFiles, err := queryTextKeys(ctx, i.db, `SELECT path FROM files ORDER BY path`)
	if err != nil {
		return view, fmt.Errorf("codegraph: diff head files: %w", err)
	}
	defer headFiles.Close()
	files, err := graphalgo.DiffKeys(headFiles, baseFiles, limit)
	if err != nil {
		return view, fmt.Errorf("codegraph: diff files: %w", err)
	}
	view.Files = ports.GraphDiffCount{Added: files.Added, Removed: files.Removed}
	view.AddedFiles, view.RemovedFiles = files.AddedKeys, files.RemovedKeys
	view.Truncated = view.Truncated || files.Truncated

	baseNodes, err := queryTextKeys(ctx, base.db, `SELECT id FROM nodes ORDER BY id`)
	if err != nil {
		return view, fmt.Errorf("codegraph: diff base symbols: %w", err)
	}
	defer baseNodes.Close()
	headNodes, err := queryTextKeys(ctx, i.db, `SELECT id FROM nodes ORDER BY id`)
	if err != nil {
		return view, fmt.Errorf("codegraph: diff head symbols: %w", err)
	}
	defer headNodes.Close()
	symbols, err := graphalgo.DiffKeys(headNodes, baseNodes, limit)
	if err != nil {
		return view, fmt.Errorf("codegraph: diff symbols: %w", err)
	}
	view.Symbols = ports.GraphDiffCount{Added: symbols.Added, Removed: symbols.Removed}
	view.Truncated = view.Truncated || symbols.Truncated
	view.AddedSymbols, err = i.loadSymbols(ctx, symbols.AddedKeys)
	if err != nil {
		return view, err
	}
	view.RemovedSymbols, err = base.loadSymbols(ctx, symbols.RemovedKeys)
	if err != nil {
		return view, err
	}

	edgeQuery := `SELECT DISTINCT source, target, kind FROM edges
		WHERE kind IN (` + placeholders(len(traversalEdgeKinds)) + `)
		ORDER BY source, target, kind`
	arguments := stringsToAny(traversalEdgeKinds)
	baseEdges, err := queryEdgeKeys(ctx, base.db, edgeQuery, arguments)
	if err != nil {
		return view, fmt.Errorf("codegraph: diff base edges: %w", err)
	}
	defer baseEdges.Close()
	headEdges, err := queryEdgeKeys(ctx, i.db, edgeQuery, arguments)
	if err != nil {
		return view, fmt.Errorf("codegraph: diff head edges: %w", err)
	}
	defer headEdges.Close()
	edges, err := graphalgo.DiffKeys(headEdges, baseEdges, limit)
	if err != nil {
		return view, fmt.Errorf("codegraph: diff edges: %w", err)
	}
	view.Edges = ports.GraphDiffCount{Added: edges.Added, Removed: edges.Removed}
	view.Truncated = view.Truncated || edges.Truncated
	view.AddedEdges = splitEdgeKeys(edges.AddedKeys)
	view.RemovedEdges = splitEdgeKeys(edges.RemovedKeys)

	view.Notes = append(view.Notes, "communities are computed in memory and are not stored by a codegraph index; the diff compares files, nodes and dependency edges")
	if view.Symbols.Added+view.Symbols.Removed == 0 && view.Edges.Added+view.Edges.Removed == 0 && view.Files.Added+view.Files.Removed == 0 {
		view.Notes = append(view.Notes, "the two indexes hold the same files, nodes and dependency edges")
	}
	if view.Truncated {
		view.Notes = append(view.Notes, fmt.Sprintf("samples are capped at %d per side per category; counts are exact", limit))
	}
	return view, nil
}

// textKeyReader streams one text column as an ordered key.
type textKeyReader struct {
	rows *sql.Rows
}

func (r *textKeyReader) Next() (string, error) {
	if !r.rows.Next() {
		return "", readStreamEnd(r.rows)
	}
	var value string
	if err := r.rows.Scan(&value); err != nil {
		return "", err
	}
	return value, nil
}

// edgeKeyReader streams (source, target, kind) rows as one NUL-separated key.
type edgeKeyReader struct {
	rows *sql.Rows
}

func (r *edgeKeyReader) Next() (string, error) {
	if !r.rows.Next() {
		return "", readStreamEnd(r.rows)
	}
	var source, target, kind string
	if err := r.rows.Scan(&source, &target, &kind); err != nil {
		return "", err
	}
	return source + "\x00" + target + "\x00" + kind, nil
}

// readStreamEnd distinguishes the end of a stream from a read failure.
func readStreamEnd(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		return err
	}
	return io.EOF
}

// rowsKeyStream owns the rows behind one key reader.
type rowsKeyStream struct {
	rows   *sql.Rows
	reader graphalgo.KeyReader
}

func (s *rowsKeyStream) Next() (string, error) {
	key, err := s.reader.Next()
	if errors.Is(err, io.EOF) {
		_ = s.rows.Close()
	}
	return key, err
}

// Close releases rows that were not consumed to the end.
func (s *rowsKeyStream) Close() {
	if s != nil && s.rows != nil {
		_ = s.rows.Close()
	}
}

// queryTextKeys opens one ordered single-column query.
func queryTextKeys(ctx context.Context, db *sql.DB, query string, arguments ...any) (*rowsKeyStream, error) {
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	return &rowsKeyStream{rows: rows, reader: &textKeyReader{rows: rows}}, nil
}

// queryEdgeKeys opens one ordered (source, target, kind) query.
func queryEdgeKeys(ctx context.Context, db *sql.DB, query string, arguments []any) (*rowsKeyStream, error) {
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	return &rowsKeyStream{rows: rows, reader: &edgeKeyReader{rows: rows}}, nil
}

// splitEdgeKeys turns NUL-separated edge keys back into DTOs.
func splitEdgeKeys(keys []string) []ports.GraphDiffEdge {
	out := make([]ports.GraphDiffEdge, 0, len(keys))
	for _, key := range keys {
		parts := strings.SplitN(key, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		out = append(out, ports.GraphDiffEdge{From: parts[0], To: parts[1], Kind: parts[2]})
	}
	return out
}
