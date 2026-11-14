package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Raw access policy (Phase 0.15). context-allow.json is the only way raw
// connector text may enter a response. Every raw read path goes through this
// gate: `brain search --include-dumps`, `brain search --kind item`,
// brain_search with kinds ["item"], context excerpts and the server-mode
// search client. A missing grant means an empty result plus an actionable
// notice, never a silent snippet.

// rawAccessHint points an operator at the policy file.
const rawAccessHint = "raw connector text withheld: add a context-allow.json grant for the project and connection (see docs/security.md, raw context excerpts)"

// rawAccessUnavailableHint explains the server-mode limit: grants live in the
// data directory, which a server client does not have.
const rawAccessUnavailableHint = "raw connector text is not available in server mode: context-allow.json grants are local/git only"

// rawAccess is the loaded grant policy for one call.
type rawAccess struct {
	available bool
	grants    domain.ContextGrants
	now       time.Time
}

// loadRawAccess reads the grant policy. A store without grants (server mode)
// is a default-deny policy, not an error; a damaged file is a hard error so a
// typo can never silently widen access.
func (a *App) loadRawAccess(ctx context.Context) (rawAccess, error) {
	if a.deps.ContextGrants == nil {
		return rawAccess{}, nil
	}
	grants, err := a.deps.ContextGrants.ContextGrants(ctx)
	if err != nil {
		return rawAccess{}, fmt.Errorf("read context grants: %w", err)
	}
	return rawAccess{available: true, grants: grants, now: a.now()}, nil
}

// Match reports the first grant that allows raw text of kind for the exact
// project and connection. An unavailable policy grants nothing.
func (r rawAccess) Match(projectID, connectionID string, kind domain.ItemKind) (domain.ContextGrant, bool) {
	if !r.available {
		return domain.ContextGrant{}, false
	}
	return r.grants.Match(projectID, connectionID, kind)
}

// MatchConnection reports the first grant for the exact project and
// connection, regardless of its kind filter.
func (r rawAccess) MatchConnection(projectID, connectionID string) (domain.ContextGrant, bool) {
	if !r.available {
		return domain.ContextGrant{}, false
	}
	return r.grants.MatchConnection(projectID, connectionID)
}

// allowsProject reports whether the policy has any grant for the project. It
// produces the "no grant for project" diagnostic of the context path.
func (r rawAccess) allowsProject(projectID string) bool {
	if !r.available {
		return false
	}
	for _, grant := range r.grants.Grants {
		if grant.Project == projectID {
			return true
		}
	}
	return false
}

// hint is the user-facing explanation for a withheld raw result.
func (r rawAccess) hint() string {
	if !r.available {
		return rawAccessUnavailableHint
	}
	return rawAccessHint
}

// wantsRawText reports whether a search asks for raw-zone text: an unfiltered
// search with dumps included, or an explicit dump/item kind.
func wantsRawText(kinds []string, includeDumps bool) bool {
	if len(kinds) > 0 {
		return slices.Contains(kinds, KindItem) || slices.Contains(kinds, KindDump)
	}
	return includeDumps
}

// filterRawHits drops raw-zone hits no grant authorizes and counts them.
// Curated kinds pass through untouched. An item hit is matched by the
// connection in its index id; a grant with a kind filter is resolved against
// the stored item, so a kind-restricted grant never leaks another item kind.
// Legacy dumps carry no connection identity and therefore cannot be granted.
func (a *App) filterRawHits(ctx context.Context, access rawAccess, hits []ports.Hit) ([]ports.Hit, int) {
	getter, _ := a.deps.Items.(itemKeyGetter)
	kept := make([]ports.Hit, 0, len(hits))
	dropped := 0
	for _, hit := range hits {
		switch hit.Kind {
		case KindItem:
			if !access.allowsItem(ctx, getter, hit) {
				dropped++
				continue
			}
		case KindDump:
			dropped++
			continue
		}
		kept = append(kept, hit)
	}
	return kept, dropped
}

// allowsItem resolves an item hit to its connection and, when the grant
// restricts kinds or age, to the stored item. A kind- or age-filtered grant
// therefore never returns a hit it would not have allowed as an excerpt.
func (r rawAccess) allowsItem(ctx context.Context, getter itemKeyGetter, hit ports.Hit) bool {
	key, ok := itemKeyFromHit(hit)
	if !ok {
		return false
	}
	connectionID, _, _ := strings.Cut(key, "/")
	grant, ok := r.MatchConnection(hit.ProjectID, connectionID)
	if !ok {
		return false
	}
	if len(grant.Kinds) == 0 && grant.MaxAgeDays == 0 {
		return true
	}
	if getter == nil {
		return false
	}
	item, err := getter.GetByKey(ctx, key)
	if err != nil {
		return false
	}
	if _, ok := r.Match(hit.ProjectID, connectionID, item.Kind); !ok {
		return false
	}
	return grant.AllowsAge(excerptTime(item), r.now)
}
