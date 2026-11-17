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

// Consolidation op kinds. A plan carries executable merge and archive ops; a
// near cluster below the merge threshold is report-only and stays in plan.Near.
const (
	// ConsolidateMerge merges an exact duplicate cluster with the two-phase
	// merge; the oldest record survives.
	ConsolidateMerge = "merge"
	// ConsolidateArchive trashes one superseded record that no live record
	// links to. The trash keeps the file, so the op is reversible.
	ConsolidateArchive = "archive"
)

// Consolidation risk labels.
const (
	ConsolidateRiskLow    = "low"
	ConsolidateRiskMedium = "medium"
)

// Journal events of the consolidation lifecycle.
const (
	JournalConsolidate         = "consolidate"
	JournalConsolidateRollback = "consolidate-rollback"
)

const (
	// ConsolidateMaxOps bounds a plan so a noisy project produces a page, not
	// a flood; --max raises or lowers it.
	ConsolidateMaxOps = 50
	// consolidateArchiveDays is the default age a record must have been
	// superseded for before it is an archive candidate.
	consolidateArchiveDays = 30
	// ConsolidateNearMergeThreshold is the Jaccard similarity at which a near
	// cluster becomes a merge op. Below it the cluster is report-only.
	ConsolidateNearMergeThreshold = 0.98
)

// ConsolidateOptions selects what `brain consolidate` plans or applies.
type ConsolidateOptions struct {
	ProjectID string
	// Scan inspects every registered project instead of one.
	Scan bool
	// Max bounds the number of ops in a plan; <= 0 uses the default cap.
	Max int
	// ArchiveAfterDays is the minimum age, counted from the moment a record
	// stopped being current, before a superseded record is an archive
	// candidate; <= 0 uses the default of 30 days.
	ArchiveAfterDays int
	// Phase is "plan" (default) or "apply". Plan never writes.
	Phase string
	// Ops lists the op ids to apply; empty applies every executable op of the
	// fresh plan. Ops are re-derived from the records, never replayed.
	Ops []string
	// Fingerprint is the plan fingerprint from the plan phase. Apply refuses
	// a stale fingerprint and returns the fresh plan.
	Fingerprint string
}

// ConsolidateOp is one proposed operation. Applying writes only when the
// caller names the op (or accepts the whole plan) in a second call.
type ConsolidateOp struct {
	// ID is deterministic: "merge-<survivor>" or "archive-<record id>".
	ID string
	// Kind is merge, archive or report.
	Kind      string
	ProjectID string
	// RecordKind is the kind of the records the op touches.
	RecordKind string
	// IDs are the touched record ids: the survivor first for a merge.
	IDs []string
	// Reason explains why the op was proposed, with the evidence.
	Reason string
	// Fingerprint covers the op's inputs, so a stale plan is detectable.
	Fingerprint string
	// Risk is low, medium or info.
	Risk string
	// Reversible names the action that undoes the op.
	Reversible string
}

// ConsolidatePlan is the proposal-only result of phase=plan.
type ConsolidatePlan struct {
	// Projects is how many projects were inspected.
	Projects int
	Ops      []ConsolidateOp
	// Near lists the near clusters below the merge threshold. They are never
	// ops: a person decides by hand.
	Near []DuplicateCluster
	// Fingerprint covers every op, so apply can refuse a stale plan.
	Fingerprint string
	// Truncated reports that the op cap cut the plan short.
	Truncated bool
}

// ConsolidateMergeResult is one applied merge op.
type ConsolidateMergeResult struct {
	Op       string
	Survivor string
	// Manifest is the store's merge manifest (merges/<ts>.json); empty when
	// the merge was already in place.
	Manifest string
	Files    []ports.MergeFile
}

// ConsolidateArchiveResult is one applied archive op.
type ConsolidateArchiveResult struct {
	Op   string
	Kind string
	ID   string
}

// ConsolidateSkip reports an op that was not applied and why.
type ConsolidateSkip struct {
	Op     string
	Reason string
}

// ConsolidateApplyResult reports what an apply wrote.
type ConsolidateApplyResult struct {
	// Manifest is the consolidation manifest under backups/consolidate-<ts>/;
	// empty when nothing was applied.
	Manifest string
	Merged   []ConsolidateMergeResult
	Archived []ConsolidateArchiveResult
	Skipped  []ConsolidateSkip
}

// ConsolidateRollbackResult reports a rollback of one consolidation manifest.
type ConsolidateRollbackResult struct {
	Manifest          string
	Merges            int
	Archived          int
	Conflicts         []string
	AlreadyRolledBack bool
	Reindexed         bool
}

// Consolidate plans or applies a consolidation run. The default phase is
// plan: it computes deterministic ops and writes nothing. Apply re-derives
// the plan, refuses a stale fingerprint and executes only the named ops.
func (a *App) Consolidate(ctx context.Context, opts ConsolidateOptions) (ConsolidatePlan, *ConsolidateApplyResult, error) {
	if err := a.requireStore(); err != nil {
		return ConsolidatePlan{}, nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := a.consolidationPlan(ctx, opts)
	if err != nil {
		return ConsolidatePlan{}, nil, err
	}
	if opts.Phase != "apply" {
		return plan, nil, nil
	}
	if fingerprint := strings.TrimSpace(opts.Fingerprint); fingerprint != "" && fingerprint != plan.Fingerprint {
		return plan, nil, fmt.Errorf("consolidation: the records changed since the plan was shown (fingerprint %s, now %s); run the plan again: %w",
			fingerprint, plan.Fingerprint, domain.ErrConflict)
	}
	result, err := a.consolidationApply(ctx, opts, plan)
	return plan, result, err
}

// consolidationPlan computes the ops of one run without writing.
func (a *App) consolidationPlan(ctx context.Context, opts ConsolidateOptions) (ConsolidatePlan, error) {
	var projects []domain.Project
	if opts.Scan {
		listed, err := a.deps.Store.ListProjects(ctx)
		if err != nil {
			return ConsolidatePlan{}, err
		}
		projects = listed
	} else {
		project, err := a.ensureProject(ctx, opts.ProjectID, false)
		if err != nil {
			return ConsolidatePlan{}, err
		}
		projects = []domain.Project{project}
	}
	plan := ConsolidatePlan{Projects: len(projects)}
	archiveAfter := opts.ArchiveAfterDays
	if archiveAfter <= 0 {
		archiveAfter = consolidateArchiveDays
	}
	max := opts.Max
	if max <= 0 {
		max = ConsolidateMaxOps
	}
	for _, project := range projects {
		in, err := a.duplicateInputs(ctx, project.ID)
		if err != nil {
			return ConsolidatePlan{}, fmt.Errorf("%s: %w", project.ID, err)
		}
		clusters := duplicatesOfProject(project.ID, in)
		var near []DuplicateCluster
		for _, cluster := range clusters {
			if cluster.Reason == DuplicateReasonExact {
				if len(plan.Ops) >= max {
					plan.Truncated = true
					break
				}
				plan.Ops = append(plan.Ops, a.exactMergeOp(cluster))
				continue
			}
			// A near cluster at or above the threshold becomes a merge op;
			// below it (or past the cap) it is reported for a human.
			if cluster.Similarity >= ConsolidateNearMergeThreshold && len(plan.Ops) < max {
				if op, ok := a.nearMergeOp(cluster); ok {
					plan.Ops = append(plan.Ops, op)
					continue
				}
			}
			near = append(near, cluster)
		}
		plan.Near = append(plan.Near, near...)
		if plan.Truncated {
			break
		}
		ops, truncated, err := a.archiveOps(ctx, project.ID, in, plan, archiveAfter, max)
		if err != nil {
			return ConsolidatePlan{}, err
		}
		plan.Ops = append(plan.Ops, ops...)
		plan.Truncated = truncated
		if plan.Truncated {
			break
		}
	}
	sort.SliceStable(plan.Ops, func(i, j int) bool { return consolidateOpLess(plan.Ops[i], plan.Ops[j]) })
	sort.SliceStable(plan.Near, func(i, j int) bool {
		left, right := plan.Near[i], plan.Near[j]
		if left.ProjectID != right.ProjectID {
			return left.ProjectID < right.ProjectID
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Members[0].ID < right.Members[0].ID
	})
	fingerprints := make([]string, 0, len(plan.Ops))
	for _, op := range plan.Ops {
		fingerprints = append(fingerprints, op.Fingerprint)
	}
	plan.Fingerprint = domain.BodyFingerprint(strings.Join(fingerprints, "\n"))
	return plan, nil
}

// exactMergeOp turns an exact cluster into a merge op. The oldest member
// survives, so the id a person sees first in `brain duplicates` keeps its
// identity.
func (a *App) exactMergeOp(cluster DuplicateCluster) ConsolidateOp {
	survivor := cluster.Members[0]
	ids := make([]string, 0, len(cluster.Members))
	for _, member := range cluster.Members {
		ids = append(ids, member.ID)
	}
	return ConsolidateOp{
		ID: "merge-" + survivor.ID, Kind: ConsolidateMerge,
		ProjectID: cluster.ProjectID, RecordKind: cluster.Kind, IDs: ids,
		Reason: fmt.Sprintf("exact duplicate cluster: %d records with identical normalized text (%d%% similar)",
			len(cluster.Members), int(cluster.Similarity*100)),
		Fingerprint: domain.BodyFingerprint(strings.Join([]string{ConsolidateMerge, cluster.Kind, strings.Join(ids, "\n")}, "\n")),
		Risk:        ConsolidateRiskLow,
		Reversible:  "brain record merge --rollback <manifest> (the manifest is printed on apply)",
	}
}

// nearMergeOp turns a near cluster at or above the threshold into a merge op.
// Same kind and same project are guaranteed by the cluster construction and
// checked again here: a cross-kind or cross-project near match is report-only.
func (a *App) nearMergeOp(cluster DuplicateCluster) (ConsolidateOp, bool) {
	if cluster.Similarity < ConsolidateNearMergeThreshold || len(cluster.Members) < 2 {
		return ConsolidateOp{}, false
	}
	if !consolidateSameProject(cluster) {
		return ConsolidateOp{}, false
	}
	survivor := cluster.Members[0]
	ids := make([]string, 0, len(cluster.Members))
	for _, member := range cluster.Members {
		ids = append(ids, member.ID)
	}
	return ConsolidateOp{
		ID: "merge-" + survivor.ID, Kind: ConsolidateMerge,
		ProjectID: cluster.ProjectID, RecordKind: cluster.Kind, IDs: ids,
		Reason: fmt.Sprintf("near-duplicate cluster at %d%% (at or above the %d%% merge threshold); the survivor's text wins, the copy stays in the trash",
			int(cluster.Similarity*100), int(ConsolidateNearMergeThreshold*100)),
		Fingerprint: domain.BodyFingerprint(strings.Join([]string{ConsolidateMerge, cluster.Kind, strings.Join(ids, "\n")}, "\n")),
		Risk:        ConsolidateRiskMedium,
		Reversible:  "brain record merge --rollback <manifest> (the manifest is printed on apply)",
	}, true
}

// consolidateSameProject reports whether every member belongs to the cluster's
// project. Clusters are built per project; the check is defense in depth for
// a merge that must never cross projects.
func consolidateSameProject(cluster DuplicateCluster) bool {
	for _, member := range cluster.Members {
		if member.ID == "" {
			return false
		}
	}
	return cluster.ProjectID != ""
}

// archiveOps proposes trashing superseded records that are older than the
// archive age and that no live record links to. Records already named by a
// merge op are skipped: the merge owns them.
func (a *App) archiveOps(ctx context.Context, projectID string, in reviewInputs, plan ConsolidatePlan, archiveAfterDays, max int) ([]ConsolidateOp, bool, error) {
	records, err := a.projectRecords(ctx, projectID)
	if err != nil {
		return nil, false, err
	}
	claimed := map[string]bool{}
	for _, op := range plan.Ops {
		for _, id := range op.IDs {
			claimed[id] = true
		}
	}
	now := a.now()
	cutoff := now.AddDate(0, 0, -archiveAfterDays)
	var ops []ConsolidateOp
	truncated := false
	for _, record := range records {
		if len(ops)+len(plan.Ops) >= max {
			truncated = true
			break
		}
		summary := record.Summary()
		if claimed[summary.ID] || summary.Pinned {
			continue
		}
		if !consolidateArchivable(record) {
			continue
		}
		anchor := consolidateArchiveAnchor(record)
		if anchor.IsZero() || anchor.After(cutoff) {
			continue
		}
		if !a.archiveWithoutLinks(projectID, records, summary.ID) {
			continue
		}
		if err := a.checkProtectedSelector(projectID, summary.Kind, summary.Title, false); err != nil {
			continue // protected records are never proposed
		}
		ops = append(ops, ConsolidateOp{
			ID: "archive-" + summary.ID, Kind: ConsolidateArchive,
			ProjectID: projectID, RecordKind: summary.Kind, IDs: []string{summary.ID},
			Reason: fmt.Sprintf("superseded since %s (%d days ago) and no live record links to it",
				anchor.UTC().Format("2006-01-02"), int(now.Sub(anchor).Hours()/24)),
			Fingerprint: consolidateArchiveFingerprint(record),
			Risk:        ConsolidateRiskMedium,
			Reversible:  fmt.Sprintf("brain record restore %s (the record moves to the project trash)", summary.ID),
		})
	}
	return ops, truncated, nil
}

// consolidateArchivable reports whether a record is a superseded record in the
// archive sense: its validity ended with a named replacement, or it is a
// decision marked superseded. A validity that merely expired names no
// replacement and stays (the stale-superseded finding owns that case).
func consolidateArchivable(record Record) bool {
	validity := record.Validity()
	if validity.SupersededBy != "" && !validity.IsCurrent() {
		return true
	}
	if record.Decision != nil && record.Decision.Status == domain.DecisionSuperseded {
		return true
	}
	return false
}

// consolidateArchiveAnchor is the instant a record stopped being current: its
// valid_to, falling back to the last update and then the creation.
func consolidateArchiveAnchor(record Record) time.Time {
	validity := record.Validity()
	if !validity.ValidTo.IsZero() {
		return validity.ValidTo
	}
	summary := record.Summary()
	if !summary.UpdatedAt.IsZero() {
		return summary.UpdatedAt
	}
	return summary.CreatedAt
}

// archiveWithoutLinks reports whether no live record links to the record:
// no [[wiki link]] from another live record of the project and no pending or
// accepted typed relation that points at it. The supersede chain does not
// count as a link: every superseded record has one, and archiving must stay
// possible.
func (a *App) archiveWithoutLinks(projectID string, records []Record, id string) bool {
	if backlinksTo(records, projectID, id) != nil {
		return false
	}
	for _, entry := range scanRelationEntries(records, "", id) {
		if entry.relation.Status != domain.RelationRejected {
			return false
		}
	}
	return true
}

// consolidateArchiveFingerprint covers the fields an archive depends on, so a
// record that was edited or linked between plan and apply changes it.
func consolidateArchiveFingerprint(record Record) string {
	var text string
	switch {
	case record.Note != nil:
		text = recordDedupText(record.Note.Title, record.Note.Body)
	case record.Decision != nil:
		text = decisionDedupText(*record.Decision)
	case record.Attempt != nil:
		text = attemptDedupText(*record.Attempt)
	default:
		text = handoffDedupText(*record.Handoff)
	}
	summary := record.Summary()
	return domain.BodyFingerprint(strings.Join([]string{
		ConsolidateArchive, summary.Kind, summary.ID,
		summary.UpdatedAt.UTC().Format(time.RFC3339Nano), text,
	}, "\n"))
}

// consolidateOpLess orders ops by project, kind and id, so a plan is stable
// across runs on the same data.
func consolidateOpLess(a, b ConsolidateOp) bool {
	if a.ProjectID != b.ProjectID {
		return a.ProjectID < b.ProjectID
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.ID < b.ID
}

// consolidationApply executes the named ops (or every executable op) of a
// fresh plan. Merges run through the two-phase merge, archives through the
// trash; both are reversible and both keep their own audit trail. The run is
// serialized globally for the data directory and each archive op is revalidated
// at apply time. The run is recorded in a consolidation manifest under backups/.
func (a *App) consolidationApply(ctx context.Context, opts ConsolidateOptions, plan ConsolidatePlan) (*ConsolidateApplyResult, error) {
	recorder, ok := a.deps.Store.(ports.ConsolidationRecorder)
	if !ok {
		return nil, fmt.Errorf("%w: this store cannot record a consolidation manifest (local and git modes only)", domain.ErrUnsupported)
	}
	selected, err := selectConsolidationOps(plan, opts.Ops)
	if err != nil {
		return nil, err
	}
	release, err := a.lockConsolidation(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	archiveAfter := opts.ArchiveAfterDays
	if archiveAfter <= 0 {
		archiveAfter = consolidateArchiveDays
	}
	result := &ConsolidateApplyResult{}
	for _, op := range selected {
		switch op.Kind {
		case ConsolidateMerge:
			a.consolidationApplyMerge(ctx, op, result)
		case ConsolidateArchive:
			a.consolidationApplyArchive(ctx, op, archiveAfter, result)
		}
	}
	if len(result.Merged) == 0 && len(result.Archived) == 0 {
		return result, nil
	}
	manifest := ports.ConsolidationManifest{CreatedAt: a.now().UTC(), Status: ports.ConsolidationApplied}
	for _, merged := range result.Merged {
		manifest.Ops = append(manifest.Ops, ports.ConsolidationOp{
			Op: merged.Op, Kind: ConsolidateMerge, RecordKind: consolidationOpRecordKind(plan, merged.Op),
			ProjectID: consolidationOpProject(plan, merged.Op), IDs: consolidationOpIDs(plan, merged.Op),
			Merge: merged.Manifest,
		})
	}
	for _, archived := range result.Archived {
		manifest.Ops = append(manifest.Ops, ports.ConsolidationOp{
			Op: archived.Op, Kind: ConsolidateArchive, RecordKind: archived.Kind,
			ProjectID: consolidationOpProject(plan, archived.Op), IDs: []string{archived.ID},
		})
	}
	ref, err := recorder.SaveConsolidationManifest(ctx, manifest)
	if err != nil {
		return result, fmt.Errorf("consolidation applied but the manifest could not be written (roll back each merge manifest and restore the archived records): %w", err)
	}
	result.Manifest = ref
	journalErr := a.journalWrite(ctx, JournalConsolidate, "", "",
		fmt.Sprintf("%d merged, %d archived: %s", len(result.Merged), len(result.Archived), ref))
	if journalErr != nil {
		return result, fmt.Errorf("consolidation applied but the journal append failed: %w", journalErr)
	}
	return result, nil
}

func (a *App) lockConsolidation(ctx context.Context) (func(), error) {
	if locker, ok := a.deps.Store.(ports.SyncLocker); ok {
		return locker.LockSync(ctx)
	}
	return func() {}, nil
}

// consolidationApplyMerge runs one merge op and records the outcome. A merge
// whose plan conflicts is skipped, not fatal, so one bad cluster does not
// block the rest of the run.
func (a *App) consolidationApplyMerge(ctx context.Context, op ConsolidateOp, result *ConsolidateApplyResult) {
	if len(op.IDs) < 2 {
		result.Skipped = append(result.Skipped, ConsolidateSkip{Op: op.ID, Reason: "a merge needs a survivor and a duplicate"})
		return
	}
	projectID := op.ProjectID
	outcome, err := a.Merge(ctx, MergeOptions{
		ProjectID: projectID, Survivor: op.IDs[0], Duplicates: op.IDs[1:], Apply: true,
	})
	if err != nil {
		reason := err.Error()
		if len(outcome.Plan.Conflicts) > 0 {
			reason = strings.Join(outcome.Plan.Conflicts, "; ")
		}
		result.Skipped = append(result.Skipped, ConsolidateSkip{Op: op.ID, Reason: reason})
		return
	}
	if outcome.Manifest == "" && !outcome.Plan.Changed {
		// An idempotent repeat: the merge is already in place, so there is
		// nothing to roll back and nothing to record.
		result.Skipped = append(result.Skipped, ConsolidateSkip{Op: op.ID, Reason: "already applied"})
		return
	}
	result.Merged = append(result.Merged, ConsolidateMergeResult{
		Op: op.ID, Survivor: op.IDs[0], Manifest: outcome.Manifest, Files: outcome.Files,
	})
}

// consolidationApplyArchive trashes one record after revalidating it at apply
// time. A record that disappeared, changed, gained links, is too young, or is
// otherwise no longer eligible is skipped as a stale op.
func (a *App) consolidationApplyArchive(ctx context.Context, op ConsolidateOp, archiveAfterDays int, result *ConsolidateApplyResult) {
	if len(op.IDs) != 1 {
		result.Skipped = append(result.Skipped, ConsolidateSkip{Op: op.ID, Reason: "an archive names exactly one record"})
		return
	}
	if reason := a.revalidateArchiveOp(ctx, op, archiveAfterDays); reason != "" {
		result.Skipped = append(result.Skipped, ConsolidateSkip{Op: op.ID, Reason: reason})
		return
	}
	record, err := a.TrashRecord(ctx, op.ProjectID, op.IDs[0])
	if err != nil {
		result.Skipped = append(result.Skipped, ConsolidateSkip{Op: op.ID, Reason: err.Error()})
		return
	}
	summary := record.Summary()
	result.Archived = append(result.Archived, ConsolidateArchiveResult{Op: op.ID, Kind: summary.Kind, ID: summary.ID})
}

func (a *App) revalidateArchiveOp(ctx context.Context, op ConsolidateOp, archiveAfterDays int) string {
	records, err := a.projectRecords(ctx, op.ProjectID)
	if err != nil {
		return err.Error()
	}
	var target *Record
	for i := range records {
		if records[i].Summary().ID == op.IDs[0] {
			target = &records[i]
			break
		}
	}
	if target == nil {
		return "record no longer exists"
	}
	if consolidateArchiveFingerprint(*target) != op.Fingerprint {
		return "stale op: record changed since plan"
	}
	summary := target.Summary()
	if summary.Pinned {
		return "stale op: record is pinned"
	}
	if !consolidateArchivable(*target) {
		return "stale op: record is no longer superseded/archivable"
	}
	if anchor := consolidateArchiveAnchor(*target); anchor.IsZero() || anchor.After(a.now().AddDate(0, 0, -archiveAfterDays)) {
		return "stale op: record is no longer old enough"
	}
	if !a.archiveWithoutLinks(op.ProjectID, records, summary.ID) {
		return "stale op: record has live links"
	}
	if err := a.checkProtectedSelector(op.ProjectID, summary.Kind, summary.Title, false); err != nil {
		return "stale op: " + err.Error()
	}
	return ""
}

// selectConsolidationOps picks the ops an apply executes: the named ids in
// plan order, or every executable op. Unknown or duplicated ids are validation
// errors, so a stale plan cannot silently shrink.
func selectConsolidationOps(plan ConsolidatePlan, ids []string) ([]ConsolidateOp, error) {
	if len(ids) == 0 {
		var out []ConsolidateOp
		for _, op := range plan.Ops {
			if op.Kind == ConsolidateMerge || op.Kind == ConsolidateArchive {
				out = append(out, op)
			}
		}
		return out, nil
	}
	byID := make(map[string]ConsolidateOp, len(plan.Ops))
	for _, op := range plan.Ops {
		byID[op.ID] = op
	}
	selected := make([]ConsolidateOp, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if seen[id] {
			return nil, domain.NewValidationError("ops", fmt.Sprintf("op %q is named twice", id))
		}
		seen[id] = true
		op, ok := byID[id]
		if !ok {
			return nil, domain.NewValidationError("ops",
				fmt.Sprintf("op %q is not in the current plan; run the plan phase again (ops are re-derived from the records, never replayed)", id))
		}
		selected = append(selected, op)
	}
	return selected, nil
}

// consolidationOpProject, consolidationOpRecordKind and consolidationOpIDs
// look an op's metadata up in the plan (used to fill the manifest).
func consolidationOpProject(plan ConsolidatePlan, opID string) string {
	for _, op := range plan.Ops {
		if op.ID == opID {
			return op.ProjectID
		}
	}
	return ""
}

func consolidationOpRecordKind(plan ConsolidatePlan, opID string) string {
	for _, op := range plan.Ops {
		if op.ID == opID {
			return op.RecordKind
		}
	}
	return ""
}

func consolidationOpIDs(plan ConsolidatePlan, opID string) []string {
	for _, op := range plan.Ops {
		if op.ID == opID {
			return op.IDs
		}
	}
	return nil
}

// ConsolidateRollback restores one consolidation manifest: every merge is
// rolled back byte for byte and every archived record is restored from the
// trash. Ops are undone in reverse order.
func (a *App) ConsolidateRollback(ctx context.Context, manifestRef string) (ConsolidateRollbackResult, error) {
	if err := a.requireStore(); err != nil {
		return ConsolidateRollbackResult{}, err
	}
	recorder, ok := a.deps.Store.(ports.ConsolidationRecorder)
	if !ok {
		return ConsolidateRollbackResult{}, fmt.Errorf("%w: this store cannot read a consolidation manifest (local and git modes only)", domain.ErrUnsupported)
	}
	manifest, err := recorder.LoadConsolidationManifest(ctx, manifestRef)
	if err != nil {
		return ConsolidateRollbackResult{}, err
	}
	result := ConsolidateRollbackResult{Manifest: manifestRef}
	if manifest.Status == ports.ConsolidationRolledBack {
		result.AlreadyRolledBack = true
		return result, nil
	}
	for i := len(manifest.Ops) - 1; i >= 0; i-- {
		op := manifest.Ops[i]
		switch op.Kind {
		case ConsolidateMerge:
			if strings.TrimSpace(op.Merge) == "" {
				result.Conflicts = append(result.Conflicts, fmt.Sprintf("%s: merge op without a merge manifest", op.Op))
				continue
			}
			outcome, err := a.RollbackMerge(ctx, op.Merge)
			if err != nil {
				result.Conflicts = append(result.Conflicts, fmt.Sprintf("%s: %s", op.Op, err.Error()))
				if len(outcome.Conflicts) > 0 {
					result.Conflicts = append(result.Conflicts, outcome.Conflicts...)
				}
				continue
			}
			if !outcome.AlreadyRolledBack {
				result.Merges++
			}
		case ConsolidateArchive:
			if len(op.IDs) != 1 {
				result.Conflicts = append(result.Conflicts, fmt.Sprintf("%s: archive op without exactly one id", op.Op))
				continue
			}
			if _, err := a.RestoreRecord(ctx, op.ProjectID, op.IDs[0]); err != nil {
				result.Conflicts = append(result.Conflicts, fmt.Sprintf("%s: %s", op.Op, err.Error()))
				continue
			}
			result.Archived++
		}
	}
	if len(result.Conflicts) > 0 {
		return result, fmt.Errorf("consolidation rollback %s refused or incomplete: %w", manifestRef, domain.ErrConflict)
	}
	if err := recorder.MarkConsolidationRolledBack(ctx, manifestRef, a.now().UTC()); err != nil {
		return result, fmt.Errorf("consolidation rollback restored the records but the manifest could not be stamped: %w", err)
	}
	if a.deps.Index != nil {
		if _, reindexErr := a.Reindex(ctx, IndexOptions{}); reindexErr != nil {
			a.markIndexDirty()
			return result, fmt.Errorf("consolidation rollback restored the records but the index rebuild failed: %w", reindexErr)
		}
		result.Reindexed = true
	}
	if err := a.journalWrite(ctx, JournalConsolidateRollback, "", "", "restored "+manifestRef); err != nil {
		return result, fmt.Errorf("consolidation rollback restored the records but the journal append failed: %w", err)
	}
	return result, nil
}
