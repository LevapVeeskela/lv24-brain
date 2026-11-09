package app

import (
	"context"
	"fmt"
	"strings"
	"time"

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

// KindDump is the index kind of connector dumps (mail, GitHub, GitLab).
const KindDump = "dump"

// SearchOptions selects what a search returns. By default connector dumps
// are left out so curated records are not drowned by mail and issue lists.
type SearchOptions struct {
	Text      string
	ProjectID string
	Limit     int
	// Kinds keeps only these kinds (note, decision, attempt, handoff, state,
	// dump); naming "dump" includes dumps.
	Kinds []string
	// IncludeDumps adds connector dumps and raw-zone items (kind "item") to
	// an unfiltered search.
	IncludeDumps bool
	// Since keeps only documents updated at or after this time; zero keeps
	// everything.
	Since time.Time
}

// Search queries curated records (no dumps); see SearchWith.
func (a *App) Search(ctx context.Context, text, projectID string, limit int) ([]ports.Hit, error) {
	return a.SearchWith(ctx, SearchOptions{Text: text, ProjectID: projectID, Limit: limit})
}

// SearchWith queries the local FTS5 index in local and git mode, and the
// server search endpoint in server mode.
func (a *App) SearchWith(ctx context.Context, opts SearchOptions) ([]ports.Hit, error) {
	if a.cfg.Mode == config.ModeServer {
		if a.deps.Searcher == nil {
			return nil, fmt.Errorf("%w: server search client is not available", domain.ErrUnsupported)
		}
		// Servers before item support reject exclude_kind=item; a server
		// treats an excluded "dump" as excluding its items too.
		return a.search(ctx, opts, []string{KindDump}, a.deps.Searcher.Search)
	}
	if a.deps.Index == nil {
		return nil, fmt.Errorf("%w: local search index is not available", domain.ErrUnsupported)
	}
	// A stale projection is still searchable; a failed repair keeps the
	// marker and is retried by the next search.
	_ = a.repairIndexIfDirty(ctx)
	return a.search(ctx, opts, []string{KindDump, KindItem}, a.deps.Index.Search)
}

// search runs the query; rawKinds are excluded from an unfiltered search
// unless opts.IncludeDumps is set.
func (a *App) search(ctx context.Context, opts SearchOptions, rawKinds []string, run func(context.Context, ports.Query) ([]ports.Hit, error)) ([]ports.Hit, error) {
	text := strings.TrimSpace(opts.Text)
	if text == "" {
		return nil, domain.NewValidationError("search.query", "is required")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	if ctx == nil {
		ctx = context.Background()
	}
	query := ports.Query{Text: text, ProjectID: strings.TrimSpace(opts.ProjectID), Limit: limit, Since: opts.Since}
	for _, kind := range opts.Kinds {
		if kind = strings.TrimSpace(kind); kind != "" {
			query.Kinds = append(query.Kinds, kind)
		}
	}
	if len(query.Kinds) == 0 && !opts.IncludeDumps {
		query.ExcludeKinds = rawKinds
	}
	return run(ctx, query)
}
