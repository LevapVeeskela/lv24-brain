// Package sqliteindex implements the brain full-text index with SQLite FTS5.
//
// The index is a derived projection: Markdown files remain the source of truth
// in local and git modes. Search scores come from FTS5 bm25 with the sign
// flipped, so a higher Score means a better match. Results are ordered by
// score, then newest update, then document id, which makes repeated queries
// deterministic.
package sqliteindex

import (
	"context"
	"database/sql"
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
	// DefaultLimit is used when a query does not set a limit.
	DefaultLimit = 20
	// MaxLimit bounds how many hits one query may return.
	MaxLimit = 200

	busyTimeout      = 5 * time.Second
	snippetStart     = "["
	snippetEnd       = "]"
	snippetEllipsis  = "…"
	snippetTokenSpan = 16

	// retryInitialDelay and retryMaxDelay pace the retries of the connection
	// pragmas and the schema bootstrap while another process holds a lock.
	retryInitialDelay = 10 * time.Millisecond
	retryMaxDelay     = 250 * time.Millisecond

	// SQLite primary result codes for a temporarily locked database. Extended
	// codes keep the primary code in the low byte.
	sqliteBusy   = 5
	sqliteLocked = 6
)

// Index stores documents in SQLite with an FTS5 mirror for search.
type Index struct {
	db *sql.DB
	// needsReindex is set when Open replaced an index with another schema
	// version; the caller must reindex from the Markdown sources.
	needsReindex bool
}

var _ ports.Index = (*Index)(nil)

// Open opens or creates the index at path. The parent directory is created when
// missing. Use ":memory:" for an ephemeral index.
func Open(path string) (*Index, error) {
	if strings.TrimSpace(path) == "" {
		return nil, domain.NewValidationError("index.path", "is required")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("sqliteindex: create directory: %w", err)
		}
	}
	connector, err := sqlite.NewConnector(dataSourceName(path))
	if err != nil {
		return nil, fmt.Errorf("sqliteindex: open %q: %w", path, err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	index := &Index{db: db}
	ctx := context.Background()
	// WAL and synchronous are per-connection settings, but the journal-mode
	// switch takes an exclusive lock that busy_timeout does not cover. Setting
	// them after opening lets the switch be retried on SQLITE_BUSY.
	if err := index.configure(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := index.prepareSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return index, nil
}

// dataSourceName builds the modernc.org/sqlite DSN. Only the busy timeout is a
// connection pragma here: journal_mode and synchronous are applied by
// configure, because the WAL switch on a brand-new database can fail with
// SQLITE_BUSY before the busy handler is consulted.
func dataSourceName(path string) string {
	pragmas := "?_pragma=busy_timeout(" + strconv.FormatInt(busyTimeout.Milliseconds(), 10) + ")"
	if path == ":memory:" {
		return "file::memory:" + pragmas
	}
	return "file:" + filepath.ToSlash(path) + pragmas
}

// configure enables WAL and NORMAL synchronous mode, retrying the journal-mode
// switch while another process holds the lock. An in-memory database reports
// "memory" and is accepted as already configured.
func (i *Index) configure(ctx context.Context) error {
	err := retryOnBusy(ctx, busyTimeout, func() error {
		var mode string
		if err := i.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
			return err
		}
		switch strings.ToLower(mode) {
		case "wal", "memory":
			return nil
		default:
			return fmt.Errorf("sqliteindex: journal mode = %q, want wal", mode)
		}
	})
	if err != nil {
		return fmt.Errorf("sqliteindex: set journal mode: %w", err)
	}
	if err := retryOnBusy(ctx, busyTimeout, func() error {
		_, err := i.db.ExecContext(ctx, "PRAGMA synchronous=NORMAL")
		return err
	}); err != nil {
		return fmt.Errorf("sqliteindex: set synchronous mode: %w", err)
	}
	return nil
}

// retryOnBusy runs operation until it succeeds, fails with something other than
// SQLITE_BUSY/SQLITE_LOCKED, exhausts the time budget, or ctx ends. The delay
// starts at retryInitialDelay and doubles up to retryMaxDelay.
func retryOnBusy(ctx context.Context, timeout time.Duration, operation func() error) error {
	deadline := time.Now().Add(timeout)
	delay := retryInitialDelay
	for {
		err := operation()
		if err == nil || !isBusyError(err) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return err
		}
		wait := delay
		if wait > remaining {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay *= 2
		if delay > retryMaxDelay {
			delay = retryMaxDelay
		}
	}
}

// isBusyError reports whether err is SQLITE_BUSY or SQLITE_LOCKED, including
// their extended codes.
func isBusyError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case sqliteBusy, sqliteLocked:
		return true
	default:
		return false
	}
}

// prepareSchema creates the schema, retrying while another process is still
// bootstrapping the same new database.
func (i *Index) prepareSchema(ctx context.Context) error {
	return retryOnBusy(ctx, busyTimeout, func() error {
		return i.prepareSchemaOnce(ctx)
	})
}

// prepareSchemaOnce creates the schema or, when the stored schemaVersion
// differs, drops the derived tables and recreates them. The whole bootstrap is
// one IMMEDIATE transaction so concurrent first opens cannot both decide to
// rebuild and wipe rows inserted by the other process. The version is stored
// last, so an interrupted rebuild is retried on the next open.
func (i *Index) prepareSchemaOnce(ctx context.Context) error {
	conn, err := i.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqliteindex: acquire schema connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("sqliteindex: begin schema transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("sqliteindex: read schema version: %w", err)
	}
	if version == schemaVersion {
		if err := applySchema(ctx, conn); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("sqliteindex: commit schema transaction: %w", err)
		}
		committed = true
		return nil
	}
	for _, statement := range dropStatements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("sqliteindex: drop old schema: %w", err)
		}
	}
	if err := applySchema(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("sqliteindex: write schema version: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("sqliteindex: commit schema transaction: %w", err)
	}
	committed = true
	i.needsReindex = true
	return nil
}

// NeedsReindex reports whether Open recreated an index whose schema version
// differed, so it is empty and must be refilled by a reindex.
func (i *Index) NeedsReindex() bool {
	return i.needsReindex
}

type schemaExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func applySchema(ctx context.Context, db schemaExecer) error {
	for _, statement := range schemaStatements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("sqliteindex: apply schema: %w", err)
		}
	}
	return nil
}

// Close releases the database handle.
func (i *Index) Close() error {
	return i.db.Close()
}

// Upsert inserts or replaces one indexed document.
func (i *Index) Upsert(ctx context.Context, doc ports.Document) error {
	if err := validateDocument(doc); err != nil {
		return err
	}
	if _, err := i.db.ExecContext(ctx, upsertSQL,
		doc.ID,
		doc.ProjectID,
		doc.Kind,
		doc.Title,
		doc.Body,
		doc.UpdatedAt.UnixNano(),
	); err != nil {
		return fmt.Errorf("sqliteindex: upsert %q: %w", doc.ID, err)
	}
	return nil
}

func validateDocument(doc ports.Document) error {
	if err := domain.ValidateRequired("index.document.id", doc.ID); err != nil {
		return err
	}
	return domain.ValidateRequired("index.document.projectId", doc.ProjectID)
}

// IDs returns the id of every indexed document (for pruning after a reindex).
func (i *Index) IDs(ctx context.Context) ([]string, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT id FROM documents`)
	if err != nil {
		return nil, fmt.Errorf("sqliteindex: list ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("sqliteindex: list ids: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Delete removes a document from the index. Unknown ids are ignored.
func (i *Index) Delete(ctx context.Context, id string) error {
	if err := domain.ValidateRequired("index.document.id", id); err != nil {
		return err
	}
	if _, err := i.db.ExecContext(ctx, deleteSQL, id); err != nil {
		return fmt.Errorf("sqliteindex: delete %q: %w", id, err)
	}
	return nil
}

// Search returns matching documents ordered by relevance, recency and id.
// Every term must match (AND). When that finds nothing and the query has two or
// more terms, the same terms are retried with OR. Filters apply to both passes.
func (i *Index) Search(ctx context.Context, query ports.Query) ([]ports.Hit, error) {
	match, ok := matchExpression(query.Text, joinAll)
	if !ok {
		return []ports.Hit{}, nil
	}
	limit := normalizeLimit(query.Limit)
	hits, err := i.runSearch(ctx, query, match, limit)
	if err != nil || len(hits) > 0 || termCount(query.Text) < 2 {
		return hits, err
	}
	anyMatch, _ := matchExpression(query.Text, joinAny)
	return i.runSearch(ctx, query, anyMatch, limit)
}

// runSearch executes one FTS5 MATCH expression with the query filters applied.
func (i *Index) runSearch(ctx context.Context, query ports.Query, match string, limit int) ([]ports.Hit, error) {
	statement, args := buildSearchStatement(query, match, limit)
	rows, err := i.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("sqliteindex: search: %w", err)
	}
	defer rows.Close()

	hits := make([]ports.Hit, 0, limit)
	for rows.Next() {
		var (
			hit     ports.Hit
			snippet sql.NullString
			rank    float64
		)
		if err := rows.Scan(&hit.ID, &hit.ProjectID, &hit.Kind, &hit.Title, &snippet, &rank); err != nil {
			return nil, fmt.Errorf("sqliteindex: scan hit: %w", err)
		}
		hit.ID = recordID(hit.ProjectID, hit.ID)
		hit.Snippet = snippet.String
		hit.Score = -rank
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqliteindex: read hits: %w", err)
	}
	return hits, nil
}

// buildSearchStatement assembles the FTS5 query. Project, include and exclude
// kind, and since filters are appended as bound placeholders in the same order as args.
func buildSearchStatement(query ports.Query, match string, limit int) (string, []any) {
	var builder strings.Builder
	builder.WriteString(searchSelectSQL)
	args := []any{snippetStart, snippetEnd, snippetEllipsis, snippetTokenSpan, match}

	if query.ProjectID != "" {
		builder.WriteString(" AND d.project_id = ?")
		args = append(args, query.ProjectID)
	}
	if clause, kindArgs := kindCondition("d.kind", "IN", query.Kinds); clause != "" {
		builder.WriteString(" AND " + clause)
		args = append(args, kindArgs...)
	}
	if clause, kindArgs := kindCondition("d.kind", "NOT IN", query.ExcludeKinds); clause != "" {
		builder.WriteString(" AND " + clause)
		args = append(args, kindArgs...)
	}
	if !query.Since.IsZero() {
		builder.WriteString(" AND d.updated_at >= ?")
		args = append(args, query.Since.UnixNano())
	}

	builder.WriteString(searchOrderSQL)
	args = append(args, limit)
	return builder.String(), args
}

// kindCondition returns "column OP (?,?,...)" with one placeholder per kind,
// or an empty clause when there are no kinds to filter on.
func kindCondition(column, operator string, kinds []string) (string, []any) {
	if len(kinds) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(kinds))
	args := make([]any, len(kinds))
	for index, kind := range kinds {
		placeholders[index] = "?"
		args[index] = kind
	}
	return fmt.Sprintf("%s %s (%s)", column, operator, strings.Join(placeholders, ",")), args
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

// recordID returns the record id of an indexed document key, stripping the
// project scope. Keys of other documents (state, dumps) are returned unchanged.
func recordID(projectID, key string) string {
	return strings.TrimPrefix(key, projectID+"/")
}
