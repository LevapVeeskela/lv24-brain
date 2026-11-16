package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// UpdateRecordProvenance rewrites only the provenance fields of one record and
// bumps its revision. The body and every other field are left untouched.
func (s *Store) UpdateRecordProvenance(ctx context.Context, kind, id string, provenance domain.Provenance, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("record.id", "is required")
	}
	table, ok := recordProvenanceTable(kind)
	if !ok {
		return domain.SyncRevision{}, domain.NewValidationError("kind", fmt.Sprintf("unknown record kind %q", kind))
	}
	if err := provenance.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	provenance = provenance.Normalized()

	at := s.nowUTC()
	actor := ActorFromContext(ctx)
	var revision domain.SyncRevision
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		projectID, err := entityProjectTx(ctx, tx, table, id)
		if err != nil {
			return err
		}
		revisionID, err := insertRevisionTx(ctx, tx, actor, kind, id, projectDetail(projectID, nil), at)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(
			ctx,
			`UPDATE `+pgx.Identifier{table}.Sanitize()+`
			 SET origin = $1, reviewed = $2, trust = $3, revision = $4
			 WHERE id = $5 AND ($6 = 0 OR revision = $6)`,
			provenance.Origin, provenance.Reviewed, provenance.Trust, revisionID, id, expected,
		)
		if err != nil {
			return mapPgError("update record provenance", err)
		}
		if tag.RowsAffected() == 0 {
			return revisionConflict(ctx, tx, table, id, expected)
		}
		if err := insertAuditTx(ctx, tx, auditEntry{
			Revision: revisionID, Actor: actor, Action: kind + ".review",
			EntityKind: kind, EntityID: id, At: at,
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

func recordProvenanceTable(kind string) (string, bool) {
	switch kind {
	case ports.RecordNote:
		return "notes", true
	case ports.RecordDecision:
		return "decisions", true
	case ports.RecordAttempt:
		return "attempts", true
	case ports.RecordHandoff:
		return "handoffs", true
	default:
		return "", false
	}
}
