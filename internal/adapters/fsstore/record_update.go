package fsstore

import (
	"context"
	"fmt"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var _ ports.RecordTextUpdater = (*Store)(nil)

// UpdateRecordText rewrites the text of one existing record in place. The file,
// ID and created_at stay the same; the store re-reads the current record under
// its lock, checks the caller's expected revision and only then renders the
// replacement. Exactly one record pointer must be set, and it must be the one
// the stored file already holds: the store never moves a record between kinds
// or projects.
func (s *Store) UpdateRecordText(ctx context.Context, update ports.RecordTextUpdate) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	set := 0
	for _, present := range []bool{
		update.Note != nil, update.Decision != nil, update.Attempt != nil, update.Handoff != nil,
	} {
		if present {
			set++
		}
	}
	if set != 1 {
		return domain.NewValidationError("record", fmt.Sprintf("exactly one record is required, got %d", set))
	}
	switch {
	case update.Note != nil:
		_, err := s.replaceNote(ctx, *update.Note, update.ExpectedUpdatedAt)
		return err
	case update.Decision != nil:
		_, err := s.replaceDecision(ctx, *update.Decision, update.ExpectedUpdatedAt)
		return err
	case update.Attempt != nil:
		_, err := s.replaceAttempt(ctx, *update.Attempt, update.ExpectedUpdatedAt)
		return err
	default:
		_, err := s.replaceHandoff(ctx, *update.Handoff, update.ExpectedUpdatedAt)
		return err
	}
}
