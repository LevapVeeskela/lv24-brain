package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const stateColumns = `project_id, goal, active, blocked, next_action, open_questions, fresh_until, revision, updated_at`

// StateRecord is project state plus its current server revision.
type StateRecord struct {
	domain.State
	Revision int64 `json:"revision"`
}

// GetState loads project state for the ports.Store contract.
func (s *Store) GetState(ctx context.Context, projectID string) (domain.State, error) {
	record, err := s.GetStateRecord(ctx, projectID)
	if err != nil {
		return domain.State{}, err
	}
	return record.State, nil
}

// GetStateRecord loads project state together with its revision.
func (s *Store) GetStateRecord(ctx context.Context, projectID string) (StateRecord, error) {
	if strings.TrimSpace(projectID) == "" {
		return StateRecord{}, domain.NewValidationError("state.projectId", "is required")
	}
	return getState(ctx, s.pool, projectID)
}

// PutState replaces project state for the ports.Store contract. It is an
// unconditional upsert; use PutStateAt for optimistic concurrency.
func (s *Store) PutState(ctx context.Context, state domain.State) (domain.SyncRevision, error) {
	_, revision, err := s.PutStateAt(ctx, state, 0)
	if err != nil {
		return domain.SyncRevision{}, err
	}
	return revision, nil
}

// PutStateAt replaces project state. expected is the caller's last seen
// revision; zero skips the optimistic check.
func (s *Store) PutStateAt(ctx context.Context, state domain.State, expected int64) (StateRecord, domain.SyncRevision, error) {
	if strings.TrimSpace(state.ProjectID) == "" {
		return StateRecord{}, domain.SyncRevision{}, domain.NewValidationError("state.projectId", "is required")
	}
	if err := state.Validate(); err != nil {
		return StateRecord{}, domain.SyncRevision{}, err
	}
	state.Active = nonNilStrings(state.Active)
	state.Blocked = nonNilStrings(state.Blocked)
	state.OpenQuestions = nonNilStrings(state.OpenQuestions)

	at := s.nowUTC()
	state.UpdatedAt = at
	var freshUntil *time.Time
	if !state.FreshUntil.IsZero() {
		fresh := databaseTime(state.FreshUntil)
		freshUntil = &fresh
	}
	actor := ActorFromContext(ctx)
	var record StateRecord
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "project_state", state.ProjectID, nil, at)
		if err != nil {
			return err
		}
		if expected > 0 {
			tag, err := tx.Exec(
				ctx,
				`UPDATE project_state
				 SET goal = $1, active = $2, blocked = $3, next_action = $4, open_questions = $5,
				     fresh_until = $6, revision = $7, updated_at = $8
				 WHERE project_id = $9 AND revision = $10`,
				state.Goal, state.Active, state.Blocked, state.NextAction, state.OpenQuestions,
				freshUntil, revisionID, at, state.ProjectID, expected,
			)
			if err != nil {
				return mapPgError("update project state", err)
			}
			if tag.RowsAffected() == 0 {
				return revisionConflict(ctx, tx, "project_state", state.ProjectID, expected)
			}
		} else {
			if _, err := tx.Exec(
				ctx,
				`INSERT INTO project_state
				 (project_id, goal, active, blocked, next_action, open_questions, fresh_until, revision, updated_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				 ON CONFLICT (project_id) DO UPDATE SET
				     goal = EXCLUDED.goal,
				     active = EXCLUDED.active,
				     blocked = EXCLUDED.blocked,
				     next_action = EXCLUDED.next_action,
				     open_questions = EXCLUDED.open_questions,
				     fresh_until = EXCLUDED.fresh_until,
				     revision = EXCLUDED.revision,
				     updated_at = EXCLUDED.updated_at`,
				state.ProjectID, state.Goal, state.Active, state.Blocked, state.NextAction,
				state.OpenQuestions, freshUntil, revisionID, at,
			); err != nil {
				return mapPgError("put project state", err)
			}
		}
		updated, err := getState(ctx, tx, state.ProjectID)
		if err != nil {
			return err
		}
		record = updated
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "state.put",
			EntityKind: "project_state", EntityID: state.ProjectID, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return StateRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

func getState(ctx context.Context, q dbQuerier, projectID string) (StateRecord, error) {
	record, err := scanState(q.QueryRow(ctx, `SELECT `+stateColumns+` FROM project_state WHERE project_id = $1`, projectID))
	if err != nil {
		return StateRecord{}, mapPgError("get project state", err)
	}
	return record, nil
}

func scanState(row rowScanner) (StateRecord, error) {
	var record StateRecord
	var freshUntil *time.Time
	if err := row.Scan(
		&record.ProjectID, &record.Goal, &record.Active, &record.Blocked, &record.NextAction,
		&record.OpenQuestions, &freshUntil, &record.Revision, &record.UpdatedAt,
	); err != nil {
		return StateRecord{}, err
	}
	if freshUntil != nil {
		record.FreshUntil = freshUntil.UTC()
	}
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}
