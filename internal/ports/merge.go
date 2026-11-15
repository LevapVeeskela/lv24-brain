package ports

import "context"

// Merge record states of a plan.
const (
	// MergeLive marks a live record the merge absorbs.
	MergeLive = "live"
	// MergeTombstone marks a record that was already absorbed and is trashed.
	MergeTombstone = "tombstone"
)

// Merge change labels of a touched file.
const (
	MergeChangeSurvivor  = "survivor"
	MergeChangeTombstone = "tombstone"
	MergeChangeLink      = "link"
	MergeChangeRelation  = "relation"
	MergeChangeMove      = "move"
)

// MergeRecord is one record a merge plan touches.
type MergeRecord struct {
	Kind  string
	ID    string
	Title string
	// State is MergeLive or MergeTombstone.
	State string
}

// MergeLinkRewrite is one reference the merge rewrites from a duplicate to the
// survivor, either a [[id]] link in a record body or a relation target.
type MergeLinkRewrite struct {
	SourceKind string
	SourceID   string
	// Field names the record field holding the reference, or "relation" for
	// a typed relation target.
	Field string
	// Ref is the reference as written (a link target or a relation target).
	Ref   string
	OldID string
	NewID string
}

// MergeRequest is the input of a merge plan.
type MergeRequest struct {
	ProjectID string
	// Survivor is the record that keeps its identity and absorbs the others.
	Survivor string
	// Duplicates are the records to absorb, in merged_from order.
	Duplicates []string
}

// MergePlan is phase one of a merge: everything the apply would change, with
// no write. Conflicts lists the reasons the plan cannot be applied.
type MergePlan struct {
	ProjectID     string
	Kind          string
	Survivor      MergeRecord
	Duplicates    []MergeRecord
	AlreadyMerged []MergeRecord
	Links         []MergeLinkRewrite
	MergedFrom    []string
	Conflicts     []string
	// Fingerprint covers the survivor, the duplicates and every pre-merge file
	// hash, so the apply refuses a plan whose inputs changed after the plan
	// was shown.
	Fingerprint string
	// Changed is false when there is nothing left to do.
	Changed bool
}

// MergeFile is one file a merge or a rollback touched.
type MergeFile struct {
	// Path is relative to the data directory, with forward slashes.
	Path string
	// Change is one of the MergeChange constants.
	Change string
}

// MergeOutcome is the result of applying a plan.
type MergeOutcome struct {
	// Manifest is the data-directory-relative path of the merge manifest,
	// "merges/<UTC>.json"; empty when the plan changed nothing.
	Manifest string
	Files    []MergeFile
}

// RollbackOutcome is the result of restoring a merge manifest.
type RollbackOutcome struct {
	Manifest          string
	Files             []MergeFile
	Conflicts         []string
	AlreadyRolledBack bool
}

// RecordMerger is implemented by stores that keep curated records as files and
// can merge two or more of them in two phases. It is optional: a store without
// it reports that merging is unavailable instead of editing records by hand.
//
// PlanMerge never writes. ApplyMerge re-verifies the plan fingerprint and the
// per-file hashes, writes a manifest before the first change and rolls its own
// partial work back when a write fails. RollbackMerge restores the recorded
// bytes exactly, or reports the files that changed since the merge.
type RecordMerger interface {
	PlanMerge(ctx context.Context, request MergeRequest) (MergePlan, error)
	ApplyMerge(ctx context.Context, plan MergePlan) (MergeOutcome, error)
	RollbackMerge(ctx context.Context, manifest string) (RollbackOutcome, error)
}
