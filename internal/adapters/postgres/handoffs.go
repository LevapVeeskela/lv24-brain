package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const handoffColumns = `id, project_id, summary, git_context, next_action, permalink, revision, created_at, created_by`

// HandoffRecord is a handoff plus its server revision.
type HandoffRecord struct {
	domain.Handoff
	Revision int64 `json:"revision"`
	// CreatedBy is the owning user's ID from ports.ActorIDFromContext at
	// creation; immutable afterwards.
	CreatedBy string `json:"createdBy,omitempty"`
}

// CreateHandoff inserts an immutable handoff snapshot.
func (s *Store) CreateHandoff(ctx context.Context, handoff domain.Handoff) (HandoffRecord, domain.SyncRevision, error) {
	if handoff.ID == "" {
		id, err := newID("handoff")
		if err != nil {
			return HandoffRecord{}, domain.SyncRevision{}, err
		}
		handoff.ID = id
	}
	if err := handoff.Validate(); err != nil {
		return HandoffRecord{}, domain.SyncRevision{}, err
	}

	at := s.nowUTC()
	if handoff.CreatedAt.IsZero() {
		handoff.CreatedAt = at
	} else {
		handoff.CreatedAt = databaseTime(handoff.CreatedAt)
	}
	actor := ActorFromContext(ctx)
	record := HandoffRecord{Handoff: handoff, CreatedBy: ports.ActorIDFromContext(ctx)}
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "handoff", handoff.ID, nil, at)
		if err != nil {
			return err
		}
		record.Revision = revisionID
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO handoffs
			 (id, project_id, summary, git_context, next_action, permalink, revision, created_at, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			handoff.ID, handoff.ProjectID, handoff.Summary, handoff.GitContext,
			handoff.NextAction, handoff.Permalink, revisionID, handoff.CreatedAt, record.CreatedBy,
		); err != nil {
			return mapPgError("create handoff", err)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "handoff.create",
			EntityKind: "handoff", EntityID: handoff.ID, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return HandoffRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// AppendHandoff implements ports.Store by creating a handoff.
func (s *Store) AppendHandoff(ctx context.Context, handoff domain.Handoff) (domain.SyncRevision, error) {
	_, revision, err := s.CreateHandoff(ctx, handoff)
	return revision, err
}

// GetHandoff loads one handoff.
func (s *Store) GetHandoff(ctx context.Context, id string) (HandoffRecord, error) {
	if strings.TrimSpace(id) == "" {
		return HandoffRecord{}, domain.NewValidationError("handoff.id", "is required")
	}
	return getHandoff(ctx, s.pool, id)
}

// ListHandoffs returns handoffs, newest first, optionally filtered by project.
func (s *Store) ListHandoffs(ctx context.Context, opts ListOptions) ([]HandoffRecord, error) {
	query, args := knowledgeListQuery("handoffs", handoffColumns, "created_at", opts)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapPgError("list handoffs", err)
	}
	return collectRows(rows, scanHandoff)
}

// DeleteHandoff removes a handoff with an optional optimistic revision check.
func (s *Store) DeleteHandoff(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("handoff.id", "is required")
	}
	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "handoff", id, nil, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM handoffs WHERE id = $1 AND ($2 = 0 OR revision = $2)`, id, expected)
		if err != nil {
			return mapPgError("delete handoff", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "handoffs", id, expected)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "handoff.delete",
			EntityKind: "handoff", EntityID: id, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return domain.SyncRevision{}, err
	}
	return revision, nil
}

func getHandoff(ctx context.Context, q dbQuerier, id string) (HandoffRecord, error) {
	record, err := scanHandoff(q.QueryRow(ctx, `SELECT `+handoffColumns+` FROM handoffs WHERE id = $1`, id))
	if err != nil {
		return HandoffRecord{}, mapPgError("get handoff", err)
	}
	return record, nil
}

func scanHandoff(row rowScanner) (HandoffRecord, error) {
	var record HandoffRecord
	if err := row.Scan(
		&record.ID, &record.ProjectID, &record.Summary, &record.GitContext,
		&record.NextAction, &record.Permalink, &record.Revision, &record.CreatedAt, &record.CreatedBy,
	); err != nil {
		return HandoffRecord{}, err
	}
	record.CreatedAt = record.CreatedAt.UTC()
	return record, nil
}
