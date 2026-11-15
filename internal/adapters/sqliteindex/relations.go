package sqliteindex

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// relationsDefaultLimit is used when a relation query sets no limit.
	relationsDefaultLimit = 100
	// relationsMaxLimit bounds how many relation rows one query may return.
	relationsMaxLimit = 500
)

var _ ports.RelationIndex = (*Index)(nil)

// validateRelations checks the indexed relation projection before a write. A
// malformed verb or status would make a listing lie, so it fails the upsert
// instead of being stored.
func validateRelations(doc ports.Document) error {
	for i, relation := range doc.Relations {
		field := fmt.Sprintf("index.document.relations[%d]", i)
		if err := domain.ValidateRequired(field+".target", strings.TrimSpace(relation.Target)); err != nil {
			return err
		}
		if !domain.RelationKind(relation.Relation).Valid() {
			return domain.NewValidationError(field+".relation", fmt.Sprintf("unknown relation %q", relation.Relation))
		}
		if !domain.RelationStatus(relation.Status).Valid() {
			return domain.NewValidationError(field+".status", fmt.Sprintf("unknown status %q", relation.Status))
		}
		if relation.Confidence < 0 || relation.Confidence > 1 {
			return domain.NewValidationError(field+".confidence", "must be between 0 and 1")
		}
		if !domain.VerdictBy(relation.VerdictBy).Valid() {
			return domain.NewValidationError(field+".verdictBy", "must be human or agent")
		}
	}
	return nil
}

// Relations returns the stored relation projection, joined with the source
// document so a caller gets the source kind and title without a second read.
// Rows are ordered by project, source, target and verb, which makes repeated
// queries and pagination deterministic.
func (i *Index) Relations(ctx context.Context, query ports.RelationQuery) ([]ports.RelationRow, error) {
	if query.SourceID != "" && query.ProjectID == "" {
		return nil, domain.NewValidationError("projectId", "is required to list the relations of one record")
	}
	if query.TargetID != "" && query.ProjectID == "" {
		return nil, domain.NewValidationError("projectId", "is required to list backlinks")
	}
	if query.Limit < 0 {
		return nil, domain.NewValidationError("limit", "must not be negative")
	}
	var builder strings.Builder
	builder.WriteString(`SELECT r.project_id, r.source_id, COALESCE(d.kind, ''), COALESCE(d.title, ''),
		r.target_id, r.relation, r.status, r.confidence, r.reason, r.judged_at, r.verdict_by
	FROM relations r
	LEFT JOIN documents d ON d.id = r.source_id`)
	var clauses []string
	var args []any
	if query.ProjectID != "" {
		clauses = append(clauses, "r.project_id = ?")
		args = append(args, query.ProjectID)
	}
	if query.SourceID != "" {
		clauses = append(clauses, "r.source_id = ?")
		args = append(args, query.ProjectID+"/"+query.SourceID)
	}
	if query.TargetID != "" {
		clauses = append(clauses, "r.target_id = ?")
		args = append(args, query.TargetID)
	}
	if clause, statusArgs := stringCondition("r.status", query.Statuses); clause != "" {
		clauses = append(clauses, clause)
		args = append(args, statusArgs...)
	}
	if clause, relationArgs := stringCondition("r.relation", query.Relations); clause != "" {
		clauses = append(clauses, clause)
		args = append(args, relationArgs...)
	}
	if len(clauses) > 0 {
		builder.WriteString(" WHERE ")
		builder.WriteString(strings.Join(clauses, " AND "))
	}
	builder.WriteString(" ORDER BY r.project_id, r.source_id, r.target_id, r.relation LIMIT ?")
	args = append(args, relationsLimit(query.Limit))

	rows, err := i.db.QueryContext(ctx, builder.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("sqliteindex: list relations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]ports.RelationRow, 0, relationsLimit(query.Limit))
	for rows.Next() {
		var (
			row      ports.RelationRow
			judgedAt sql.NullInt64
		)
		if err := rows.Scan(&row.ProjectID, &row.SourceID, &row.SourceKind, &row.SourceTitle,
			&row.TargetID, &row.Relation, &row.Status, &row.Confidence, &row.Reason,
			&judgedAt, &row.VerdictBy); err != nil {
			return nil, fmt.Errorf("sqliteindex: scan relation: %w", err)
		}
		row.SourceID = strings.TrimPrefix(row.SourceID, row.ProjectID+"/")
		if judgedAt.Valid {
			row.JudgedAt = time.Unix(0, judgedAt.Int64).UTC()
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqliteindex: read relations: %w", err)
	}
	return out, nil
}

func relationsLimit(limit int) int {
	if limit <= 0 {
		return relationsDefaultLimit
	}
	if limit > relationsMaxLimit {
		return relationsMaxLimit
	}
	return limit
}

// stringCondition returns "column IN (?,?,...)" for values, or an empty clause.
func stringCondition(column string, values []string) (string, []any) {
	if len(values) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	for i, value := range values {
		placeholders[i] = "?"
		args[i] = value
	}
	return fmt.Sprintf("%s IN (%s)", column, strings.Join(placeholders, ",")), args
}
