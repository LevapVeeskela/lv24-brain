// Package codegraph reads an external CodeGraph index in place.
//
// The adapter is deliberately read-only. It opens <checkout>/.codegraph/
// codegraph.db with SQLite's read-only and query-only modes, verifies the
// schema and extraction versions it was written for, maps nodes and edges into
// the ports.GraphIndex DTOs, and reads source text from the checkout at query
// time. Nothing is ever copied into the brain store and nothing is written back
// into the index: the CodeGraph CLI owns indexing, this package only reads.
//
// Version gate: schema 11 and extraction 27 (CodeGraph 1.6.x). A different
// schema or extraction version is refused with a *SchemaError, because the node
// and edge columns this adapter reads are not guaranteed to exist. A newer
// CodeGraph version with the same schema is accepted and reported as a note.
package codegraph

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"modernc.org/sqlite"
)

const (
	// Engine is the engine name this adapter reports.
	Engine = ports.GraphEngineCodeGraph

	// SupportedSchemaVersion is the CodeGraph schema this adapter reads.
	SupportedSchemaVersion = 11
	// SupportedExtractionVersion is the extraction version that fills it.
	SupportedExtractionVersion = 27
	// MinIndexedWithVersion is the oldest CodeGraph release whose index this
	// adapter understands.
	MinIndexedWithVersion = "1.6.2"
	// VerifiedIndexedWithVersion is the release the adapter was measured
	// against. A newer release with the same schema is accepted with a note.
	VerifiedIndexedWithVersion = "1.6.2"

	// DefaultDBRelPath is the index path relative to a checkout root.
	DefaultDBRelPath = ".codegraph/codegraph.db"

	busyTimeoutMS = 2000

	// defaultNeighborLimit bounds the caller and callee lists of one node.
	defaultNeighborLimit = 20
	// maxNodeSourceLines bounds how many declaration lines Node returns.
	maxNodeSourceLines = 400
	// maxSourceFileBytes refuses to render a file larger than this.
	maxSourceFileBytes = 8 << 20
)

// traversalEdgeKinds are the edges that mean "one definition depends on
// another". contains/file ownership is deliberately excluded: it would make
// every symbol in a file look like a dependent of every other one.
var traversalEdgeKinds = []string{
	"calls", "references", "instantiates", "extends", "implements",
	"imports", "navigates", "overrides", "decorates",
}

// Options configures one read-only index.
type Options struct {
	// DBPath is the codegraph.db file. It is required.
	DBPath string
	// Root is the checkout the index was built from. Source text is read
	// from here. It is required for Node and Explore.
	Root string
	// Project labels the answers and is reported in Status.
	Project string
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Index is a read-only CodeGraph index bound to one checkout.
type Index struct {
	db       *sql.DB
	dbPath   string
	root     string
	project  string
	readMode string
	notes    []string
	info     schemaInfo
	now      func() time.Time
}

var _ ports.GraphIndex = (*Index)(nil)

// schemaInfo is the version and state metadata read at open time.
type schemaInfo struct {
	SchemaVersion     int
	ExtractionVersion int
	IndexedWith       string
	IndexState        string
	Commit            string
	Dirty             bool
	DiscoveredFiles   int
	Notes             []string
}

// SchemaError reports an index this adapter cannot read. It unwraps to
// domain.ErrUnsupported so a caller can present it as "this engine cannot read
// that index" instead of a generic failure.
type SchemaError struct {
	DBPath      string
	Schema      int
	Extraction  int
	IndexedWith string
	Reason      string
}

func (e *SchemaError) Error() string {
	detail := e.Reason
	if detail == "" {
		detail = fmt.Sprintf("schema %d, extraction %d, written by codegraph %q", e.Schema, e.Extraction, e.IndexedWith)
	}
	return fmt.Sprintf("codegraph: refusing index %s: %s (this adapter reads schema %d, extraction %d, codegraph >= %s)",
		e.DBPath, detail, SupportedSchemaVersion, SupportedExtractionVersion, MinIndexedWithVersion)
}

func (e *SchemaError) Unwrap() error { return domain.ErrUnsupported }

// Open opens and verifies the index at opts.DBPath. The database is opened
// read-only with PRAGMA query_only, so no statement can write to it.
func Open(ctx context.Context, opts Options) (*Index, error) {
	path := strings.TrimSpace(opts.DBPath)
	if path == "" {
		return nil, domain.NewValidationError("graph.path", "is required")
	}
	root := strings.TrimSpace(opts.Root)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("codegraph: index %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, domain.NewValidationError("graph.path",
			fmt.Sprintf("%q is a directory; point at the codegraph.db file inside it", path))
	}
	db, mode, err := openReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	index := &Index{
		db:       db,
		dbPath:   path,
		root:     filepath.Clean(root),
		project:  strings.TrimSpace(opts.Project),
		readMode: mode,
		now:      opts.Now,
	}
	if index.now == nil {
		index.now = time.Now
	}
	if err := index.verify(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return index, nil
}

// Close releases the database handle.
func (i *Index) Close() error {
	if i == nil || i.db == nil {
		return nil
	}
	return i.db.Close()
}

// DBPath is the index file this adapter reads.
func (i *Index) DBPath() string { return i.dbPath }

// Root is the checkout source text is read from.
func (i *Index) Root() string { return i.root }

// openReadOnly opens path twice at most: first with SQLite's read-only mode,
// then, when a live writer holds the WAL and the read-only open reports BUSY,
// with immutable=1. Immutable reads skip locking and the WAL; it is a
// last-resort fallback that is disclosed as a note.
func openReadOnly(ctx context.Context, path string) (*sql.DB, string, error) {
	db, err := openDSN(path, "mode=ro&_query_only=1&_pragma=busy_timeout("+strconv.Itoa(busyTimeoutMS)+")")
	if err == nil {
		if probeErr := probe(ctx, db); probeErr == nil {
			return db, "read-only", nil
		} else if !isBusyError(probeErr) {
			_ = db.Close()
			return nil, "", fmt.Errorf("codegraph: open %s: %w", path, probeErr)
		}
		_ = db.Close()
	}
	if !isBusyError(err) && err != nil {
		return nil, "", fmt.Errorf("codegraph: open %s: %w", path, err)
	}
	db, err = openDSN(path, "immutable=1&_query_only=1")
	if err != nil {
		return nil, "", fmt.Errorf("codegraph: open %s: %w", path, err)
	}
	if probeErr := probe(ctx, db); probeErr != nil {
		_ = db.Close()
		return nil, "", fmt.Errorf("codegraph: open %s read-only: %w", path, probeErr)
	}
	return db, "immutable", nil
}

func openDSN(path, params string) (*sql.DB, error) {
	connector, err := sqlite.NewConnector("file:" + filepath.ToSlash(path) + "?" + params)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	return db, nil
}

// probe forces one read and confirms the query-only guard is in effect.
func probe(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&version); err != nil {
		return err
	}
	var queryOnly int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil {
		return err
	}
	if queryOnly != 1 {
		return errors.New("query_only is not enabled")
	}
	return nil
}

func isBusyError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
		return true
	default:
		return false
	}
}

// verify checks the required tables and refuses an index this adapter does not
// understand.
func (i *Index) verify(ctx context.Context) error {
	var tables int
	err := i.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN
		('nodes', 'edges', 'files', 'nodes_fts', 'project_metadata', 'schema_versions')`).Scan(&tables)
	if err != nil {
		return fmt.Errorf("codegraph: read schema of %s: %w", i.dbPath, err)
	}
	if tables < 6 {
		return &SchemaError{DBPath: i.dbPath, Reason: "not a CodeGraph index: nodes, edges, files, nodes_fts, project_metadata or schema_versions is missing"}
	}
	var schema int
	if err := i.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_versions`).Scan(&schema); err != nil {
		return fmt.Errorf("codegraph: read schema version: %w", err)
	}
	meta, err := i.readMetadata(ctx)
	if err != nil {
		return err
	}
	info := schemaInfo{
		SchemaVersion:     schema,
		ExtractionVersion: atoi(meta["indexed_with_extraction_version"]),
		IndexedWith:       strings.TrimSpace(meta["indexed_with_version"]),
		IndexState:        strings.TrimSpace(meta["index_state"]),
		DiscoveredFiles:   atoi(meta["index_files_discovered"]),
	}
	if info.IndexedWith == "" {
		return &SchemaError{DBPath: i.dbPath, Schema: schema, Extraction: info.ExtractionVersion,
			Reason: "not a CodeGraph index: project_metadata.indexed_with_version is missing"}
	}
	var dirty struct {
		Commit string   `json:"commit"`
		Paths  []string `json:"paths"`
	}
	if raw := strings.TrimSpace(meta["indexed_dirty_paths"]); raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &dirty); err == nil {
			info.Commit = dirty.Commit
			info.Dirty = len(dirty.Paths) > 0
		}
	}
	if info.Commit == "" {
		info.Commit = strings.TrimSpace(meta["indexed_at_commit"])
	}
	if schema != SupportedSchemaVersion || info.ExtractionVersion != SupportedExtractionVersion {
		return &SchemaError{DBPath: i.dbPath, Schema: schema, Extraction: info.ExtractionVersion, IndexedWith: info.IndexedWith}
	}
	if compareVersions(info.IndexedWith, MinIndexedWithVersion) < 0 {
		return &SchemaError{DBPath: i.dbPath, Schema: schema, Extraction: info.ExtractionVersion, IndexedWith: info.IndexedWith,
			Reason: fmt.Sprintf("codegraph %s is older than the supported minimum %s", info.IndexedWith, MinIndexedWithVersion)}
	}
	i.info = info
	if i.readMode == "immutable" {
		i.notes = append(i.notes, "opened immutable without WAL replay because a writer held the index; results reflect the last checkpoint")
	}
	if compareVersions(info.IndexedWith, VerifiedIndexedWithVersion) > 0 {
		i.notes = append(i.notes, fmt.Sprintf("index was written by codegraph %s; this adapter was verified against %s with the same schema", info.IndexedWith, VerifiedIndexedWithVersion))
	}
	if info.IndexState != "" && info.IndexState != "complete" {
		i.notes = append(i.notes, fmt.Sprintf("index_state is %q: the index may be incomplete", info.IndexState))
	}
	return nil
}

func (i *Index) readMetadata(ctx context.Context) (map[string]string, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT key, value FROM project_metadata`)
	if err != nil {
		return nil, fmt.Errorf("codegraph: read project_metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	meta := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("codegraph: read project_metadata: %w", err)
		}
		meta[key] = value
	}
	return meta, rows.Err()
}

// Status reports the index identity, size, counts and age.
func (i *Index) Status(ctx context.Context, project string) (ports.GraphStatus, error) {
	status := ports.GraphStatus{
		Engine:             Engine,
		Project:            i.label(project),
		Root:               i.root,
		DBPath:             i.dbPath,
		SchemaVersion:      i.info.SchemaVersion,
		ExtractionVersion:  i.info.ExtractionVersion,
		IndexedWithVersion: i.info.IndexedWith,
		IndexState:         i.info.IndexState,
		Commit:             i.info.Commit,
		Dirty:              i.info.Dirty,
		Notes:              append([]string(nil), i.notes...),
	}
	if info, err := os.Stat(i.dbPath); err == nil {
		status.SizeBytes = info.Size()
	}
	var indexedAt float64
	err := i.db.QueryRowContext(ctx,
		`SELECT CAST(COALESCE(MAX(indexed_at), 0) AS REAL) FROM files`).Scan(&indexedAt)
	if err != nil {
		return ports.GraphStatus{}, fmt.Errorf("codegraph: read index time: %w", err)
	}
	if indexedAt > 0 {
		status.IndexedAt = time.UnixMilli(int64(indexedAt)).UTC()
		age := i.now().Sub(status.IndexedAt)
		if age > 0 {
			status.AgeSeconds = int64(age.Seconds())
		}
	}
	counts := []struct {
		query string
		field *int
	}{
		{`SELECT count(*) FROM files`, &status.Files},
		{`SELECT count(*) FROM nodes`, &status.Nodes},
		{`SELECT count(*) FROM edges`, &status.Edges},
		{`SELECT count(*) FROM unresolved_refs`, &status.UnresolvedRefs},
	}
	for _, count := range counts {
		if err := i.db.QueryRowContext(ctx, count.query).Scan(count.field); err != nil {
			return ports.GraphStatus{}, fmt.Errorf("codegraph: count rows: %w", err)
		}
	}
	if status.NodesByKind, err = i.countBy(ctx, `SELECT kind, count(*) FROM nodes GROUP BY kind ORDER BY count(*) DESC, kind LIMIT 16`); err != nil {
		return ports.GraphStatus{}, err
	}
	if status.FilesByLanguage, err = i.countBy(ctx, `SELECT language, count(*) FROM files GROUP BY language ORDER BY count(*) DESC, language LIMIT 16`); err != nil {
		return ports.GraphStatus{}, err
	}
	if i.info.DiscoveredFiles > 0 && i.info.DiscoveredFiles != status.Files {
		status.Notes = append(status.Notes, fmt.Sprintf(
			"project_metadata counted %d discovered files but the files table holds %d; treat the table as authoritative",
			i.info.DiscoveredFiles, status.Files))
	}
	return status, nil
}

func (i *Index) countBy(ctx context.Context, query string) (map[string]int, error) {
	rows, err := i.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("codegraph: count by kind: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, fmt.Errorf("codegraph: count by kind: %w", err)
		}
		out[name] = count
	}
	return out, rows.Err()
}

// Sync reports that this adapter cannot index. The CodeGraph CLI owns writes;
// running it here would violate the read-only contract.
func (i *Index) Sync(ctx context.Context, project string, paths []string) (ports.GraphSyncResult, error) {
	note := "the codegraph index is opened read-only; run `codegraph sync` in the checkout and query again"
	if i.root != "" {
		note = "the codegraph index is opened read-only; run `codegraph sync` in " + i.root + " and query again"
	}
	if len(paths) > 0 {
		note += fmt.Sprintf(" (requested paths: %s)", strings.Join(paths, ", "))
	}
	return ports.GraphSyncResult{
		Engine:      Engine,
		Project:     i.label(project),
		Unsupported: true,
		Notes:       []string{note},
	}, nil
}

// label resolves the project label of an answer.
func (i *Index) label(project string) string {
	if strings.TrimSpace(project) == "" {
		return i.project
	}
	return project
}

// sqliteTableColumns is the node column list every query scans.
const nodeColumns = `id, kind, name, qualified_name, file_path, language, start_line, end_line, signature, docstring, visibility, is_exported`

// rowScanner is implemented by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanNode reads one node row.
func scanNode(row rowScanner) (ports.GraphSymbol, error) {
	var symbol ports.GraphSymbol
	var signature, docstring, visibility sql.NullString
	var exported int
	err := row.Scan(&symbol.ID, &symbol.Kind, &symbol.Name, &symbol.QualName, &symbol.File,
		&symbol.Language, &symbol.StartLine, &symbol.EndLine, &signature, &docstring, &visibility, &exported)
	if err != nil {
		return ports.GraphSymbol{}, err
	}
	symbol.Signature = signature.String
	symbol.Docstring = docstring.String
	symbol.Visibility = visibility.String
	symbol.Exported = exported != 0
	return symbol, nil
}

// normalizeKinds resolves an explicit kind filter or the default symbol kinds.
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

// defaultKinds are the definitions an agent navigates. Import and file nodes
// are excluded unless a caller names them: they are the noisiest rows in the
// index and rarely answer "where is this defined".
var defaultKinds = []string{
	"function", "method", "class", "struct", "interface", "trait", "protocol",
	"enum", "enum_member", "type_alias", "constant", "variable", "field",
	"property", "route", "component", "union", "namespace", "module", "export",
}

// kindRankFor orders search hits so a callable definition comes before a field
// or a data constant with the same name.
func kindRankFor(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return `CASE ` + prefix + `kind
	WHEN 'function' THEN 0 WHEN 'method' THEN 0
	WHEN 'class' THEN 1 WHEN 'struct' THEN 1 WHEN 'interface' THEN 1
	WHEN 'trait' THEN 1 WHEN 'protocol' THEN 1 WHEN 'enum' THEN 1
	WHEN 'type_alias' THEN 1 WHEN 'union' THEN 1
	WHEN 'route' THEN 2 WHEN 'component' THEN 2
	WHEN 'constant' THEN 3 WHEN 'variable' THEN 3
	WHEN 'field' THEN 4 WHEN 'property' THEN 4
	ELSE 5 END`
}

// placeholders renders n comma-separated SQL placeholders.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// stringsToAny converts a string list for variadic query arguments.
func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for index, value := range values {
		out[index] = value
	}
	return out
}

// atoi parses a decimal integer, returning zero on anything else.
func atoi(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return parsed
}

// compareVersions compares dotted numeric versions. A missing or malformed
// segment is zero, so "1.6" < "1.6.1".
func compareVersions(left, right string) int {
	parse := func(value string) []int {
		parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(value), "v"), ".")
		out := make([]int, 0, len(parts))
		for _, part := range parts {
			end := 0
			for end < len(part) && part[end] >= '0' && part[end] <= '9' {
				end++
			}
			out = append(out, atoi(part[:end]))
		}
		return out
	}
	a, b := parse(left), parse(right)
	for index := 0; index < len(a) || index < len(b); index++ {
		var av, bv int
		if index < len(a) {
			av = a[index]
		}
		if index < len(b) {
			bv = b[index]
		}
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}
