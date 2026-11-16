package codeindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/graphalgo"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Diff compares this native store with another native index file. Both sides
// are read only: the current store is queried through i.db and the base store
// is opened with SQLite mode=ro and query_only.
//
// A diff is a set difference keyed by symbol id, file path and
// (source, target, kind) over the dependency edges, so line shifts and repeated
// call sites do not appear as changes. A base index of another checkout of the
// same repository is the intended input: symbol ids are derived from the
// package path and qualified name, never from a line number.
func (i *Index) Diff(ctx context.Context, project string, opts ports.GraphDiffQuery) (ports.GraphDiffView, error) {
	view := ports.GraphDiffView{
		BaseProject: strings.TrimSpace(opts.BaseProject),
		HeadProject: i.label(project),
		HeadIndex:   i.dbPath,
	}
	if err := i.ensureAnalyticsReadable(ctx); err != nil {
		return view, err
	}
	basePath, err := graphalgo.ValidateIndexPath(i.dbPath, opts.BaseIndexPath)
	if err != nil {
		return view, err
	}
	baseDB, err := openReadOnlyStore(basePath)
	if err != nil {
		return view, err
	}
	defer func() { _ = baseDB.Close() }()
	baseMeta, err := verifyNativeIndex(ctx, basePath, baseDB)
	if err != nil {
		return view, err
	}
	view.BaseIndex = basePath
	headMeta, err := i.readMeta(ctx)
	if err != nil {
		return view, err
	}
	view.BaseCommit = strings.TrimSpace(baseMeta[metaGitSHA])
	view.HeadCommit = strings.TrimSpace(headMeta[metaGitSHA])
	limit := clampLimit(opts.Limit, ports.DefaultGraphDiffSamples, ports.MaxGraphDiffSamples)

	baseFiles, err := queryKeys(ctx, baseDB, `SELECT path FROM files ORDER BY path`)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff base files: %w", err)
	}
	defer baseFiles.Close()
	headFiles, err := queryKeys(ctx, i.db, `SELECT path FROM files ORDER BY path`)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff head files: %w", err)
	}
	defer headFiles.Close()
	files, err := graphalgo.DiffKeys(headFiles, baseFiles, limit)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff files: %w", err)
	}
	view.Files = ports.GraphDiffCount{Added: files.Added, Removed: files.Removed}
	view.AddedFiles, view.RemovedFiles = files.AddedKeys, files.RemovedKeys
	view.Truncated = view.Truncated || files.Truncated

	baseSymbols, err := queryKeys(ctx, baseDB, `SELECT id FROM symbols ORDER BY id`)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff base symbols: %w", err)
	}
	defer baseSymbols.Close()
	headSymbols, err := queryKeys(ctx, i.db, `SELECT id FROM symbols ORDER BY id`)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff head symbols: %w", err)
	}
	defer headSymbols.Close()
	symbols, err := graphalgo.DiffKeys(headSymbols, baseSymbols, limit)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff symbols: %w", err)
	}
	view.Symbols = ports.GraphDiffCount{Added: symbols.Added, Removed: symbols.Removed}
	view.Truncated = view.Truncated || symbols.Truncated
	view.AddedSymbols, err = loadSymbolsFrom(ctx, i.db, symbols.AddedKeys)
	if err != nil {
		return view, err
	}
	view.RemovedSymbols, err = loadSymbolsFrom(ctx, baseDB, symbols.RemovedKeys)
	if err != nil {
		return view, err
	}

	edgeQuery := `SELECT DISTINCT source, target, kind FROM edges
		WHERE kind IN (` + placeholders(len(traversalEdgeKinds)) + `)
		ORDER BY source, target, kind`
	arguments := stringsToAny(traversalEdgeKinds)
	baseEdges, err := queryEdgeKeys(ctx, baseDB, edgeQuery, arguments)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff base edges: %w", err)
	}
	defer baseEdges.Close()
	headEdges, err := queryEdgeKeys(ctx, i.db, edgeQuery, arguments)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff head edges: %w", err)
	}
	defer headEdges.Close()
	edges, err := graphalgo.DiffKeys(headEdges, baseEdges, limit)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff edges: %w", err)
	}
	view.Edges = ports.GraphDiffCount{Added: edges.Added, Removed: edges.Removed}
	view.Truncated = view.Truncated || edges.Truncated
	view.AddedEdges = splitEdgeKeys(edges.AddedKeys)
	view.RemovedEdges = splitEdgeKeys(edges.RemovedKeys)

	baseCommunities, err := queryKeys(ctx, baseDB, `SELECT signature FROM communities ORDER BY signature`)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff base communities: %w", err)
	}
	defer baseCommunities.Close()
	headCommunities, err := queryKeys(ctx, i.db, `SELECT signature FROM communities ORDER BY signature`)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff head communities: %w", err)
	}
	defer headCommunities.Close()
	communities, err := graphalgo.DiffKeys(headCommunities, baseCommunities, limit)
	if err != nil {
		return view, fmt.Errorf("codeindex: diff communities: %w", err)
	}
	view.Communities = ports.GraphDiffCount{Added: communities.Added, Removed: communities.Removed}
	view.AddedCommunities, view.RemovedCommunities = communities.AddedKeys, communities.RemovedKeys
	view.Truncated = view.Truncated || communities.Truncated

	if symbols.Added+symbols.Removed == 0 && edges.Added+edges.Removed == 0 && files.Added+files.Removed == 0 {
		view.Notes = append(view.Notes, "the two indexes hold the same files, symbols and dependency edges")
	}
	if communities.Added+communities.Removed > 0 {
		view.Notes = append(view.Notes, "a community id follows its signature; changed members produce a new signature and a new id on the next rebuild")
	}
	if view.Truncated {
		view.Notes = append(view.Notes, fmt.Sprintf("samples are capped at %d per side per category; counts are exact", limit))
	}
	view.Notes = append(view.Notes, "edges are compared over the dependency kinds only; repeated call sites of one pair are one edge")
	return view, nil
}

// validateBaseIndexPath refuses a base index that is missing, not a regular
// file, a symlink, or the store being queried.
func validateBaseIndexPath(headPath, raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", domain.NewValidationError("graph.base_index", "is required: name the index file of the base project")
	}
	if !filepath.IsAbs(path) {
		return "", domain.NewValidationError("graph.base_index", "must be an absolute path to a base index file")
	}
	path = filepath.Clean(path)
	info, err := os.Lstat(path)
	if err != nil {
		return "", domain.NewValidationError("graph.base_index", fmt.Sprintf("cannot read %s: %v", path, err))
	}
	if !info.Mode().IsRegular() {
		return "", domain.NewValidationError("graph.base_index", fmt.Sprintf("%s is not a regular index file", path))
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", domain.NewValidationError("graph.base_index", "must not be a symbolic link")
	}
	if filepath.Clean(headPath) == path {
		return "", domain.NewValidationError("graph.base_index", "must differ from the index being queried")
	}
	return path, nil
}

// openReadOnlyStore opens a native index without WAL or write pragmas.
func openReadOnlyStore(path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_query_only=1&_pragma=busy_timeout(" + fmt.Sprint(busyTimeoutMS) + ")"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("codeindex: open base index %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	var queryOnly int
	if err := db.QueryRow(`PRAGMA query_only`).Scan(&queryOnly); err != nil || queryOnly != 1 {
		_ = db.Close()
		if err == nil {
			err = errors.New("query_only is not enabled")
		}
		return nil, fmt.Errorf("codeindex: open base index %s read-only: %w", path, err)
	}
	return db, nil
}

// verifyNativeIndex refuses a base file that is not a native index of the
// schema this build reads. It returns its meta rows.
func verifyNativeIndex(ctx context.Context, path string, db queryer) (map[string]string, error) {
	meta, err := readMetaFrom(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not a native graph index: %v", domain.ErrUnsupported, path, err)
	}
	if len(meta) == 0 {
		return nil, fmt.Errorf("%w: %s is not a native graph index", domain.ErrUnsupported, path)
	}
	if engine := strings.TrimSpace(meta[metaEngine]); engine != "" && engine != Engine {
		return nil, fmt.Errorf("%w: %s was written by engine %q", domain.ErrUnsupported, path, engine)
	}
	schema, extractor := metaInt(meta, metaSchema), metaInt(meta, metaExtractor)
	if schema != SchemaVersion || extractor != ExtractorVersion {
		return nil, fmt.Errorf("%w: %s was written with schema %d/extraction %d; this build reads %d/%d",
			domain.ErrUnsupported, path, schema, extractor, SchemaVersion, ExtractorVersion)
	}
	return meta, nil
}

// readMetaFrom reads the meta table from any handle.
func readMetaFrom(ctx context.Context, db queryer) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return nil, fmt.Errorf("codeindex: read base meta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	meta := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("codeindex: read base meta: %w", err)
		}
		meta[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("codeindex: read base meta: %w", err)
	}
	return meta, nil
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

// queryKeys opens one ordered text query and returns its key reader. The
// caller must close the reader through the returned closer.
func queryKeys(ctx context.Context, db queryer, query string, arguments ...any) (*rowsKeyStream, error) {
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	return &rowsKeyStream{rows: rows, reader: &textKeyReader{rows: rows}}, nil
}

// queryEdgeKeys opens one ordered (source, target, kind) query.
func queryEdgeKeys(ctx context.Context, db queryer, query string, arguments []any) (*rowsKeyStream, error) {
	rows, err := db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	return &rowsKeyStream{rows: rows, reader: &edgeKeyReader{rows: rows}}, nil
}

// rowsKeyStream owns the rows behind one key reader and closes them when the
// merge consumes the stream.
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

// Close releases the rows of a stream that was not consumed to the end.
func (s *rowsKeyStream) Close() {
	if s != nil && s.rows != nil {
		_ = s.rows.Close()
	}
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
