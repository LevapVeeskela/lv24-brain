package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// DefaultProposalDigestWindow is the look-back of `brain digest --source`
	// when neither --since nor --all is given.
	DefaultProposalDigestWindow = 30 * 24 * time.Hour
	// DefaultProposalDigestLimit bounds how many stored items one digest scans.
	DefaultProposalDigestLimit = 500
	// maxProposalScanPage caps one ItemStore.List page.
	maxProposalScanPage = 1000
)

// ProposalAction says what a digest did (or would do) with one candidate.
type ProposalAction string

const (
	ProposalActionCreated   ProposalAction = "created"
	ProposalActionUpdated   ProposalAction = "updated"
	ProposalActionUnchanged ProposalAction = "unchanged"
)

// ProposalDigestOptions selects one connection's stored items and the rules
// that turn them into candidate records. The digest never reads a provider:
// only items already stored by ingestion are considered.
type ProposalDigestOptions struct {
	// ConnectionID names the configured source connection.
	ConnectionID string
	// ProjectID is the destination project id or alias. Empty resolves from
	// the connection's single project or the items' unambiguous ProjectID.
	ProjectID string
	// Since is the window start; zero selects DefaultProposalDigestWindow
	// before the app clock. Ignored when All is true.
	Since time.Time
	// All disables the time window.
	All bool
	// Limit caps scanned items; zero selects DefaultProposalDigestLimit.
	Limit int
	// Rules restricts which deterministic rules run; empty enables all.
	Rules []string
	// Write persists pending proposals. The default is a read-only preview.
	Write bool
	// DryRun is rejected together with Write. The app records it only so the
	// caller's intent is explicit; it is otherwise the same as a preview.
	DryRun bool
}

// ProposalDigestCounts summarizes one digest run. Created and Updated count
// what was written, or what a preview would write.
type ProposalDigestCounts struct {
	Scanned   int  `json:"scanned"`
	Created   int  `json:"created"`
	Updated   int  `json:"updated"`
	Unchanged int  `json:"unchanged"`
	Skipped   int  `json:"skipped"`
	Truncated bool `json:"truncated"`
}

// ProposalDigestEntry is one candidate that matched a rule. Action is created,
// updated or unchanged; skipped items only affect Counts.Skipped.
type ProposalDigestEntry struct {
	Proposal domain.Proposal
	Action   ProposalAction
}

// ProposalDigest is the result of a proposal digest.
type ProposalDigest struct {
	GeneratedAt  time.Time
	ProjectID    string
	ConnectionID string
	// Since is nil when All is true.
	Since   *time.Time
	All     bool
	Write   bool
	Counts  ProposalDigestCounts
	Entries []ProposalDigestEntry
}

// Written is the number of proposals created or refreshed by --write; a
// preview always reports zero.
func (d ProposalDigest) Written() int {
	if !d.Write {
		return 0
	}
	return d.Counts.Created + d.Counts.Updated
}

// ProposalDigest builds candidate records from one connection's stored raw
// items. It never runs the source and never touches curated records. A rerun
// is idempotent: proposal ids are deterministic, an unchanged payload is
// counted once, and terminal proposals are never overwritten.
func (a *App) ProposalDigest(ctx context.Context, opts ProposalDigestOptions) (ProposalDigest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ProposalDigest{}, err
	}
	if opts.Write && opts.DryRun {
		return ProposalDigest{}, domain.NewValidationError("digest", "--write cannot be combined with --dry-run")
	}
	if opts.Limit < 0 {
		return ProposalDigest{}, domain.NewValidationError("limit", "must not be negative")
	}
	if opts.Limit == 0 {
		opts.Limit = DefaultProposalDigestLimit
	}
	if a.deps.Connections == nil {
		return ProposalDigest{}, notImplemented("proposal digest: connections")
	}
	if a.deps.Items == nil {
		return ProposalDigest{}, notImplemented("proposal digest: stored items")
	}
	if a.deps.Proposals == nil {
		return ProposalDigest{}, notImplemented("proposal digest: proposal store")
	}
	connectionID := strings.TrimSpace(opts.ConnectionID)
	if err := domain.ValidateConnectionID("connection.id", connectionID); err != nil {
		return ProposalDigest{}, err
	}
	filter, err := proposalRuleFilter(opts.Rules)
	if err != nil {
		return ProposalDigest{}, err
	}
	connection, err := a.deps.Connections.Get(ctx, connectionID)
	if err != nil {
		return ProposalDigest{}, fmt.Errorf("load connection %s: %w", connectionID, err)
	}
	projectID, err := a.resolveProposalProject(ctx, connection, opts.ProjectID, opts.Limit)
	if err != nil {
		return ProposalDigest{}, err
	}

	now := a.now()
	result := ProposalDigest{
		GeneratedAt:  now,
		ProjectID:    projectID,
		ConnectionID: connection.ID,
		All:          opts.All,
		Write:        opts.Write,
	}
	if !opts.All {
		cutoff := opts.Since
		if cutoff.IsZero() {
			cutoff = now.Add(-DefaultProposalDigestWindow)
		}
		cutoff = cutoff.UTC()
		result.Since = &cutoff
	}

	existing, err := a.deps.Proposals.List(ctx, projectID, ports.ProposalListOptions{ConnectionID: connection.ID})
	if err != nil {
		return result, fmt.Errorf("list proposals: %w", err)
	}
	existingByID := make(map[string]domain.Proposal, len(existing))
	for _, proposal := range existing {
		existingByID[proposal.ID] = proposal
	}

	scanned, truncated, err := a.scanProposalItems(ctx, connection.ID, opts.Limit, func(item domain.Item) error {
		return a.digestProposalItem(ctx, connection, item, projectID, result.Since, filter, existingByID, opts.Write, &result)
	})
	result.Counts.Scanned = scanned
	result.Counts.Truncated = truncated
	if err != nil {
		return result, err
	}
	return result, nil
}

// digestProposalItem classifies one stored item into the running digest.
func (a *App) digestProposalItem(
	ctx context.Context,
	connection domain.Connection,
	item domain.Item,
	projectID string,
	since *time.Time,
	filter map[string]struct{},
	existing map[string]domain.Proposal,
	write bool,
	result *ProposalDigest,
) error {
	if item.Deleted || strings.TrimSpace(item.ProjectID) == "" || item.ProjectID != projectID {
		result.Counts.Skipped++
		return nil
	}
	if since != nil && itemOutsideWindow(item, *since) {
		result.Counts.Skipped++
		return nil
	}
	match, ok := matchProposalRuleWithFilter(connection, item, filter)
	if !ok {
		result.Counts.Skipped++
		return nil
	}
	proposal, err := buildProposal(projectID, connection, item, match)
	if err != nil {
		// A damaged or out-of-bounds item must not abort the whole digest.
		result.Counts.Skipped++
		return nil
	}
	current, exists := existing[proposal.ID]
	if exists && current.IsTerminal() {
		result.Counts.Skipped++
		return nil
	}
	action := ProposalActionCreated
	switch {
	case exists && current.PayloadHash == proposal.PayloadHash:
		action = ProposalActionUnchanged
	case exists:
		action = ProposalActionUpdated
	}

	if action == ProposalActionUnchanged {
		result.Counts.Unchanged++
		result.Entries = append(result.Entries, ProposalDigestEntry{Proposal: current, Action: action})
		return nil
	}
	stored := proposal
	if write {
		stored, err = a.deps.Proposals.Put(ctx, proposal)
		if err != nil {
			return fmt.Errorf("write proposal %s: %w", proposal.ID, err)
		}
	}
	if action == ProposalActionCreated {
		result.Counts.Created++
	} else {
		result.Counts.Updated++
	}
	result.Entries = append(result.Entries, ProposalDigestEntry{Proposal: stored, Action: action})
	return nil
}

// resolveProposalProject applies the documented precedence: --project, the
// connection's single project, then the unambiguous ProjectID of stored items.
func (a *App) resolveProposalProject(ctx context.Context, connection domain.Connection, explicit string, limit int) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		if a.deps.Store == nil {
			// Embedders without a registry still get deterministic project
			// scoping; the write path validates the project later.
			return explicit, nil
		}
		project, ok, err := a.LookupProject(ctx, explicit)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", domain.NewValidationError("project",
				fmt.Sprintf("%q is not a registered project id or alias", explicit))
		}
		return project.ID, nil
	}
	if len(connection.Projects) == 1 {
		if projectID := strings.TrimSpace(connection.Projects[0]); projectID != "" {
			return projectID, nil
		}
	}
	projects := make(map[string]struct{})
	if _, _, err := a.scanProposalItems(ctx, connection.ID, limit, func(item domain.Item) error {
		if item.Deleted {
			return nil
		}
		if projectID := strings.TrimSpace(item.ProjectID); projectID != "" {
			projects[projectID] = struct{}{}
		}
		return nil
	}); err != nil {
		return "", err
	}
	ids := make([]string, 0, len(projects))
	for projectID := range projects {
		ids = append(ids, projectID)
	}
	sort.Strings(ids)
	switch len(ids) {
	case 0:
		return "", domain.NewValidationError("project",
			"no stored item of connection "+connection.ID+" is mapped to a project; pass --project <id>")
	case 1:
		return ids[0], nil
	default:
		return "", domain.NewValidationError("project",
			fmt.Sprintf("connection %s has items in projects %s; pass --project <id>", connection.ID, strings.Join(ids, ", ")))
	}
}

// scanProposalItems pages stored items in key order and calls fn for each one
// until limit is reached. It reports whether more live items remained.
func (a *App) scanProposalItems(ctx context.Context, connectionID string, limit int, fn func(domain.Item) error) (int, bool, error) {
	if limit <= 0 {
		return 0, false, nil
	}
	scanned := 0
	cursor := ""
	for scanned < limit {
		pageSize := limit - scanned
		if pageSize > maxProposalScanPage {
			pageSize = maxProposalScanPage
		}
		page, err := a.deps.Items.List(ctx, connectionID, ports.ItemListOptions{Cursor: cursor, Limit: pageSize})
		if err != nil {
			return scanned, false, err
		}
		if len(page.Items) == 0 {
			return scanned, page.NextCursor != "", nil
		}
		for index, item := range page.Items {
			if err := fn(item); err != nil {
				return scanned, false, err
			}
			scanned++
			if scanned == limit {
				return scanned, page.NextCursor != "" || index+1 < len(page.Items), nil
			}
		}
		if page.NextCursor == "" {
			return scanned, false, nil
		}
		cursor = page.NextCursor
	}
	return scanned, false, nil
}

// itemOutsideWindow uses the provider update time when known, then the host
// observation time, then creation. A newly observed item is never hidden just
// because its provider timestamps are old or missing.
func itemOutsideWindow(item domain.Item, cutoff time.Time) bool {
	at := item.UpdatedAt
	if at.IsZero() {
		at = item.ObservedAt
	}
	if at.IsZero() {
		at = item.CreatedAt
	}
	return !at.IsZero() && at.Before(cutoff)
}
