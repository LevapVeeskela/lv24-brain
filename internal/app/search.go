package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// DefaultSearchLimit is used when a search does not set a limit.
const DefaultSearchLimit = 20

// Search queries the local FTS5 index. Server mode search is not wired.
func (a *App) Search(ctx context.Context, text, projectID string, limit int) ([]ports.Hit, error) {
	if a.cfg.Mode == config.ModeServer {
		return nil, fmt.Errorf("%w: search uses the local index; server search is not wired", domain.ErrUnsupported)
	}
	if a.deps.Index == nil {
		return nil, fmt.Errorf("%w: local search index is not available", domain.ErrUnsupported)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, domain.NewValidationError("search.query", "is required")
	}
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return a.deps.Index.Search(ctx, ports.Query{
		Text:      text,
		ProjectID: strings.TrimSpace(projectID),
		Limit:     limit,
	})
}
