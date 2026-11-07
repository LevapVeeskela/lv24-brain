package ports

import (
	"context"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Record kinds handled by RecordEditor.
const (
	RecordNote     = "note"
	RecordDecision = "decision"
	RecordAttempt  = "attempt"
	RecordHandoff  = "handoff"
)

// DecisionUpdater is implemented by stores that can rewrite a stored
// decision, including server clients (status and supersede changes).
type DecisionUpdater interface {
	// UpdateDecision rewrites the stored decision with the same ID and
	// project. ErrNotFound when no such decision exists.
	UpdateDecision(ctx context.Context, decision domain.Decision) error
}

// RecordEditor is implemented by stores that can change or retract single
// records (local and git mode). It is optional so server clients keep
// satisfying Store.
type RecordEditor interface {
	DecisionUpdater
	// TrashRecord moves one record (kind: note, decision, attempt, handoff)
	// out of the project's listings into its trash, where RestoreRecord can
	// bring it back. ErrNotFound when no such record exists.
	TrashRecord(ctx context.Context, projectID, kind, id string) error
	// RestoreRecord moves a trashed record back. ErrNotFound when the trash
	// has no such record; ErrConflict when a live record already uses the ID.
	RestoreRecord(ctx context.Context, projectID, kind, id string) error
	// ListTrash lists trashed records of a project.
	ListTrash(ctx context.Context, projectID string) ([]TrashedRecord, error)
}

// TrashedRecord identifies one record in a project's trash.
type TrashedRecord struct {
	Kind  string
	ID    string
	Title string
}
