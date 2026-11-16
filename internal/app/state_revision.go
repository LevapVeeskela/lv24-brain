package app

import (
	"fmt"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// StateRevision is the opaque revision of a stored project state. It is derived
// from the state's last update, so every machine that reads the same state.md
// reports the same revision, and it is the value expected_revision compares
// against. A state that was never written has the revision "none".
func StateRevision(state domain.State) string {
	if state.UpdatedAt.IsZero() {
		return "none"
	}
	return state.UpdatedAt.UTC().Format(time.RFC3339Nano)
}

// StateRevisionConflict reports a refused guarded state write. It unwraps to
// domain.ErrConflict and carries the current revision, so a client can show what
// it collided with and retry without a second read.
type StateRevisionConflict struct {
	ProjectID string
	Expected  string
	Current   string
}

func (e *StateRevisionConflict) Error() string {
	return fmt.Sprintf("state of %s is at revision %s, not %s", e.ProjectID, e.Current, e.Expected)
}

// Unwrap maps the conflict onto the shared conflict error so every adapter
// reports it as 409 without matching on this concrete type.
func (e *StateRevisionConflict) Unwrap() error { return domain.ErrConflict }
