package postgres

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const decisionColumns = `id, project_id, title, status, decision_context, alternatives, consequences, evidence, permalink, revision, created_at, updated_at, created_by`

// DecisionRecord is a decision plus its current server revision.
type DecisionRecord struct {
	domain.Decision
	Revision int64 `json:"revision"`
	// CreatedBy is the owning user's ID from ports.ActorIDFromContext at
	// creation; immutable afterwards.
	CreatedBy string `json:"createdBy,omitempty"`
}

// CreateDecision inserts a decision. An empty status defaults to proposed.
func (s *Store) CreateDecision(ctx context.Context, decision domain.Decision) (DecisionRecord, domain.SyncRevision, error) {
	if decision.ID == "" {
		id, err := newID("decision")
		if err != nil {
			return DecisionRecord{}, domain.SyncRevision{}, err
		}
		decision.ID = id
	}
	if decision.Status == "" {
		decision.Status = domain.DecisionProposed
	}
	if err := decision.Validate(); err != nil {
		return DecisionRecord{}, domain.SyncRevision{}, err
	}
	decision.Alternatives = nonNilStrings(decision.Alternatives)
	decision.Consequences = nonNilStrings(decision.Consequences)
	decision.Evidence = nonNilStrings(decision.Evidence)

	at := s.nowUTC()
	if decision.CreatedAt.IsZero() {
		decision.CreatedAt = at
	} else {
		decision.CreatedAt = databaseTime(decision.CreatedAt)
	}
	decision.UpdatedAt = at
	actor := ActorFromContext(ctx)
	record := DecisionRecord{Decision: decision, CreatedBy: ports.ActorIDFromContext(ctx)}
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "decision", decision.ID, projectDetail(decision.ProjectID, map[string]any{"status": string(decision.Status)}), at)
		if err != nil {
			return err
		}
		record.Revision = revisionID
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO decisions
			 (id, project_id, title, status, decision_context, alternatives, consequences, evidence, permalink, revision, created_at, updated_at, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			decision.ID, decision.ProjectID, decision.Title, string(decision.Status), decision.Context,
			decision.Alternatives, decision.Consequences, decision.Evidence, decision.Permalink,
			revisionID, decision.CreatedAt, decision.UpdatedAt, record.CreatedBy,
		); err != nil {
			return mapPgError("create decision", err)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "decision.create",
			EntityKind: "decision", EntityID: decision.ID,
			Detail: map[string]any{"status": string(decision.Status)}, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return DecisionRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// AppendDecision implements ports.Store by creating a decision.
func (s *Store) AppendDecision(ctx context.Context, decision domain.Decision) (domain.SyncRevision, error) {
	_, revision, err := s.CreateDecision(ctx, decision)
	return revision, err
}

// GetDecision loads one decision.
func (s *Store) GetDecision(ctx context.Context, id string) (DecisionRecord, error) {
	if strings.TrimSpace(id) == "" {
		return DecisionRecord{}, domain.NewValidationError("decision.id", "is required")
	}
	return getDecision(ctx, s.pool, id)
}

// ListDecisions returns decisions, newest first, optionally filtered by
// project.
func (s *Store) ListDecisions(ctx context.Context, opts ListOptions) ([]DecisionRecord, error) {
	query, args := knowledgeListQuery("decisions", decisionColumns, "updated_at", opts)
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapPgError("list decisions", err)
	}
	return collectRows(rows, scanDecision)
}

// UpdateDecision replaces the decision title, status, context, alternatives,
// consequences, evidence and permalink. project_id is immutable. expected is
// the caller's last seen revision; zero skips the optimistic check.
func (s *Store) UpdateDecision(ctx context.Context, decision domain.Decision, expected int64) (DecisionRecord, domain.SyncRevision, error) {
	if strings.TrimSpace(decision.ID) == "" {
		return DecisionRecord{}, domain.SyncRevision{}, domain.NewValidationError("decision.id", "is required")
	}
	if strings.TrimSpace(decision.Title) == "" {
		return DecisionRecord{}, domain.SyncRevision{}, domain.NewValidationError("decision.title", "is required")
	}
	if decision.Status == "" {
		decision.Status = domain.DecisionProposed
	}
	if !decision.Status.Valid() {
		return DecisionRecord{}, domain.SyncRevision{}, domain.NewValidationError("decision.status", "unknown status")
	}
	decision.Alternatives = nonNilStrings(decision.Alternatives)
	decision.Consequences = nonNilStrings(decision.Consequences)
	decision.Evidence = nonNilStrings(decision.Evidence)

	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var record DecisionRecord
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		projectID, err := entityProjectTx(ctx, tx, "decisions", decision.ID)
		if err != nil {
			return err
		}
		revisionID, err := insertRevisionTx(ctx, tx, actor, "decision", decision.ID, projectDetail(projectID, map[string]any{"status": string(decision.Status)}), at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(
			ctx,
			`UPDATE decisions
			 SET title = $1, status = $2, decision_context = $3, alternatives = $4, consequences = $5,
			     evidence = $6, permalink = $7, revision = $8, updated_at = $9
			 WHERE id = $10 AND ($11 = 0 OR revision = $11)`,
			decision.Title, string(decision.Status), decision.Context, decision.Alternatives,
			decision.Consequences, decision.Evidence, decision.Permalink, revisionID, at,
			decision.ID, expected,
		)
		if err != nil {
			return mapPgError("update decision", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "decisions", decision.ID, expected)
		}
		updated, err := getDecision(ctx, tx, decision.ID)
		if err != nil {
			return err
		}
		record = updated
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "decision.update",
			EntityKind: "decision", EntityID: decision.ID,
			Detail: map[string]any{"status": string(decision.Status)}, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return DecisionRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// DeleteDecision removes a decision with an optional optimistic revision check.
func (s *Store) DeleteDecision(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("decision.id", "is required")
	}
	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		projectID, err := entityProjectTx(ctx, tx, "decisions", id)
		if err != nil {
			return err
		}
		revisionID, err := insertRevisionTx(ctx, tx, actor, "decision", id, projectDetail(projectID, nil), at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM decisions WHERE id = $1 AND ($2 = 0 OR revision = $2)`, id, expected)
		if err != nil {
			return mapPgError("delete decision", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "decisions", id, expected)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "decision.delete",
			EntityKind: "decision", EntityID: id, At: at,
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

func getDecision(ctx context.Context, q dbQuerier, id string) (DecisionRecord, error) {
	record, err := scanDecision(q.QueryRow(ctx, `SELECT `+decisionColumns+` FROM decisions WHERE id = $1`, id))
	if err != nil {
		return DecisionRecord{}, mapPgError("get decision", err)
	}
	return record, nil
}

func scanDecision(row rowScanner) (DecisionRecord, error) {
	var record DecisionRecord
	var status string
	if err := row.Scan(
		&record.ID, &record.ProjectID, &record.Title, &status, &record.Context,
		&record.Alternatives, &record.Consequences, &record.Evidence, &record.Permalink,
		&record.Revision, &record.CreatedAt, &record.UpdatedAt, &record.CreatedBy,
	); err != nil {
		return DecisionRecord{}, err
	}
	record.Status = domain.DecisionStatus(status)
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	return record, nil
}
