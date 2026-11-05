package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const auditColumns = `id, revision, actor, action, entity_kind, entity_id, detail, created_at`

// AuditEvent is one append-only audit record.
type AuditEvent struct {
	ID         int64          `json:"id"`
	Revision   int64          `json:"revision"`
	Actor      string         `json:"actor,omitempty"`
	Action     string         `json:"action"`
	EntityKind string         `json:"entityKind,omitempty"`
	EntityID   string         `json:"entityId,omitempty"`
	Detail     map[string]any `json:"detail,omitempty"`
	CreatedAt  time.Time      `json:"createdAt"`
}

// AuditFilter narrows an audit listing.
type AuditFilter struct {
	Actor      string
	Action     string
	EntityKind string
	EntityID   string
	Since      *time.Time
	Until      *time.Time
	Limit      int
	Offset     int
}

// RecordAuditEvent appends a standalone audit event and revision. Use it for
// administrative actions that do not mutate a stored entity (backup, settings,
// connector configuration). Detail must not contain secrets or credentials.
func (s *Store) RecordAuditEvent(ctx context.Context, action, entityKind, entityID string, detail map[string]any) (AuditEvent, domain.SyncRevision, error) {
	action = strings.TrimSpace(action)
	if action == "" {
		return AuditEvent{}, domain.SyncRevision{}, domain.NewValidationError("audit.action", "is required")
	}
	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var event AuditEvent
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, entityKind, entityID, detail, at)
		if err != nil {
			return err
		}
		eventID, err := insertAuditEventTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: action,
			EntityKind: entityKind, EntityID: entityID, Detail: detail, At: at,
		})
		if err != nil {
			return err
		}
		fetched, err := getAuditEvent(ctx, tx, eventID)
		if err != nil {
			return err
		}
		event = fetched
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return AuditEvent{}, domain.SyncRevision{}, err
	}
	return event, revision, nil
}

// GetAuditEvent loads one audit event.
func (s *Store) GetAuditEvent(ctx context.Context, id int64) (AuditEvent, error) {
	if id < 0 {
		return AuditEvent{}, domain.NewValidationError("audit.id", "must be non-negative")
	}
	return getAuditEvent(ctx, s.pool, id)
}

// ListAuditEvents returns audit events newest first with optional filters.
func (s *Store) ListAuditEvents(ctx context.Context, filter AuditFilter) ([]AuditEvent, error) {
	query := "SELECT " + auditColumns + " FROM audit_events"
	args := make([]any, 0, 8)
	conditions := make([]string, 0, 6)
	if filter.Actor != "" {
		args = append(args, filter.Actor)
		conditions = append(conditions, fmt.Sprintf("actor = $%d", len(args)))
	}
	if filter.Action != "" {
		args = append(args, filter.Action)
		conditions = append(conditions, fmt.Sprintf("action = $%d", len(args)))
	}
	if filter.EntityKind != "" {
		args = append(args, filter.EntityKind)
		conditions = append(conditions, fmt.Sprintf("entity_kind = $%d", len(args)))
	}
	if filter.EntityID != "" {
		args = append(args, filter.EntityID)
		conditions = append(conditions, fmt.Sprintf("entity_id = $%d", len(args)))
	}
	if filter.Since != nil {
		args = append(args, filter.Since.UTC())
		conditions = append(conditions, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if filter.Until != nil {
		args = append(args, filter.Until.UTC())
		conditions = append(conditions, fmt.Sprintf("created_at < $%d", len(args)))
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
		return nil, mapPgError("list audit events", err)
	}
	return collectRows(rows, scanAuditEvent)
}

func getAuditEvent(ctx context.Context, q dbQuerier, id int64) (AuditEvent, error) {
	record, err := scanAuditEvent(q.QueryRow(ctx, `SELECT `+auditColumns+` FROM audit_events WHERE id = $1`, id))
	if err != nil {
		return AuditEvent{}, mapPgError("get audit event", err)
	}
	return record, nil
}

func scanAuditEvent(row rowScanner) (AuditEvent, error) {
	var record AuditEvent
	var detail []byte
	if err := row.Scan(
		&record.ID, &record.Revision, &record.Actor, &record.Action,
		&record.EntityKind, &record.EntityID, &detail, &record.CreatedAt,
	); err != nil {
		return AuditEvent{}, err
	}
	decoded, err := unmarshalJSON(detail, map[string]any{})
	if err != nil {
		return AuditEvent{}, err
	}
	record.Detail = decoded
	record.CreatedAt = record.CreatedAt.UTC()
	return record, nil
}
