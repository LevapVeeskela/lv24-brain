package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Journal events of the merge lifecycle.
const (
	// JournalMerge records one applied two-phase merge.
	JournalMerge = "merge"
	// JournalMergeRollback records one rollback of a merge manifest.
	JournalMergeRollback = "merge-rollback"
)

// MergeOptions selects a two-phase merge. DryRun only plans; Apply writes.
type MergeOptions struct {
	ProjectID string
	// Survivor keeps its identity and absorbs the duplicates.
	Survivor string
	// Duplicates are the records to absorb.
	Duplicates []string
	// DryRun returns the plan and never writes, even when Apply is set.
	DryRun bool
	// Apply writes the plan. Without it the call only plans.
	Apply bool
}

// MergeResult is the plan plus, when applied, the manifest and touched files.
type MergeResult struct {
	Plan     ports.MergePlan
	Applied  bool
	Manifest string
	Files    []ports.MergeFile
}

// MergeRollbackResult is the restored state of one merge.
type MergeRollbackResult struct {
	Manifest          string
	Files             []ports.MergeFile
	Conflicts         []string
	AlreadyRolledBack bool
	Reindexed         bool
}

// mergeStore returns the store's two-phase merge capability.
func (a *App) mergeStore() (ports.RecordMerger, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	merger, ok := a.deps.Store.(ports.RecordMerger)
	if !ok {
		return nil, fmt.Errorf("%w: this store cannot merge records (local and git modes only)", domain.ErrUnsupported)
	}
	return merger, nil
}

// PlanMerge computes a merge plan without writing. Conflicts are reported in
// the plan; a protected record or an unknown project is an error.
func (a *App) PlanMerge(ctx context.Context, opts MergeOptions) (ports.MergePlan, error) {
	merger, err := a.mergeStore()
	if err != nil {
		return ports.MergePlan{}, err
	}
	project, err := a.ensureProject(ctx, opts.ProjectID, false)
	if err != nil {
		return ports.MergePlan{}, err
	}
	survivor := strings.TrimSpace(opts.Survivor)
	if survivor == "" {
		return ports.MergePlan{}, domain.NewValidationError("survivor", "is required")
	}
	plan, err := merger.PlanMerge(ctx, ports.MergeRequest{
		ProjectID: project.ID, Survivor: survivor, Duplicates: opts.Duplicates,
	})
	if err != nil {
		return ports.MergePlan{}, err
	}
	if err := a.checkMergeProtection(project.ID, plan); err != nil {
		return plan, err
	}
	return plan, nil
}

// Merge plans and, unless the caller asked only for a plan, applies the merge.
// The apply is idempotent: repeating a merge that already happened writes
// nothing. A conflicting plan is refused before any write.
func (a *App) Merge(ctx context.Context, opts MergeOptions) (MergeResult, error) {
	plan, err := a.PlanMerge(ctx, opts)
	result := MergeResult{Plan: plan}
	if err != nil {
		return result, err
	}
	if len(plan.Conflicts) > 0 {
		return result, domain.NewValidationError("merge", "the plan has conflicts: "+strings.Join(plan.Conflicts, "; "))
	}
	if opts.DryRun || !opts.Apply {
		return result, nil
	}
	if !plan.Changed {
		return result, nil
	}
	merger, err := a.mergeStore()
	if err != nil {
		return result, err
	}
	outcome, err := merger.ApplyMerge(ctx, plan)
	result.Applied = err == nil
	result.Manifest = outcome.Manifest
	result.Files = outcome.Files
	if err != nil {
		return result, err
	}
	ids := mergeTouchedIDs(plan)
	indexErr := a.refreshMergeIndex(ctx, plan.ProjectID, ids)
	journalErr := a.journalWrite(ctx, JournalMerge, plan.ProjectID, plan.Survivor.ID,
		fmt.Sprintf("merged %d into %s", len(plan.Duplicates), plan.Survivor.ID))
	if indexErr != nil {
		return result, fmt.Errorf("merge %s applied but the search index update failed: %w", plan.Survivor.ID, indexErr)
	}
	if journalErr != nil {
		return result, fmt.Errorf("merge %s applied but the journal append failed: %w", plan.Survivor.ID, journalErr)
	}
	return result, nil
}

// RollbackMerge restores one merge manifest byte for byte and rebuilds the
// derived index from the restored records.
func (a *App) RollbackMerge(ctx context.Context, manifest string) (MergeRollbackResult, error) {
	merger, err := a.mergeStore()
	if err != nil {
		return MergeRollbackResult{}, err
	}
	outcome, err := merger.RollbackMerge(ctx, manifest)
	result := MergeRollbackResult{
		Manifest: outcome.Manifest, Files: outcome.Files,
		Conflicts: outcome.Conflicts, AlreadyRolledBack: outcome.AlreadyRolledBack,
	}
	if err != nil {
		return result, err
	}
	if outcome.AlreadyRolledBack {
		return result, nil
	}
	if a.deps.Index != nil {
		if _, reindexErr := a.Reindex(ctx, IndexOptions{}); reindexErr != nil {
			a.markIndexDirty()
			return result, fmt.Errorf("merge rollback restored the records but the index rebuild failed: %w", reindexErr)
		}
		result.Reindexed = true
	}
	if err := a.journalWrite(ctx, JournalMergeRollback, "", "",
		"restored "+outcome.Manifest); err != nil {
		return result, fmt.Errorf("merge rollback restored the records but the journal append failed: %w", err)
	}
	return result, nil
}

// checkMergeProtection refuses a merge that would edit a protected project or
// a protected record directory. The survivor and every duplicate are checked,
// so a protected record cannot be absorbed by a merge.
func (a *App) checkMergeProtection(projectID string, plan ports.MergePlan) error {
	records := make([]ports.MergeRecord, 0, len(plan.Duplicates)+len(plan.AlreadyMerged)+1)
	records = append(records, plan.Survivor)
	records = append(records, plan.Duplicates...)
	for _, record := range records {
		if record.ID == "" {
			continue
		}
		if err := a.checkProtectedSelector(projectID, record.Kind, record.Title, false); err != nil {
			return err
		}
	}
	return nil
}

// mergeTouchedIDs lists the record ids a merge changed or removed.
func mergeTouchedIDs(plan ports.MergePlan) []string {
	seen := map[string]struct{}{plan.Survivor.ID: {}}
	for _, record := range plan.Duplicates {
		seen[record.ID] = struct{}{}
	}
	for _, record := range plan.AlreadyMerged {
		seen[record.ID] = struct{}{}
	}
	for _, link := range plan.Links {
		seen[link.SourceID] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
}

// refreshMergeIndex re-projects the records a merge touched: a live record is
// upserted, a trashed duplicate is removed from the index.
func (a *App) refreshMergeIndex(ctx context.Context, projectID string, ids []string) error {
	if a.deps.Index == nil {
		return nil
	}
	for _, id := range ids {
		record, err := a.GetRecord(ctx, projectID, id)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			if deleteErr := a.deps.Index.Delete(ctx, indexKey(projectID, id)); deleteErr != nil {
				a.markIndexDirty()
				return deleteErr
			}
		case err != nil:
			a.markIndexDirty()
			return err
		default:
			if err := a.upsertIndex(ctx, recordDocument(record)); err != nil {
				return err
			}
		}
	}
	return nil
}
