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
		if err := writeSchemaMeta(ctx, conn); err != nil {
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
	if err := writeSchemaMeta(ctx, conn); err != nil {
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

// Index metadata keys stored in the meta table.
const (
	metaSchemaVersion = "schema_version"
	metaGeneration    = "index_generation"
	metaIndexedFrom   = "indexed_from"
)

// writeSchemaMeta records the schema version the open index runs. It runs in
// the schema transaction after the table exists, so Meta can always name the
// version without a special case.
func writeSchemaMeta(ctx context.Context, db schemaExecer) error {
	if _, err := db.ExecContext(ctx, upsertMetaSQL, metaSchemaVersion, strconv.Itoa(schemaVersion)); err != nil {
		return fmt.Errorf("sqliteindex: record schema version: %w", err)
	}
	return nil
}

// Meta returns the index metadata of the last completed reindex:
// index_generation, indexed_from and schema_version. An index that was never
// reindexed returns only schema_version.
func (i *Index) Meta(ctx context.Context) (map[string]string, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return nil, fmt.Errorf("sqliteindex: read meta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	meta := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("sqliteindex: read meta: %w", err)
		}
		meta[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqliteindex: read meta: %w", err)
	}
	return meta, nil
}

// SetMeta upserts index metadata, for example index_generation and
// indexed_from after a successful reindex. Keys are written in one transaction,
// so a reader never sees a generation from one run next to a HEAD from another.
func (i *Index) SetMeta(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if err := domain.ValidateRequired("index.meta.key", key); err != nil {
			return err
		}
		keys = append(keys, key)
	}
	return retryOnBusy(ctx, busyTimeout, func() error {
		tx, err := i.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqliteindex: begin meta write: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		for _, key := range keys {
			if _, err := tx.ExecContext(ctx, upsertMetaSQL, key, values[key]); err != nil {
				return fmt.Errorf("sqliteindex: write meta %q: %w", key, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqliteindex: commit meta write: %w", err)
		}
		return nil
	})
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

// projectionColumns are the derived, machine-independent columns of one
// document. They are computed by the use-case layer because they describe the
// record, not the storage: content_hash collapses duplicates across clones and
// stub marks a low-signal record the retrieval hides by default.
type projectionColumns struct {
	contentHash string
	stub        bool
}

// Upsert inserts or replaces one indexed document and its derived relation
// rows. The derived projection columns are left empty; callers that project a
// record (the reindex and the write paths) use UpsertProjection instead.
func (i *Index) Upsert(ctx context.Context, doc ports.Document) error {
	return i.upsert(ctx, doc, projectionColumns{})
}

// UpsertProjection is Upsert with the derived projection columns. The hash and
// the stub flag are computed once by the projection layer and stored verbatim,
// so two machines indexing the same record derive the same values.
func (i *Index) UpsertProjection(ctx context.Context, doc ports.Document, contentHash string, stub bool) error {
	if err := validateContentHash(contentHash); err != nil {
		return err
	}
	return i.upsert(ctx, doc, projectionColumns{contentHash: contentHash, stub: stub})
}

func (i *Index) upsert(ctx context.Context, doc ports.Document, projection projectionColumns) error {
	if err := validateDocument(doc); err != nil {
		return err
	}
	if err := validateRelations(doc); err != nil {
		return err
	}
	return retryOnBusy(ctx, busyTimeout, func() error {
		return i.upsertOnce(ctx, doc, projection)
	})
}

// UpsertBatch writes a whole slice of documents in one transaction with one
// prepared statement set, which is what a reindex needs: a per-document
// transaction costs a commit each and dominates a corpus rebuild. Every
// document is validated before the transaction opens, so a malformed document
// writes nothing. Like Upsert, a lost write lock retries the whole batch; the
// writes are idempotent upserts, so a retry is safe. Callers with a large
// corpus chunk the slice to bound the transaction and its WAL growth.
func (i *Index) UpsertBatch(ctx context.Context, docs []ports.Document) error {
	return i.upsertBatch(ctx, docs, nil)
}

// UpsertProjectionBatch is UpsertBatch with the derived projection columns,
// one entry per document. It is the reindex path: the projection layer already
// computed the hash and stub, and the batch stores them in the same
// transaction as the documents.
func (i *Index) UpsertProjectionBatch(ctx context.Context, docs []ports.Document, contentHashes []string, stubs []bool) error {
	if len(contentHashes) != len(docs) || len(stubs) != len(docs) {
		return domain.NewValidationError("index.document.projection",
			fmt.Sprintf("projection has %d hashes and %d stubs for %d documents", len(contentHashes), len(stubs), len(docs)))
	}
	projections := make([]projectionColumns, len(docs))
	for idx := range docs {
		if err := validateContentHash(contentHashes[idx]); err != nil {
			return err
		}
		projections[idx] = projectionColumns{contentHash: contentHashes[idx], stub: stubs[idx]}
	}
	return i.upsertBatch(ctx, docs, projections)
}

func (i *Index) upsertBatch(ctx context.Context, docs []ports.Document, projections []projectionColumns) error {
	if len(docs) == 0 {
		return nil
	}
	for _, doc := range docs {
		if err := validateDocument(doc); err != nil {
			return err
		}
		if err := validateRelations(doc); err != nil {
			return err
		}
	}
	return retryOnBusy(ctx, busyTimeout, func() error {
		return i.upsertBatchOnce(ctx, docs, projections)
	})
}

// Optimize compacts the FTS5 index and truncates the write-ahead log. A reindex
// writes every document, which leaves the FTS b-tree fragmented and the WAL
// large; both are pure size and read-cost overhead until the segments are
// merged. The merge is required; the log truncation is best effort, because a
// concurrent reader may hold the log and that must not fail a completed reindex.
func (i *Index) Optimize(ctx context.Context) error {
	if _, err := i.db.ExecContext(ctx, `INSERT INTO documents_fts(documents_fts) VALUES('optimize')`); err != nil {
		return fmt.Errorf("sqliteindex: optimize full-text index: %w", err)
	}
	if err := i.Checkpoint(ctx); err != nil && !errors.Is(err, errCheckpointBusy) {
		return err
	}
	return nil
}

// errCheckpointBusy reports that an active reader kept the write-ahead log from
// being truncated. The index is consistent; only the log file stays large.
var errCheckpointBusy = errors.New("checkpoint blocked by an active reader")

// Checkpoint truncates the write-ahead log, retrying while another process or
// connection holds a read lock. An in-memory index has no WAL and succeeds
// without touching a file.
func (i *Index) Checkpoint(ctx context.Context) error {
	deadline := time.Now().Add(busyTimeout)
	delay := retryInitialDelay
	for {
		busy, err := i.checkpointOnce(ctx)
		if err != nil {
			return err
		}
		if busy == 0 {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("sqliteindex: %w", errCheckpointBusy)
		}
		wait := min(delay, remaining)
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

// checkpointOnce runs PRAGMA wal_checkpoint(TRUNCATE) and reports whether the
// truncation had to stop for a reader. The first column is that busy flag; the
// other two are the log size and the frames checkpointed.
func (i *Index) checkpointOnce(ctx context.Context) (int, error) {
	var (
		busy    int
		log     int
		checked int
	)
	if err := i.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checked); err != nil {
		return 0, fmt.Errorf("sqliteindex: checkpoint the write-ahead log: %w", err)
	}
	return busy, nil
}

// upsertOnce performs one upsert transaction.
func (i *Index) upsertOnce(ctx context.Context, doc ports.Document, projection projectionColumns) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqliteindex: begin upsert %q: %w", doc.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := writeDocument(ctx, txWriter{tx: tx}, doc, projection); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqliteindex: commit upsert %q: %w", doc.ID, err)
	}
	return nil
}

// upsertBatchOnce performs one batch transaction. The statements are prepared
// once for the transaction, so a batch compiles its SQL a fixed number of times
// instead of three times per document.
func (i *Index) upsertBatchOnce(ctx context.Context, docs []ports.Document, projections []projectionColumns) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqliteindex: begin batch of %d documents: %w", len(docs), err)
	}
	defer func() { _ = tx.Rollback() }()
	writer, err := newStmtWriter(ctx, tx)
	if err != nil {
		return err
	}
	for idx, doc := range docs {
		if err := writeDocument(ctx, writer, doc, projectionAt(projections, idx)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqliteindex: commit batch of %d documents: %w", len(docs), err)
	}
	return nil
}

// projectionAt returns the projection of one batch document. A plain
// UpsertBatch has none, which leaves the derived columns empty.
func projectionAt(projections []projectionColumns, idx int) projectionColumns {
	if idx < 0 || idx >= len(projections) {
		return projectionColumns{}
	}
	return projections[idx]
}

// docWriter writes one document and its derived relation rows. Both the
// per-document transaction and the batch use it, so the two paths cannot drift.
type docWriter interface {
	upsertDocument(ctx context.Context, doc ports.Document, projection projectionColumns) error
	clearRelations(ctx context.Context, id string) error
	addRelation(ctx context.Context, doc ports.Document, relation ports.RelationLink) error
}

// writeDocument replaces one document and its relation projection.
func writeDocument(ctx context.Context, writer docWriter, doc ports.Document, projection projectionColumns) error {
	if err := writer.upsertDocument(ctx, doc, projection); err != nil {
		return err
	}
	if err := writer.clearRelations(ctx, doc.ID); err != nil {
		return err
	}
	for _, relation := range doc.Relations {
		if err := writer.addRelation(ctx, doc, relation); err != nil {
			return err
		}
	}
	return nil
}

// txWriter runs the statements unprepared, for the single-document path.
type txWriter struct{ tx *sql.Tx }

func (w txWriter) upsertDocument(ctx context.Context, doc ports.Document, projection projectionColumns) error {
	if _, err := w.tx.ExecContext(ctx, upsertSQL, documentArgs(doc, projection)...); err != nil {
		return fmt.Errorf("sqliteindex: upsert %q: %w", doc.ID, err)
	}
	return nil
}

func (w txWriter) clearRelations(ctx context.Context, id string) error {
	if _, err := w.tx.ExecContext(ctx, deleteRelationsSQL, id); err != nil {
		return fmt.Errorf("sqliteindex: clear relations of %q: %w", id, err)
	}
	return nil
}

func (w txWriter) addRelation(ctx context.Context, doc ports.Document, relation ports.RelationLink) error {
	if _, err := w.tx.ExecContext(ctx, upsertRelationSQL, relationArgs(doc, relation)...); err != nil {
		return fmt.Errorf("sqliteindex: index relation %q of %q: %w", relation.Target, doc.ID, err)
	}
	return nil
}

// stmtWriter runs the prepared statements of one batch transaction. The
// statements belong to the transaction, so Commit or Rollback releases them.
type stmtWriter struct {
	upsert   *sql.Stmt
	clear    *sql.Stmt
	relation *sql.Stmt
}

func newStmtWriter(ctx context.Context, tx *sql.Tx) (stmtWriter, error) {
	writer := stmtWriter{}
	for _, spec := range []struct {
		sql string
		dst **sql.Stmt
	}{
		{upsertSQL, &writer.upsert},
		{deleteRelationsSQL, &writer.clear},
		{upsertRelationSQL, &writer.relation},
	} {
		stmt, err := tx.PrepareContext(ctx, spec.sql)
		if err != nil {
			return stmtWriter{}, fmt.Errorf("sqliteindex: prepare batch statement: %w", err)
		}
		*spec.dst = stmt
	}
	return writer, nil
}

func (w stmtWriter) upsertDocument(ctx context.Context, doc ports.Document, projection projectionColumns) error {
	if _, err := w.upsert.ExecContext(ctx, documentArgs(doc, projection)...); err != nil {
		return fmt.Errorf("sqliteindex: upsert %q: %w", doc.ID, err)
	}
	return nil
}

func (w stmtWriter) clearRelations(ctx context.Context, id string) error {
	if _, err := w.clear.ExecContext(ctx, id); err != nil {
		return fmt.Errorf("sqliteindex: clear relations of %q: %w", id, err)
	}
	return nil
}

func (w stmtWriter) addRelation(ctx context.Context, doc ports.Document, relation ports.RelationLink) error {
	if _, err := w.relation.ExecContext(ctx, relationArgs(doc, relation)...); err != nil {
		return fmt.Errorf("sqliteindex: index relation %q of %q: %w", relation.Target, doc.ID, err)
	}
	return nil
}

// documentArgs binds one document to upsertSQL.
func documentArgs(doc ports.Document, projection projectionColumns) []any {
	return []any{
		doc.ID,
		doc.ProjectID,
		doc.Kind,
		doc.Title,
		doc.Body,
		doc.UpdatedAt.UnixNano(),
		contentUpdatedAtNanos(doc),
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
		// Origin and trust are stored verbatim: the projection is not the place
		// to normalize provenance, and an empty value must stay empty so the
		// ranking can tell "not projected" from a known trust level.
		doc.Origin,
		doc.Trust,
		// The derived columns come from the projection layer; a plain Upsert
		// leaves them empty so an adapter-only write is visibly unprojected.
		projection.contentHash,
		boolToInt(projection.stub),
	}
}

// validateContentHash rejects a projected document without a hash: an empty
// value would make duplicate collapsing silently skip the record.
func validateContentHash(contentHash string) error {
	if err := domain.ValidateRequired("index.document.contentHash", contentHash); err != nil {
		return err
	}
	return nil
}

// relationArgs binds one relation row to upsertRelationSQL.
func relationArgs(doc ports.Document, relation ports.RelationLink) []any {
	return []any{
		doc.ID,
		doc.ProjectID,
		relation.Target,
		relation.Relation,
		relation.Status,
		relation.Confidence,
		relation.Reason,
		nullableNanos(relation.JudgedAt),
		relation.VerdictBy,
	}
}

// validFromNanos is the stored valid_from. A document without one became true
// when it was last written, matching records created before validity existed.
func validFromNanos(doc ports.Document) int64 {
	if !doc.ValidFrom.IsZero() {
		return doc.ValidFrom.UnixNano()
	}
	return doc.UpdatedAt.UnixNano()
}

// contentUpdatedAtNanos is the stored content_updated_at. A document without
// one changed its text for the last time when it was written, which is the
// conservative answer for a since window: the record is not hidden.
func contentUpdatedAtNanos(doc ports.Document) int64 {
	if !doc.ContentUpdatedAt.IsZero() {
		return doc.ContentUpdatedAt.UnixNano()
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
			contentHash  string
			updatedAt    int64
		)
		if err := rows.Scan(&hit.ID, &hit.ProjectID, &hit.Kind, &hit.Title, &snippet, &body, &score,
			&contrib[0], &contrib[1], &contrib[2], &contrib[3], &contrib[4],
			&validFrom, &validTo, &supersededBy, &status, &confidence,
			&subtype, &scope, &topicKey, &occurrences, &pinnedFlag, &contentHash, &updatedAt); err != nil {
			return nil, fmt.Errorf("sqliteindex: scan hit: %w", err)
		}
		hit.ID = recordID(hit.ProjectID, hit.ID)
		hit.Snippet = snippet.String
		hit.Body = body.String
		hit.ContentHash = contentHash
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
	if !query.IncludeStubs {
		// IDX-4: a stub stays in the index and in counts; retrieval hides it
		// unless the caller explicitly asks for it.
		builder.WriteString(" AND d.stub = 0")
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
		// A record's text change time, not its revision: review, snooze and
		// validity writes move updated_at but leave the text alone. Records
		// indexed before the column existed have 0 there, so fall back to
		// their updated_at.
		builder.WriteString(" AND COALESCE(NULLIF(d.content_updated_at, 0), d.updated_at) >= ?")
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
