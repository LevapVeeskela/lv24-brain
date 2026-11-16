package serverstore

import (
	"context"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// UpdateRecordProvenance rewrites only the provenance fields of one record and
// bumps its revision. It delegates to the wrapped postgres store.
func (s *Store) UpdateRecordProvenance(ctx context.Context, kind, id string, provenance domain.Provenance, expected int64) (domain.SyncRevision, error) {
	revision, err := s.pg.UpdateRecordProvenance(ctx, kind, id, provenance, expected)
	return revision, mapError(err)
}
