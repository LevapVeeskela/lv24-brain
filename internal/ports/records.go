package ports

import (
	"context"
	"time"

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

// RecordReviewer is implemented by stores that can change the review state of
// one stored record (local and git mode). It is optional for the same reason
// as RecordEditor: server clients have no provenance-edit route yet. trust
// empty keeps the stored trust level. changed is false for an idempotent
// repeat of a review that changes nothing.
type RecordReviewer interface {
	ReviewRecord(ctx context.Context, projectID, kind, id string, trust domain.Trust) (changed bool, err error)
}

// RecordScheduler is implemented by stores that can move the review date of one
// stored record without touching its text. It is optional like RecordReviewer:
// a store without it reports that scheduling is unavailable. A zero date clears
// review_after; reviewed_at records that the review happened.
type RecordScheduler interface {
	SetReviewAfter(ctx context.Context, projectID, kind, id string, at time.Time) (changed bool, err error)
}

// RecordTextUpdate is one in-place text edit of a stored record. Exactly one
// record pointer is set; its ID, kind and project identify the stored file.
// ExpectedUpdatedAt is the revision the caller read: a non-zero value makes the
// edit a compare-and-swap and a mismatch returns a
// domain.RevisionConflictError without writing. The store keeps the ID, file
// name, created_at, provenance, validity, relations, review state and topic
// classification; the caller supplies the new text and bounded history.
type RecordTextUpdate struct {
	Note     *domain.Note
	Decision *domain.Decision
	Attempt  *domain.Attempt
	Handoff  *domain.Handoff
	// ExpectedUpdatedAt is zero for a last-write-wins edit.
	ExpectedUpdatedAt time.Time
}

// RecordTextUpdater is implemented by stores that can rewrite the text of one
// existing record under a compare-and-swap. It is optional like the other
// single-record editors: a store without it reports that editing is
// unavailable.
type RecordTextUpdater interface {
	UpdateRecordText(ctx context.Context, update RecordTextUpdate) error
}

// RecordValiditySetter is implemented by stores that can rewrite the validity
// fields (valid_from, valid_to, superseded_by, supersedes, confidence) of one
// stored record without changing its text. It is optional: the supersede
// command reports that the feature is unavailable for a store that lacks it.
type RecordValiditySetter interface {
	SetRecordValidity(ctx context.Context, projectID, kind, id string, validity domain.Validity) error
}

// RecordPinner is implemented by stores that can set or clear the pinned flag of
// one stored record without changing its text. It is optional: a store without
// it reports that pinning is unavailable.
type RecordPinner interface {
	SetRecordPinned(ctx context.Context, projectID, kind, id string, pinned bool) (bool, error)
}

// TrashedRecord identifies one record in a project's trash.
type TrashedRecord struct {
	Kind  string
	ID    string
	Title string
}
