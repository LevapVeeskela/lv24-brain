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

	var (
		rows *sql.Rows
		err  error
	)
	args := []any{snippetStart, snippetEnd, snippetEllipsis, snippetTokenSpan, match}
	statement := searchSQL
	if query.ProjectID != "" {
		statement = searchProjectSQL
		args = append(args, query.ProjectID)
	}
	args = append(args, limit)
	rows, err = i.db.QueryContext(ctx, statement, args...)
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

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}
