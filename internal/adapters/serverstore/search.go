package serverstore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/postgres"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 100
)

// Search runs a PostgreSQL full-text search over notes, decisions, attempts
// and handoffs. Matching uses the generated search_tsv columns (migration
// 0007) with websearch_to_tsquery, so quoted phrases, OR and exclusion work;
// plain words additionally match their word-form stem as a prefix (see
// buildTSQuery), and query.Since drops older items.
// Ranking and LIMIT happen in SQL before snippets are built, so the result is
// the true top N. When the query has no lexemes (only punctuation), it falls
// back to a case-insensitive substring match. All input is parameterized.
func (s *Store) Search(ctx context.Context, query ports.Query) ([]ports.Hit, error) {
	return s.SearchScoped(ctx, query, httpapi.ProjectScope{})
}

// SearchScoped is Search restricted to the projects a caller may read: hits
// outside scope.Only (when set) or inside scope.Exclude are dropped in SQL,
// before ranking and LIMIT, so pages stay full and nothing leaks.
func (s *Store) SearchScoped(ctx context.Context, query ports.Query, scope httpapi.ProjectScope) ([]ports.Hit, error) {
	text := strings.TrimSpace(query.Text)
	if text == "" {
		return nil, domain.NewValidationError("search.text", "is required")
	}
	limit := query.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	tsquery, tsArgs := buildTSQuery(text, searchFirstTSParam)
	args := append([]any{
		query.ProjectID,
		optionalTime(query.Since),
		"%" + escapeLike(text) + "%",
		optionalArray(query.Kinds),
		optionalArray(query.ExcludeKinds),
		limit,
		optionalArray(scope.Only),
		optionalArray(scope.Exclude),
	}, tsArgs...)
	rows, err := s.pg.Pool().Query(ctx, searchStatement(tsquery), args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	hits := make([]ports.Hit, 0, limit)
	for rows.Next() {
		var hit ports.Hit
		if err := rows.Scan(&hit.ID, &hit.ProjectID, &hit.Kind, &hit.Title, &hit.Snippet, &hit.Score); err != nil {
			return nil, mapError(err)
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return hits, nil
}

// optionalTime binds the zero time as SQL NULL ("no filter").
func optionalTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

// searchStatement returns searchSQL with the tsquery expression built by
// buildTSQuery. The expression holds only fixed SQL and $N placeholders, never
// user text.
func searchStatement(tsquery string) string {
	return strings.Replace(searchSQL, searchTSQueryMarker, tsquery, 1)
}

// optionalArray binds an empty filter as SQL NULL, which the query treats as
// "no filter"; a non-empty slice binds as text[].
func optionalArray(values []string) any {
	if len(values) == 0 {
		return nil
	}
	return values
}

// searchTSQueryMarker is replaced by the tsquery expression of buildTSQuery.
const searchTSQueryMarker = "{{tsquery}}"

// searchFirstTSParam is the first placeholder the tsquery expression uses.
const searchFirstTSParam = 9

// searchSQL parameters: $1 project id (empty for all), $2 since (NULL = no
// time filter), $3 ILIKE pattern for the fallback, $4 kinds (NULL = all), $5
// excluded kinds (NULL = none), $6 limit, $7 readable project ids (NULL =
// all), $8 unreadable project ids (NULL = none), $9 onwards the tsquery
// fragments of buildTSQuery.
//
// The CTEs are: query (the tsquery), items (union of the searchable tables),
// matched (filters plus rank), page (ranked top N). Snippets are built only
// for the N rows in page, with ts_headline, bounded to MaxWords.
const searchSQL = `
WITH query AS (
    SELECT {{tsquery}} AS tsq, $3::text AS pattern
), items AS (
    SELECT id, project_id, 'note'::text AS kind, title,
           coalesce(body, '') AS content, updated_at, search_tsv AS tsv
    FROM notes
    UNION ALL
    SELECT id, project_id, 'decision'::text, title,
           coalesce(decision_context, '') || ' ' || brain_search_join(alternatives) || ' ' ||
           brain_search_join(consequences) || ' ' || brain_search_join(evidence),
           updated_at, search_tsv
    FROM decisions
    UNION ALL
    SELECT id, project_id, 'attempt'::text, title,
           coalesce(hypothesis, '') || ' ' || coalesce(result, '') || ' ' || coalesce(reason, ''),
           updated_at, search_tsv
    FROM attempts
    UNION ALL
    SELECT id, project_id, 'handoff'::text, summary,
           coalesce(git_context, '') || ' ' || coalesce(next_action, ''),
           created_at, search_tsv
    FROM handoffs
), matched AS (
    SELECT items.id, items.project_id, items.kind, items.title, items.content,
           items.updated_at, query.tsq,
           CAST(CASE WHEN numnode(query.tsq) > 0 THEN ts_rank(items.tsv, query.tsq) ELSE 0 END AS float8) AS rank
    FROM items
    CROSS JOIN query
    WHERE ($1 = '' OR items.project_id = $1)
      AND ($4::text[] IS NULL OR items.kind = ANY($4::text[]))
      AND ($5::text[] IS NULL OR NOT (items.kind = ANY($5::text[])))
      AND ($7::text[] IS NULL OR items.project_id = ANY($7::text[]))
      AND ($8::text[] IS NULL OR NOT (items.project_id = ANY($8::text[])))
      AND ($2::timestamptz IS NULL OR items.updated_at >= $2::timestamptz)
      AND (
            (numnode(query.tsq) > 0 AND items.tsv @@ query.tsq)
         OR (numnode(query.tsq) = 0 AND (items.title || ' ' || items.content) ILIKE query.pattern ESCAPE '!')
      )
), page AS (
    SELECT * FROM matched
    ORDER BY rank DESC, updated_at DESC, id ASC
    LIMIT $6
)
SELECT id, project_id, kind, title,
       ts_headline('simple', content, tsq,
                   'MaxFragments=1, MaxWords=30, MinWords=10, StartSel="[", StopSel="]"') AS snippet,
       rank
FROM page
ORDER BY rank DESC, updated_at DESC, id ASC`

// ListRevisions returns the global revision trail. Descending listings and all
// filters are delegated to postgres.Store. Ascending listings are issued here
// because postgres.RevisionFilter cannot express ascending order, and SSE
// replay requires oldest-first batches.
func (s *Store) ListRevisions(ctx context.Context, filter httpapi.RevisionFilter) ([]httpapi.RevisionRecord, error) {
	if !filter.Ascending {
		records, err := s.pg.ListRevisions(ctx, postgres.RevisionFilter{
			AfterID:    filter.AfterID,
			EntityKind: filter.EntityKind,
			EntityID:   filter.EntityID,
			Limit:      filter.Limit,
			Offset:     filter.Offset,
		})
		if err != nil {
			return nil, mapError(err)
		}
		revisions := make([]httpapi.RevisionRecord, 0, len(records))
		for _, record := range records {
			revisions = append(revisions, toRevisionRecord(record))
		}
		return revisions, nil
	}
	return s.listRevisionsAscending(ctx, filter)
}

func (s *Store) listRevisionsAscending(ctx context.Context, filter httpapi.RevisionFilter) ([]httpapi.RevisionRecord, error) {
	query := `SELECT id, kind, actor, entity_kind, entity_id, detail, created_at FROM revisions`
	args := make([]any, 0, 5)
	conditions := make([]string, 0, 3)
	if filter.AfterID > 0 {
		args = append(args, filter.AfterID)
		conditions = append(conditions, fmt.Sprintf("id > $%d", len(args)))
	}
	if filter.EntityKind != "" {
		args = append(args, filter.EntityKind)
		conditions = append(conditions, fmt.Sprintf("entity_kind = $%d", len(args)))
	}
	if filter.EntityID != "" {
		args = append(args, filter.EntityID)
		conditions = append(conditions, fmt.Sprintf("entity_id = $%d", len(args)))
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	args = append(args, clampLimit(filter.Limit))
	limitPlaceholder := len(args)
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	args = append(args, offset)
	offsetPlaceholder := len(args)
	query += fmt.Sprintf(" ORDER BY id ASC LIMIT $%d OFFSET $%d", limitPlaceholder, offsetPlaceholder)

	rows, err := s.pg.Pool().Query(ctx, query, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	revisions := make([]httpapi.RevisionRecord, 0, 16)
	for rows.Next() {
		var record httpapi.RevisionRecord
		var kind string
		var detail []byte
		if err := rows.Scan(
			&record.ID, &kind, &record.Actor, &record.EntityKind, &record.EntityID, &detail, &record.CreatedAt,
		); err != nil {
			return nil, mapError(err)
		}
		record.Kind = domain.SyncKind(kind)
		if len(detail) > 0 && string(detail) != "null" {
			if err := json.Unmarshal(detail, &record.Detail); err != nil {
				return nil, fmt.Errorf("serverstore: decode revision detail: %w", err)
			}
		}
		record.CreatedAt = record.CreatedAt.UTC()
		revisions = append(revisions, record)
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}
	return revisions, nil
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 500 {
		return 500
	}
	return limit
}

// escapeLike neutralizes LIKE wildcards for the '!' escape character.
func escapeLike(value string) string {
	replacer := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return replacer.Replace(value)
}
