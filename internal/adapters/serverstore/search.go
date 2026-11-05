package serverstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/adapters/httpapi"
	"github.com/levapveeskela/lv24-brain/internal/adapters/postgres"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 100

	// searchFetchFactor over-fetches candidates so title matches are not lost
	// before in-memory scoring.
	searchFetchFactor = 4
	maxSearchFetch    = 500

	snippetBytes = 160
)

// Search performs a case-insensitive substring search across notes, decisions,
// attempts and handoffs. postgres.Store has no search implementation, so this
// adapter owns the read-only query. All input is parameterized; LIKE wildcards
// in the query text are escaped.
func (s *Store) Search(ctx context.Context, query ports.Query) ([]ports.Hit, error) {
	text := strings.ToLower(strings.TrimSpace(query.Text))
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
	fetchLimit := limit * searchFetchFactor
	if fetchLimit > maxSearchFetch {
		fetchLimit = maxSearchFetch
	}

	rows, err := s.pg.Pool().Query(ctx, searchSQL, query.ProjectID, "%"+escapeLike(text)+"%", fetchLimit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	hits := make([]ports.Hit, 0, fetchLimit)
	for rows.Next() {
		var id, projectID, kind, title, body, extra string
		if err := rows.Scan(&id, &projectID, &kind, &title, &body, &extra); err != nil {
			return nil, mapError(err)
		}
		score := 1.0
		if strings.Contains(strings.ToLower(title), text) {
			score = 2.0
		}
		content := strings.TrimSpace(body + "\n" + extra)
		hits = append(hits, ports.Hit{
			ID:        id,
			ProjectID: projectID,
			Kind:      kind,
			Title:     title,
			Snippet:   snippet(content, text, snippetBytes),
			Score:     score,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, mapError(err)
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].ID < hits[j].ID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// searchSQL unions every searchable knowledge table, filters by project and
// substring, and leaves scoring to the caller. ILIKE uses '!' as the escape
// character so no backslash-level quoting is involved.
const searchSQL = `
SELECT id, project_id, kind, title, body, extra
FROM (
    SELECT id, project_id, 'note'::text AS kind, title, body, ''::text AS extra
    FROM notes
    UNION ALL
    SELECT id, project_id, 'decision'::text, title, decision_context,
           coalesce(array_to_string(alternatives, ' '), '') || ' ' ||
           coalesce(array_to_string(consequences, ' '), '') || ' ' ||
           coalesce(array_to_string(evidence, ' '), '')
    FROM decisions
    UNION ALL
    SELECT id, project_id, 'attempt'::text, title,
           coalesce(hypothesis, '') || ' ' || coalesce(result, '') || ' ' || coalesce(reason, ''), ''::text
    FROM attempts
    UNION ALL
    SELECT id, project_id, 'handoff'::text, summary,
           coalesce(git_context, '') || ' ' || coalesce(next_action, ''), ''::text
    FROM handoffs
) AS items
WHERE ($1 = '' OR project_id = $1)
  AND (title || ' ' || body || ' ' || extra) ILIKE $2 ESCAPE '!'
ORDER BY id
LIMIT $3`

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

func snippet(body, needle string, max int) string {
	if body == "" || max <= 0 {
		return ""
	}
	runes := []rune(body)
	if len(runes) <= max {
		return strings.TrimSpace(body)
	}
	index := indexRunes([]rune(strings.ToLower(body)), []rune(strings.ToLower(needle)))
	if index < 0 {
		index = 0
	}
	start := index - max/3
	if start < 0 {
		start = 0
	}
	end := start + max
	if end > len(runes) {
		end = len(runes)
		start = end - max
		if start < 0 {
			start = 0
		}
	}
	return strings.TrimSpace(string(runes[start:end]))
}

func indexRunes(haystack, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
