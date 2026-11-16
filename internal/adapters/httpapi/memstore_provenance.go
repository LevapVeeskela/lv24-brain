package httpapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// UpdateRecordProvenance rewrites only the provenance fields of one record and
// bumps its revision. The body and every other field are left untouched.
func (m *MemoryStore) UpdateRecordProvenance(ctx context.Context, kind, id string, provenance domain.Provenance, expected int64) (domain.SyncRevision, error) {
	if strings.TrimSpace(id) == "" {
		return domain.SyncRevision{}, domain.NewValidationError("record.id", "is required")
	}
	if _, ok := recordMemoryKind(kind); !ok {
		return domain.SyncRevision{}, domain.NewValidationError("kind", fmt.Sprintf("unknown record kind %q", kind))
	}
	if err := provenance.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	provenance = provenance.Normalized()
	m.mu.Lock()
	defer m.mu.Unlock()
	switch kind {
	case ports.RecordNote:
		current, ok := m.notes[id]
		if !ok {
			return domain.SyncRevision{}, domain.ErrNotFound
		}
		if expected > 0 && current.Revision != expected {
			return domain.SyncRevision{}, &ConflictError{Entity: "note", ID: id, Expected: expected, Current: current.Revision}
		}
		revision := m.nextRevisionLocked(ctx, "note", id, "review", projectDetail(current.ProjectID, nil))
		current.Provenance = provenance
		current.Revision = revisionValue(revision)
		m.notes[id] = current
		return revision, nil
	case ports.RecordDecision:
		current, ok := m.decisions[id]
		if !ok {
			return domain.SyncRevision{}, domain.ErrNotFound
		}
		if expected > 0 && current.Revision != expected {
			return domain.SyncRevision{}, &ConflictError{Entity: "decision", ID: id, Expected: expected, Current: current.Revision}
		}
		revision := m.nextRevisionLocked(ctx, "decision", id, "review", projectDetail(current.ProjectID, nil))
		current.Provenance = provenance
		current.Revision = revisionValue(revision)
		m.decisions[id] = current
		return revision, nil
	case ports.RecordAttempt:
		current, ok := m.attempts[id]
		if !ok {
			return domain.SyncRevision{}, domain.ErrNotFound
		}
		if expected > 0 && current.Revision != expected {
			return domain.SyncRevision{}, &ConflictError{Entity: "attempt", ID: id, Expected: expected, Current: current.Revision}
		}
		revision := m.nextRevisionLocked(ctx, "attempt", id, "review", projectDetail(current.ProjectID, nil))
		current.Provenance = provenance
		current.Revision = revisionValue(revision)
		m.attempts[id] = current
		return revision, nil
	case ports.RecordHandoff:
		current, ok := m.handoffs[id]
		if !ok {
			return domain.SyncRevision{}, domain.ErrNotFound
		}
		if expected > 0 && current.Revision != expected {
			return domain.SyncRevision{}, &ConflictError{Entity: "handoff", ID: id, Expected: expected, Current: current.Revision}
		}
		revision := m.nextRevisionLocked(ctx, "handoff", id, "review", projectDetail(current.ProjectID, nil))
		current.Provenance = provenance
		current.Revision = revisionValue(revision)
		m.handoffs[id] = current
		return revision, nil
	}
	return domain.SyncRevision{}, domain.ErrNotFound
}

func recordMemoryKind(kind string) (string, bool) {
	switch kind {
	case ports.RecordNote, ports.RecordDecision, ports.RecordAttempt, ports.RecordHandoff:
		return kind, true
	default:
		return "", false
	}
}
