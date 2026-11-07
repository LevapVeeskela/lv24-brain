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

// Searcher runs a full-text search against a remote server. Server mode uses
// it for brain search through GET /v1/search.
type Searcher interface {
	Search(ctx context.Context, query ports.Query) ([]ports.Hit, error)
}

// Search queries the local FTS5 index in local and git mode, and the server
// search endpoint in server mode.
func (a *App) Search(ctx context.Context, text, projectID string, limit int) ([]ports.Hit, error) {
	if a.cfg.Mode == config.ModeServer {
		if a.deps.Searcher == nil {
			return nil, fmt.Errorf("%w: server search client is not available", domain.ErrUnsupported)
		}
		return a.search(ctx, text, projectID, limit, a.deps.Searcher.Search)
	}
	if a.deps.Index == nil {
		return nil, fmt.Errorf("%w: local search index is not available", domain.ErrUnsupported)
	}
	// A stale projection is still searchable; a failed repair keeps the
	// marker and is retried by the next search.
	_ = a.repairIndexIfDirty(ctx)
	return a.search(ctx, text, projectID, limit, a.deps.Index.Search)
}

func (a *App) search(ctx context.Context, text, projectID string, limit int, run func(context.Context, ports.Query) ([]ports.Hit, error)) ([]ports.Hit, error) {
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
	return run(ctx, ports.Query{
		Text:      text,
		ProjectID: strings.TrimSpace(projectID),
		Limit:     limit,
	})
}
