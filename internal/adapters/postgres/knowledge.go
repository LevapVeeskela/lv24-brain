package postgres

import (
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ListOptions narrows a project-scoped knowledge listing. A zero Limit uses
// the adapter default and values above the maximum are clamped.
type ListOptions struct {
	ProjectID string
	Limit     int
	Offset    int
}

// collectRows scans every row with scan and closes the result set.
func collectRows[T any](rows pgx.Rows, scan func(rowScanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var items []T
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, mapPgError("scan row", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPgError("read rows", err)
	}
	return items, nil
}

// knowledgeListQuery builds a parameterized list query. table, columns and
// orderColumn are package constants, never caller input.
func knowledgeListQuery(table, columns, orderColumn string, opts ListOptions) (string, []any) {
	query := "SELECT " + columns + " FROM " + table
	args := make([]any, 0, 3)
	if opts.ProjectID != "" {
		args = append(args, opts.ProjectID)
		query += fmt.Sprintf(" WHERE project_id = $%d", len(args))
	}
	args = append(args, clampLimit(opts.Limit))
	limitPlaceholder := len(args)
	args = append(args, normalizeOffset(opts.Offset))
	offsetPlaceholder := len(args)
	query += fmt.Sprintf(" ORDER BY %s DESC, id LIMIT $%d OFFSET $%d", orderColumn, limitPlaceholder, offsetPlaceholder)
	return query, args
}
