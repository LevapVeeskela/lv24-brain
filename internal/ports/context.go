package ports

import (
	"context"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// ContextGrantStore loads the explicit raw-context grants. A missing grant
// file is an empty default-deny policy, not an error.
type ContextGrantStore interface {
	ContextGrants(ctx context.Context) (domain.ContextGrants, error)
}
