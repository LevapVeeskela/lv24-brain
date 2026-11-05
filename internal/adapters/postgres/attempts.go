package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const attemptColumns = `id, project_id, title, hypothesis, result, reason, permalink, revision, created_at, updated_at`

// AttemptRecord is an attempt plus its current server revision.
type AttemptRecord struct {
	domain.Attempt
	Revision int64 `json:"revision"`
}

// CreateAttempt inserts an attempt record.
func (s *Store) CreateAttempt(ctx context.Context, attempt domain.Attempt) (AttemptRecord, domain.SyncRevision, error) {
	if attempt.ID == "" {
		id, err := newID("attempt")
		if err != nil {
			return AttemptRecord{}, domain.SyncRevision{}, err
		}
		attempt.ID = id
	}
	if err := attempt.Validate(); err != nil {
		return AttemptRecord{}, domain.SyncRevision{}, err
	}

	at := s.nowUTC()
	if attempt.CreatedAt.IsZero() {
		attempt.CreatedAt = at
	} else {
		attempt.CreatedAt = databaseTime(attempt.CreatedAt)
	}
	attempt.UpdatedAt = at
	actor := ActorFromContext(ctx)
	record := AttemptRecord{Attempt: attempt}
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "attempt", attempt.ID, nil, at)
		if err != nil {
			return err
		}
		record.Revision = revisionID
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO attempts
			 (id, project_id, title, hypothesis, result, reason, permalink, revision, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			attempt.ID, attempt.ProjectID, attempt.Title, attempt.Hypothesis, attempt.Result,
			attempt.Reason, attempt.Permalink, revisionID, attempt.CreatedAt, attempt.UpdatedAt,
		); err != nil {
			return mapPgError("create attempt", err)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "attempt.create",
			EntityKind: "attempt", EntityID: attempt.ID, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return AttemptRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// AppendAttempt implements ports.Store by creating an attempt.
func (s *Store) AppendAttempt(ctx context.Context, attempt domain.Attempt) (domain.SyncRevision, error) {
	_, revision, err := s.CreateAttempt(ctx, attempt)
	return revision, err
}

// GetAttempt loads one attempt.
func (s *Store) GetAttempt(ctx context.Context, id string) (AttemptRecord, error) {
	if strings.TrimSpace(id) == "" {
		return AttemptRecord{}, domain.NewValidationError("attempt.id", "is required")
	}
	return getAttempt(ctx, s.pool, id)
}

// ListAttempts returns attempts, newest first, optionally filtered by project.
func (s *Store) ListAttempts(ctx context.Context, opts ListOptions) ([]AttemptRecord, error) {
	query, args := knowledgeListQuery("attempts", attemptColumns, "updated_at", opts)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapPgError("list attempts", err)
	}
	return collectRows(rows, scanAttempt)
}

// UpdateAttempt replaces the attempt title, hypothesis, result, reason and
// permalink. project_id is immutable. expected is the caller's last seen
// revision; zero skips the optimistic check.
func (s *Store) UpdateAttempt(ctx context.Context, attempt domain.Attempt, expected int64) (AttemptRecord, domain.SyncRevision, error) {
	if strings.TrimSpace(attempt.ID) == "" {
		return AttemptRecord{}, domain.SyncRevision{}, domain.NewValidationError("attempt.id", "is required")
	}
	if strings.TrimSpace(attempt.Title) == "" {
		return AttemptRecord{}, domain.SyncRevision{}, domain.NewValidationError("attempt.title", "is required")
	}

	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var record AttemptRecord
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "attempt", attempt.ID, nil, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(
			ctx,
			`UPDATE attempts
			 SET title = $1, hypothesis = $2, result = $3, reason = $4, permalink = $5,
			     revision = $6, updated_at = $7
			 WHERE id = $8 AND ($9 = 0 OR revision = $9)`,
			attempt.Title, attempt.Hypothesis, attempt.Result, attempt.Reason, attempt.Permalink,
			revisionID, at, attempt.ID, expected,
		)
		if err != nil {
			return mapPgError("update attempt", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "attempts", attempt.ID, expected)
		}
		updated, err := getAttempt(ctx, tx, attempt.ID)
		if err != nil {
			return err
		}
		record = updated
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "attempt.update",
			EntityKind: "attempt", EntityID: attempt.ID, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return AttemptRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// DeleteAttempt removes an attempt with an optional optimistic revision check.
func (s *Store) DeleteAttempt(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("attempt.id", "is required")
	}
	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "attempt", id, nil, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM attempts WHERE id = $1 AND ($2 = 0 OR revision = $2)`, id, expected)
		if err != nil {
			return mapPgError("delete attempt", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "attempts", id, expected)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "attempt.delete",
			EntityKind: "attempt", EntityID: id, At: at,
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

func getAttempt(ctx context.Context, q dbQuerier, id string) (AttemptRecord, error) {
	record, err := scanAttempt(q.QueryRow(ctx, `SELECT `+attemptColumns+` FROM attempts WHERE id = $1`, id))
	if err != nil {
		return AttemptRecord{}, mapPgError("get attempt", err)
	}
	return record, nil
}

func scanAttempt(row rowScanner) (AttemptRecord, error) {
	var record AttemptRecord
	if err := row.Scan(
		&record.ID, &record.ProjectID, &record.Title, &record.Hypothesis, &record.Result,
		&record.Reason, &record.Permalink, &record.Revision, &record.CreatedAt, &record.UpdatedAt,
	); err != nil {
		return AttemptRecord{}, err
	}
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}
