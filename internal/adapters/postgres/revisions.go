package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const revisionColumns = `id, kind, actor, entity_kind, entity_id, detail, created_at`

// RevisionRecord is one row of the global server revision trail.
type RevisionRecord struct {
	ID         int64           `json:"id"`
	Kind       domain.SyncKind `json:"kind"`
	Actor      string          `json:"actor,omitempty"`
	EntityKind string          `json:"entityKind,omitempty"`
	EntityID   string          `json:"entityId,omitempty"`
	Detail     map[string]any  `json:"detail,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// Sync converts the record into a domain revision.
func (r RevisionRecord) Sync() domain.SyncRevision {
	return RevisionFromValue(r.ID, r.CreatedAt)
}

// RevisionFilter narrows a revision listing.
type RevisionFilter struct {
	AfterID    int64
	EntityKind string
	EntityID   string
	Limit      int
	Offset     int
}

// CurrentRevision returns the newest server revision. An empty database
// reports revision "0" with a zero timestamp.
func (s *Store) CurrentRevision(ctx context.Context) (domain.SyncRevision, error) {
	var id int64
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT id, created_at FROM revisions ORDER BY id DESC LIMIT 1`).Scan(&id, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SyncRevision{Kind: domain.SyncKindServer, Value: "0"}, nil
	}
	if err != nil {
		return domain.SyncRevision{}, mapPgError("current revision", err)
	}
	return RevisionFromValue(id, at), nil
}

// GetRevision loads one revision row.
func (s *Store) GetRevision(ctx context.Context, id int64) (RevisionRecord, error) {
	if id < 0 {
		return RevisionRecord{}, domain.NewValidationError("revision.id", "must be non-negative")
	}
	record, err := scanRevision(s.pool.QueryRow(ctx, `SELECT `+revisionColumns+` FROM revisions WHERE id = $1`, id))
	if err != nil {
		return RevisionRecord{}, mapPgError("get revision", err)
	}
	return record, nil
}

// ListRevisions returns revisions newest first with optional filters.
func (s *Store) ListRevisions(ctx context.Context, filter RevisionFilter) ([]RevisionRecord, error) {
	query := "SELECT " + revisionColumns + " FROM revisions"
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
	args = append(args, normalizeOffset(filter.Offset))
	offsetPlaceholder := len(args)
	query += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d OFFSET $%d", limitPlaceholder, offsetPlaceholder)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapPgError("list revisions", err)
	}
	return collectRows(rows, scanRevision)
}

func scanRevision(row rowScanner) (RevisionRecord, error) {
	var record RevisionRecord
	var kind string
	var detail []byte
	if err := row.Scan(
		&record.ID, &kind, &record.Actor, &record.EntityKind, &record.EntityID, &detail, &record.CreatedAt,
	); err != nil {
		return RevisionRecord{}, err
	}
	record.Kind = domain.SyncKind(kind)
	decoded, err := unmarshalJSON(detail, map[string]any{})
	if err != nil {
		return RevisionRecord{}, err
	}
	record.Detail = decoded
	record.CreatedAt = record.CreatedAt.UTC()
	return record, nil
}
