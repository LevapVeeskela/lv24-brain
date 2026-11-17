package ports

import (
	"context"
	"time"
)

// Consolidation run states of a manifest.
const (
	ConsolidationApplied    = "applied"
	ConsolidationRolledBack = "rolled_back"
)

// ConsolidationOp is one recorded operation of an applied consolidation run.
// Merge ops name the store's own merge manifest, so a rollback can restore the
// exact bytes; archive ops name the trashed record, so a rollback can restore
// it.
type ConsolidationOp struct {
	// Op is the deterministic plan id of the operation, "merge-<survivor>" or
	// "archive-<record id>".
	Op string `json:"op"`
	// Kind is merge or archive.
	Kind string `json:"kind"`
	// ProjectID and RecordKind locate the records.
	ProjectID  string `json:"project_id"`
	RecordKind string `json:"record_kind"`
	// IDs are the record ids the op touched: the survivor first for a merge,
	// the single record for an archive.
	IDs []string `json:"ids"`
	// Fingerprint is the plan fingerprint the apply verified.
	Fingerprint string `json:"fingerprint"`
	// Merge is the data-directory-relative merge manifest (merges/<ts>.json);
	// empty for an archive op.
	Merge string `json:"merge,omitempty"`
}

// ConsolidationManifest is the on-disk record of one applied consolidation
// run. It lives under backups/consolidate-<ts>/manifest.json and is the input
// of a rollback.
type ConsolidationManifest struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Status    string    `json:"status"`
	// RolledBackAt is set after a successful rollback.
	RolledBackAt time.Time         `json:"rolled_back_at,omitzero"`
	Ops          []ConsolidationOp `json:"ops"`
}

// ConsolidationRecorder is implemented by stores that can persist the
// manifest of an applied consolidation run under the data directory. It is
// optional: a store without it keeps consolidation proposal-only.
type ConsolidationRecorder interface {
	// SaveConsolidationManifest writes a new manifest and returns its
	// data-directory-relative reference.
	SaveConsolidationManifest(ctx context.Context, manifest ConsolidationManifest) (string, error)
	// LoadConsolidationManifest reads one manifest by reference; a bare file
	// name is looked up under backups/.
	LoadConsolidationManifest(ctx context.Context, ref string) (ConsolidationManifest, error)
	// MarkConsolidationRolledBack stamps a manifest as rolled back after a
	// successful restore.
	MarkConsolidationRolledBack(ctx context.Context, ref string, at time.Time) error
}
