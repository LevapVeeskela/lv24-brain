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
)

// Index stores documents in SQLite with an FTS5 mirror for search.
type Index struct {
	db *sql.DB
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
	if err := index.applySchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return index, nil
}

// dataSourceName builds the modernc.org/sqlite DSN with per-connection pragmas.
func dataSourceName(path string) string {
	pragmas := "?_pragma=busy_timeout(" + strconv.FormatInt(busyTimeout.Milliseconds(), 10) + ")" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		return "file::memory:" + pragmas
	}
	return "file:" + filepath.ToSlash(path) + pragmas
}

func (i *Index) applySchema(ctx context.Context) error {
	for _, statement := range schemaStatements {
		if _, err := i.db.ExecContext(ctx, statement); err != nil {
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
func (i *Index) Search(ctx context.Context, query ports.Query) ([]ports.Hit, error) {
	match, ok := matchExpression(query.Text)
	if !ok {
		return []ports.Hit{}, nil
	}
	limit := normalizeLimit(query.Limit)
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
// kind filters are appended as bound placeholders in the same order as args.
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
