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

// boolToInt converts a boolean to its SQLite integer form.
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// Close releases the database handle.
func (i *Index) Close() error {
	return i.db.Close()
}

// Upsert inserts or replaces one indexed document and its derived relation
// rows. Both happen in one transaction, so a reader never sees a document with
// a stale relation set. Another process writing the same index can make the
// transaction lose the lock, so the whole unit is retried while the database is
// busy.
func (i *Index) Upsert(ctx context.Context, doc ports.Document) error {
	if err := validateDocument(doc); err != nil {
		return err
	}
	if err := validateRelations(doc); err != nil {
		return err
	}
	return retryOnBusy(ctx, busyTimeout, func() error {
		return i.upsertOnce(ctx, doc)
	})
}

// upsertOnce performs one upsert transaction.
func (i *Index) upsertOnce(ctx context.Context, doc ports.Document) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqliteindex: begin upsert %q: %w", doc.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, upsertSQL,
		doc.ID,
		doc.ProjectID,
		doc.Kind,
		doc.Title,
		doc.Body,
		doc.UpdatedAt.UnixNano(),
		validFromNanos(doc),
		nullableNanos(doc.ValidTo),
		nullableString(doc.SupersededBy),
		nullableString(doc.Supersedes),
		doc.Status,
		doc.Confidence,
		doc.Subtype,
		normalizedScope(doc.Scope),
		doc.TopicKey,
		normalizedOccurrences(doc.Occurrences),
		boolToInt(doc.Pinned),
	); err != nil {
		return fmt.Errorf("sqliteindex: upsert %q: %w", doc.ID, err)
	}
	if _, err := tx.ExecContext(ctx, deleteRelationsSQL, doc.ID); err != nil {
		return fmt.Errorf("sqliteindex: clear relations of %q: %w", doc.ID, err)
	}
	for _, relation := range doc.Relations {
		if _, err := tx.ExecContext(ctx, upsertRelationSQL,
			doc.ID,
			doc.ProjectID,
			relation.Target,
			relation.Relation,
			relation.Status,
			relation.Confidence,
			relation.Reason,
			nullableNanos(relation.JudgedAt),
			relation.VerdictBy,
		); err != nil {
			return fmt.Errorf("sqliteindex: index relation %q of %q: %w", relation.Target, doc.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqliteindex: commit upsert %q: %w", doc.ID, err)
	}
	return nil
}

// validFromNanos is the stored valid_from. A document without one became true
// when it was last written, matching records created before validity existed.
func validFromNanos(doc ports.Document) int64 {
	if !doc.ValidFrom.IsZero() {
		return doc.ValidFrom.UnixNano()
	}
	return doc.UpdatedAt.UnixNano()
}

// nullableNanos stores the zero time as SQL NULL: an empty valid_to is a
// record that is still current.
func nullableNanos(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return at.UnixNano()
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.TrimSpace(value)
}

// normalizedScope stores an explicit project scope for documents written
// without one, so a scope filter matches either spelling.
func normalizedScope(scope string) string {
	if value := strings.TrimSpace(scope); value != "" {
		return value
	}
	return string(domain.ScopeProject)
}

func normalizedOccurrences(count int) int {
	if count < 1 {
		return 1
	}
	return count
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

// Delete removes a document and its relation rows from the index. Unknown ids
// are ignored. Like Upsert, the unit is retried while another process holds the
// write lock.
func (i *Index) Delete(ctx context.Context, id string) error {
	if err := domain.ValidateRequired("index.document.id", id); err != nil {
		return err
	}
	return retryOnBusy(ctx, busyTimeout, func() error {
		return i.deleteOnce(ctx, id)
	})
}

func (i *Index) deleteOnce(ctx context.Context, id string) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqliteindex: begin delete %q: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, deleteRelationsSQL, id); err != nil {
		return fmt.Errorf("sqliteindex: delete relations of %q: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, deleteSQL, id); err != nil {
		return fmt.Errorf("sqliteindex: delete %q: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqliteindex: commit delete %q: %w", id, err)
	}
	return nil
}

// Search returns matching documents ordered by the composite score, then
// recency and id. A query without a match mode keeps the historical behaviour:
// every term must match (AND), and when that finds nothing and the query has
// two or more terms the same terms are retried with OR. MatchModeAll skips the
// retry, MatchModeAny starts with OR. Filters apply to every pass.
func (i *Index) Search(ctx context.Context, query ports.Query) ([]ports.Hit, error) {
	mode := strings.TrimSpace(query.MatchMode)
	if mode == ports.MatchModeAny {
		match, ok := matchExpression(query.Text, joinAny)
		if !ok {
			return []ports.Hit{}, nil
		}
		return i.runSearch(ctx, query, match)
	}
	match, ok := matchExpression(query.Text, joinAll)
	if !ok {
		return []ports.Hit{}, nil
	}
	hits, err := i.runSearch(ctx, query, match)
	if err != nil || len(hits) > 0 || mode == ports.MatchModeAll || termCount(query.Text) < 2 {
		return hits, err
	}
	anyMatch, _ := matchExpression(query.Text, joinAny)
	return i.runSearch(ctx, query, anyMatch)
}

// runSearch executes one FTS5 MATCH expression with the query filters applied.
// Hits carry the composite score, the signals that fired and, when the query
// asks for it, the weighted contribution of each signal and the document body.
func (i *Index) runSearch(ctx context.Context, query ports.Query, match string) ([]ports.Hit, error) {
	statement, args := buildSearchStatement(query, match)
	rows, err := i.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("sqliteindex: search: %w", err)
	}
	defer rows.Close()

	now := query.Now
	if now.IsZero() {
		now = time.Now()
	}
	hits := make([]ports.Hit, 0, normalizeLimit(query.Limit))
	for rows.Next() {
		var (
			hit          ports.Hit
			snippet      sql.NullString
			body         sql.NullString
			score        float64
			contrib      [5]float64
			validFrom    int64
			validTo      sql.NullInt64
			supersededBy sql.NullString
			status       string
			confidence   float64
			subtype      string
			scope        string
			topicKey     string
			occurrences  int
			pinnedFlag   int
			updatedAt    int64
		)
		if err := rows.Scan(&hit.ID, &hit.ProjectID, &hit.Kind, &hit.Title, &snippet, &body, &score,
			&contrib[0], &contrib[1], &contrib[2], &contrib[3], &contrib[4],
			&validFrom, &validTo, &supersededBy, &status, &confidence,
			&subtype, &scope, &topicKey, &occurrences, &pinnedFlag, &updatedAt); err != nil {
			return nil, fmt.Errorf("sqliteindex: scan hit: %w", err)
		}
		hit.ID = recordID(hit.ProjectID, hit.ID)
		hit.Snippet = snippet.String
		hit.Body = body.String
		hit.Score = score
		hit.Subtype = subtype
		hit.Scope = scope
		hit.TopicKey = topicKey
		hit.Occurrences = normalizedOccurrences(occurrences)
		fresh := isFresh(time.Unix(0, updatedAt), now)
		hit.MatchReasons = matchReasons(hit.Kind, contributions(contrib), fresh, pinned(hit.Kind, status, confidence, pinnedFlag == 1))
		if query.Explain {
			hit.Explain = &ports.Explain{
				BM25:       contrib[0],
				Recency:    contrib[1],
				Importance: contrib[2],
				Pin:        contrib[3],
				Link:       contrib[4],
			}
		}
		if validFrom > 0 {
			hit.ValidFrom = time.Unix(0, validFrom).UTC()
		}
		if validTo.Valid {
			hit.ValidTo = time.Unix(0, validTo.Int64).UTC()
		}
		hit.SupersededBy = supersededBy.String
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqliteindex: read hits: %w", err)
	}
	if len(hits) == 0 {
		// Callers merge empty pages into a nil slice; return an empty one so
		// JSON encodes [] rather than null.
		return []ports.Hit{}, nil
	}
	return hits, nil
}

// contributions converts the scanned contribution array into a slice.
func contributions(values [5]float64) []float64 {
	return values[:]
}

// buildSearchStatement assembles the ranked FTS5 query. The four snippet
// placeholders, the MATCH placeholder and the ranking instant come first;
// project, kind, since, validity, score and limit follow in the order the
// placeholders appear in rankedSelectPrefix and rankedSelectSuffix.
func buildSearchStatement(query ports.Query, match string) (string, []any) {
	var builder strings.Builder
	builder.WriteString(rankedSelectPrefix(query.IncludeBody))
	// Placeholder order follows the SQL text: the four snippet arguments, the
	// ranking instant (recency) and then the MATCH expression.
	args := []any{snippetStart, snippetEnd, snippetEllipsis, snippetTokenSpan, query.NowOrClock().Unix(), match}

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
	if subtype := strings.TrimSpace(query.Subtype); subtype != "" {
		builder.WriteString(" AND d.subtype = ?")
		args = append(args, subtype)
	}
	if scope := strings.TrimSpace(query.Scope); scope != "" {
		builder.WriteString(" AND d.scope = ?")
		args = append(args, scope)
	}
	if topicKey := strings.TrimSpace(query.TopicKey); topicKey != "" {
		builder.WriteString(" AND d.topic_key = ?")
		args = append(args, topicKey)
	}
	if !query.Since.IsZero() {
		builder.WriteString(" AND d.updated_at >= ?")
		args = append(args, query.Since.UnixNano())
	}
	// The temporal filter mirrors domain.ValidityFilter: an as-of instant
	// selects records valid then, otherwise a record that ended at or before
	// now is hidden unless the caller asked for superseded ones.
	switch {
	case !query.AsOf.IsZero():
		builder.WriteString(" AND d.valid_from <= ? AND (d.valid_to IS NULL OR d.valid_to > ?)")
		args = append(args, query.AsOf.UnixNano(), query.AsOf.UnixNano())
	case !query.IncludeSuperseded:
		builder.WriteString(" AND (d.valid_to IS NULL OR d.valid_to > ?)")
		args = append(args, query.NowOrClock().UnixNano())
	}

	builder.WriteString(rankedSelectSuffix())
	args = append(args, query.MinScore, normalizeLimit(query.Limit))
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
