package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const connectorRunColumns = `id, source, started_at, finished_at, status, counts, error_message, revision, connection_id, stats`

// ConnectorRunRecord is a connector run plus its current server revision.
type ConnectorRunRecord struct {
	domain.ConnectorRun
	Revision int64 `json:"revision"`
}

// ConnectorRunFilter narrows a connector run listing.
type ConnectorRunFilter struct {
	Source string
	Status domain.ConnectorStatus
	Limit  int
	Offset int
}

// CreateConnectorRun starts a run record. An empty status defaults to running
// and an empty start time defaults to now.
func (s *Store) CreateConnectorRun(ctx context.Context, run domain.ConnectorRun) (ConnectorRunRecord, domain.SyncRevision, error) {
	if run.ID == "" {
		id, err := newID("run")
		if err != nil {
			return ConnectorRunRecord{}, domain.SyncRevision{}, err
		}
		run.ID = id
	}
	if run.Status == "" {
		run.Status = domain.ConnectorRunning
	}
	if err := run.Validate(); err != nil {
		return ConnectorRunRecord{}, domain.SyncRevision{}, err
	}
	if run.Counts == nil {
		run.Counts = map[string]int{}
	}

	at := s.nowUTC()
	if run.StartedAt.IsZero() {
		run.StartedAt = at
	} else {
		run.StartedAt = databaseTime(run.StartedAt)
	}
	if run.FinishedAt != nil {
		finished := databaseTime(*run.FinishedAt)
		run.FinishedAt = &finished
	}
	actor := ActorFromContext(ctx)
	counts, err := marshalJSON(run.Counts)
	if err != nil {
		return ConnectorRunRecord{}, domain.SyncRevision{}, err
	}
	stats, err := marshalRunStats(run.Stats)
	if err != nil {
		return ConnectorRunRecord{}, domain.SyncRevision{}, err
	}
	record := ConnectorRunRecord{ConnectorRun: run}
	var revision domain.SyncRevision

	err = s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "connector_run", run.ID, map[string]any{"status": string(run.Status)}, at)
		if err != nil {
			return err
		}
		record.Revision = revisionID
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO connector_runs
			 (id, source, started_at, finished_at, status, counts, error_message, revision, connection_id, stats)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10)`,
			run.ID, run.Source, run.StartedAt, run.FinishedAt, string(run.Status), counts, run.Error, revisionID,
			run.ConnectionID, stats,
		); err != nil {
			return mapPgError("create connector run", err)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "connector_run.start",
			EntityKind: "connector_run", EntityID: run.ID,
			Detail: map[string]any{"status": string(run.Status)}, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return ConnectorRunRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// FinishConnectorRun records the final status, counts and error of a run.
// expected is the caller's last seen revision; zero skips the optimistic check.
func (s *Store) FinishConnectorRun(ctx context.Context, id string, status domain.ConnectorStatus, counts map[string]int, errorMessage string, expected int64) (ConnectorRunRecord, domain.SyncRevision, error) {
	return s.FinishConnectorRunStats(ctx, id, status, counts, nil, errorMessage, expected)
}

// FinishConnectorRunStats is FinishConnectorRun for item-level runs: a
// non-nil stats replaces the stored RunStats, nil keeps them unchanged.
func (s *Store) FinishConnectorRunStats(ctx context.Context, id string, status domain.ConnectorStatus, counts map[string]int, stats *domain.RunStats, errorMessage string, expected int64) (ConnectorRunRecord, domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return ConnectorRunRecord{}, domain.SyncRevision{}, domain.NewValidationError("connectorRun.id", "is required")
	}
	if !status.Valid() {
		return ConnectorRunRecord{}, domain.SyncRevision{}, domain.NewValidationError("connectorRun.status", "unknown status")
	}
	if counts == nil {
		counts = map[string]int{}
	}
	encodedCounts, err := marshalJSON(counts)
	if err != nil {
		return ConnectorRunRecord{}, domain.SyncRevision{}, err
	}
	if stats != nil {
		if err := stats.Validate(); err != nil {
			return ConnectorRunRecord{}, domain.SyncRevision{}, err
		}
	}
	encodedStats, err := marshalRunStats(stats)
	if err != nil {
		return ConnectorRunRecord{}, domain.SyncRevision{}, err
	}

	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var record ConnectorRunRecord
	var revision domain.SyncRevision

	err = s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "connector_run", id, map[string]any{"status": string(status)}, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(
			ctx,
			`UPDATE connector_runs
			 SET finished_at = $1, status = $2, counts = $3, error_message = $4, revision = $5,
			     stats = coalesce($8::jsonb, stats)
			 WHERE id = $6 AND ($7 = 0 OR revision = $7)`,
			at, string(status), encodedCounts, errorMessage, revisionID, id, expected, encodedStats,
		)
		if err != nil {
			return mapPgError("finish connector run", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "connector_runs", id, expected)
		}
		updated, err := getConnectorRun(ctx, tx, id)
		if err != nil {
			return err
		}
		record = updated
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "connector_run.finish",
			EntityKind: "connector_run", EntityID: id,
			Detail: map[string]any{"status": string(status)}, At: at,
		}); err != nil {
			return err
		}
		revision = RevisionFromValue(revisionID, at)
		return nil
	})
	if err != nil {
		return ConnectorRunRecord{}, domain.SyncRevision{}, err
	}
	return record, revision, nil
}

// GetConnectorRun loads one connector run.
func (s *Store) GetConnectorRun(ctx context.Context, id string) (ConnectorRunRecord, error) {
	if strings.TrimSpace(id) == "" {
		return ConnectorRunRecord{}, domain.NewValidationError("connectorRun.id", "is required")
	}
	return getConnectorRun(ctx, s.pool, id)
}

// ListConnectorRuns returns runs, newest first, with optional source and status
// filters.
func (s *Store) ListConnectorRuns(ctx context.Context, filter ConnectorRunFilter) ([]ConnectorRunRecord, error) {
	query := "SELECT " + connectorRunColumns + " FROM connector_runs"
	args := make([]any, 0, 4)
	conditions := make([]string, 0, 2)
	if filter.Source != "" {
		args = append(args, filter.Source)
		conditions = append(conditions, fmt.Sprintf("source = $%d", len(args)))
	}
	if filter.Status != "" {
		if !filter.Status.Valid() {
			return nil, domain.NewValidationError("connectorRun.status", "unknown status")
		}
		args = append(args, string(filter.Status))
		conditions = append(conditions, fmt.Sprintf("status = $%d", len(args)))
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	args = append(args, clampLimit(filter.Limit))
	limitPlaceholder := len(args)
	args = append(args, normalizeOffset(filter.Offset))
	offsetPlaceholder := len(args)
	query += fmt.Sprintf(" ORDER BY started_at DESC, id LIMIT $%d OFFSET $%d", limitPlaceholder, offsetPlaceholder)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, mapPgError("list connector runs", err)
	}
	return collectRows(rows, scanConnectorRun)
}

// DeleteConnectorRun removes a run with an optional optimistic revision check.
func (s *Store) DeleteConnectorRun(ctx context.Context, id string, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("connectorRun.id", "is required")
	}
	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var revision domain.SyncRevision

	err := s.withTx(ctx, func(tx pgx.Tx) error {
		revisionID, err := insertRevisionTx(ctx, tx, actor, "connector_run", id, nil, at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM connector_runs WHERE id = $1 AND ($2 = 0 OR revision = $2)`, id, expected)
		if err != nil {
			return mapPgError("delete connector run", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, "connector_runs", id, expected)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: "connector_run.delete",
			EntityKind: "connector_run", EntityID: id, At: at,
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

// marshalRunStats encodes stats as jsonb, or SQL NULL when stats is nil.
func marshalRunStats(stats *domain.RunStats) (any, error) {
	if stats == nil {
		return nil, nil
	}
	raw, err := json.Marshal(stats)
	if err != nil {
		return nil, fmt.Errorf("encode connector run stats: %w", err)
	}
	return raw, nil
}

func getConnectorRun(ctx context.Context, q dbQuerier, id string) (ConnectorRunRecord, error) {
	record, err := scanConnectorRun(q.QueryRow(ctx, `SELECT `+connectorRunColumns+` FROM connector_runs WHERE id = $1`, id))
	if err != nil {
		return ConnectorRunRecord{}, mapPgError("get connector run", err)
	}
	return record, nil
}

func scanConnectorRun(row rowScanner) (ConnectorRunRecord, error) {
	var record ConnectorRunRecord
	var status string
	var finishedAt *time.Time
	var counts, stats []byte
	var connectionID *string
	if err := row.Scan(
		&record.ID, &record.Source, &record.StartedAt, &finishedAt, &status,
		&counts, &record.Error, &record.Revision, &connectionID, &stats,
	); err != nil {
		return ConnectorRunRecord{}, err
	}
	if connectionID != nil {
		record.ConnectionID = *connectionID
	}
	if len(stats) > 0 && string(stats) != "null" {
		var decoded domain.RunStats
		if err := json.Unmarshal(stats, &decoded); err != nil {
			return ConnectorRunRecord{}, fmt.Errorf("decode connector run stats: %w", err)
		}
		record.Stats = &decoded
	}
	record.Status = domain.ConnectorStatus(status)
	decodedCounts, err := unmarshalJSON(counts, map[string]int{})
	if err != nil {
		return ConnectorRunRecord{}, err
	}
	record.Counts = decodedCounts
	if finishedAt != nil {
		finished := finishedAt.UTC()
		record.FinishedAt = &finished
	}
	record.StartedAt = record.StartedAt.UTC()
	return record, nil
}
