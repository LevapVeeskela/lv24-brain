package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/bootstrap"
)

// runConsolidate implements the proposal-only consolidation job:
//
//	brain consolidate [--plan|--apply] [--op <id>]... [--max N] [--json]
//	brain consolidate --rollback <manifest> [--json]
//
// The default phase is --plan: it computes the ops and writes nothing. Apply
// executes only the ops named with --op, or every executable op of the fresh
// plan, and records a manifest under backups/consolidate-<ts>/ that --rollback
// restores.
func runConsolidate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("consolidate", stderr)
	project := fs.String("project", "", "project id or alias")
	scan := fs.Bool("scan", false, "plan every registered project instead of one")
	plan := fs.Bool("plan", false, "print the proposal-only plan (the default)")
	apply := fs.Bool("apply", false, "apply the named ops, or every executable op of the fresh plan")
	var ops stringList
	fs.Var(&ops, "op", "apply only this op id (repeatable or comma-separated)")
	max := fs.Int("max", 0, "cap the number of ops in the plan (default 50)")
	archiveAfter := fs.Int("archive-after", 0, "minimum days a record must have been superseded before archiving (default 30)")
	fingerprint := fs.String("fingerprint", "", "apply refuses a stale plan: pass the plan fingerprint from --plan --json")
	rollback := fs.String("rollback", "", "restore the state recorded in this consolidation manifest")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if *plan && *apply {
		fmt.Fprintln(stderr, "brain consolidate: pass either --plan or --apply, not both")
		return 2
	}
	if len(positional) > 0 {
		fmt.Fprintln(stderr, "brain consolidate: takes no record ids; ops are chosen with --op")
		return 2
	}
	if *scan && strings.TrimSpace(*project) != "" {
		fmt.Fprintln(stderr, "brain consolidate: --scan inspects every registered project; drop the project argument")
		return 2
	}
	opts := app.ConsolidateOptions{
		ProjectID: *project, Scan: *scan, Max: *max, ArchiveAfterDays: *archiveAfter,
		Fingerprint: *fingerprint,
	}
	if *apply {
		opts.Phase = "apply"
	}
	if values := ops.values(); len(values) > 0 {
		opts.Ops = values
	}
	ctx := context.Background()
	var runtime *bootstrap.Runtime
	// --scan and --rollback are data-directory scoped: neither needs a project.
	if *scan || strings.TrimSpace(*rollback) != "" {
		cfg, err := loadRequiredConfig(*cfgPath)
		if err != nil {
			return reportError(stderr, "consolidate", err)
		}
		runtime, err = bootstrap.Build(cfg)
		if err != nil {
			return reportError(stderr, "consolidate", err)
		}
	} else {
		var target app.Resolved
		var code int
		runtime, target, code = stateTarget(ctx, *cfgPath, *project, nil, stderr, "consolidate")
		if code != 0 {
			return code
		}
		if opts.ProjectID == "" {
			opts.ProjectID = target.Project.ID
		}
	}
	defer func() { _ = runtime.Close() }()
	if manifest := strings.TrimSpace(*rollback); manifest != "" {
		if *apply || len(opts.Ops) > 0 {
			fmt.Fprintln(stderr, "brain consolidate: --rollback takes no --apply and no --op")
			return 2
		}
		return runConsolidateRollback(ctx, runtime.App, manifest, *asJSON, stdout, stderr)
	}
	planResult, applied, err := runtime.App.Consolidate(ctx, opts)
	if err != nil {
		return reportError(stderr, "consolidate", err)
	}
	if *asJSON {
		if *apply {
			return writeJSONReport(stdout, stderr, "consolidate", toJSONConsolidateApply(*applied, planResult.Fingerprint))
		}
		return writeJSONReport(stdout, stderr, "consolidate", toJSONConsolidatePlan(planResult))
	}
	printConsolidatePlan(stdout, planResult)
	if !*apply {
		fmt.Fprintln(stdout, "nothing written; re-run with --apply (and --op) to execute")
		return 0
	}
	printConsolidateApply(stdout, *applied)
	return 0
}

// runConsolidateRollback restores one consolidation manifest.
func runConsolidateRollback(ctx context.Context, a *app.App, manifest string, asJSON bool, stdout, stderr io.Writer) int {
	result, err := a.ConsolidateRollback(ctx, manifest)
	if err != nil {
		if len(result.Conflicts) > 0 {
			printMergeConflicts(stderr, result.Conflicts)
		}
		return reportError(stderr, "consolidate --rollback", err)
	}
	if asJSON {
		return writeJSONReport(stdout, stderr, "consolidate --rollback", toJSONConsolidateRollback(result))
	}
	if result.AlreadyRolledBack {
		fmt.Fprintf(stdout, "%s was already rolled back\n", result.Manifest)
		return 0
	}
	fmt.Fprintf(stdout, "rolled back %s (%d merges, %d archives restored", result.Manifest, result.Merges, result.Archived)
	if result.Reindexed {
		fmt.Fprint(stdout, ", index rebuilt")
	}
	fmt.Fprintln(stdout, ")")
	return 0
}

func printConsolidatePlan(w io.Writer, plan app.ConsolidatePlan) {
	scope := fmt.Sprintf("%d project(s)", plan.Projects)
	fmt.Fprintf(w, "# Consolidate - %s\n", scope)
	fmt.Fprintf(w, "fingerprint: %s\n", plan.Fingerprint)
	if plan.Truncated {
		fmt.Fprintln(w, "truncated: the plan hit the op cap; raise --max to see the rest")
	}
	if len(plan.Ops) == 0 && len(plan.Near) == 0 {
		fmt.Fprintln(w, "no consolidation candidates")
		return
	}
	for _, op := range plan.Ops {
		fmt.Fprintf(w, "- [%s] %s %s (%s): %s\n", op.Kind, op.ID, strings.Join(op.IDs, ", "), op.Risk, op.Reason)
		fmt.Fprintf(w, "  undo: %s\n", op.Reversible)
	}
	for _, cluster := range plan.Near {
		ids := make([]string, 0, len(cluster.Members))
		for _, member := range cluster.Members {
			ids = append(ids, member.ID)
		}
		fmt.Fprintf(w, "- [near %d%%] %s %s (%s): report-only, below the %d%% merge threshold\n",
			int(cluster.Similarity*100), cluster.ProjectID, cluster.Kind, strings.Join(ids, ", "),
			int(app.ConsolidateNearMergeThreshold*100))
	}
}

func printConsolidateApply(w io.Writer, result app.ConsolidateApplyResult) {
	for _, merged := range result.Merged {
		fmt.Fprintf(w, "merged %s into %s; manifest: %s (%d files)\n", merged.Op, merged.Survivor, merged.Manifest, len(merged.Files))
	}
	for _, archived := range result.Archived {
		fmt.Fprintf(w, "archived %s %s (restorable with brain record restore)\n", archived.Kind, archived.ID)
	}
	for _, skip := range result.Skipped {
		fmt.Fprintf(w, "skipped %s: %s\n", skip.Op, skip.Reason)
	}
	if result.Manifest != "" {
		fmt.Fprintf(w, "manifest: %s\n", result.Manifest)
	} else {
		fmt.Fprintln(w, "nothing applied")
	}
}

// jsonConsolidateOp is one op of `brain consolidate --json`.
type jsonConsolidateOp struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	ProjectID   string   `json:"project_id"`
	RecordKind  string   `json:"record_kind"`
	IDs         []string `json:"ids"`
	Reason      string   `json:"reason"`
	Fingerprint string   `json:"fingerprint"`
	Risk        string   `json:"risk"`
	Reversible  string   `json:"reversible"`
}

// jsonConsolidatePlan is the machine-readable plan.
type jsonConsolidatePlan struct {
	Projects    int                    `json:"projects"`
	Ops         []jsonConsolidateOp    `json:"ops"`
	Near        []jsonDuplicateCluster `json:"near"`
	Fingerprint string                 `json:"fingerprint"`
	Truncated   bool                   `json:"truncated"`
}

func toJSONConsolidatePlan(plan app.ConsolidatePlan) jsonConsolidatePlan {
	out := jsonConsolidatePlan{
		Projects: plan.Projects, Fingerprint: plan.Fingerprint, Truncated: plan.Truncated,
		Ops:  make([]jsonConsolidateOp, 0, len(plan.Ops)),
		Near: make([]jsonDuplicateCluster, 0, len(plan.Near)),
	}
	for _, op := range plan.Ops {
		out.Ops = append(out.Ops, jsonConsolidateOp{
			ID: op.ID, Kind: op.Kind, ProjectID: op.ProjectID, RecordKind: op.RecordKind,
			IDs: nonNilStrings(op.IDs), Reason: op.Reason, Fingerprint: op.Fingerprint,
			Risk: op.Risk, Reversible: op.Reversible,
		})
	}
	for _, cluster := range plan.Near {
		out.Near = append(out.Near, toJSONDuplicateCluster(cluster))
	}
	return out
}

// jsonConsolidateApply is the machine-readable apply result.
type jsonConsolidateApply struct {
	Fingerprint string                    `json:"fingerprint"`
	Manifest    string                    `json:"manifest"`
	Merged      []jsonConsolidateMerged   `json:"merged"`
	Archived    []jsonConsolidateArchived `json:"archived"`
	Skipped     []jsonConsolidateSkip     `json:"skipped"`
}

type jsonConsolidateMerged struct {
	Op       string `json:"op"`
	Survivor string `json:"survivor"`
	Manifest string `json:"manifest"`
	Files    int    `json:"files"`
}

type jsonConsolidateArchived struct {
	Op   string `json:"op"`
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type jsonConsolidateSkip struct {
	Op     string `json:"op"`
	Reason string `json:"reason"`
}

func toJSONConsolidateApply(result app.ConsolidateApplyResult, fingerprint string) jsonConsolidateApply {
	out := jsonConsolidateApply{
		Fingerprint: fingerprint, Manifest: result.Manifest,
		Merged:   make([]jsonConsolidateMerged, 0, len(result.Merged)),
		Archived: make([]jsonConsolidateArchived, 0, len(result.Archived)),
		Skipped:  make([]jsonConsolidateSkip, 0, len(result.Skipped)),
	}
	for _, merged := range result.Merged {
		out.Merged = append(out.Merged, jsonConsolidateMerged{
			Op: merged.Op, Survivor: merged.Survivor, Manifest: merged.Manifest, Files: len(merged.Files),
		})
	}
	for _, archived := range result.Archived {
		out.Archived = append(out.Archived, jsonConsolidateArchived{Op: archived.Op, Kind: archived.Kind, ID: archived.ID})
	}
	for _, skip := range result.Skipped {
		out.Skipped = append(out.Skipped, jsonConsolidateSkip{Op: skip.Op, Reason: skip.Reason})
	}
	return out
}

// toJSONDuplicateCluster renders a near cluster for the consolidate plan.
func toJSONDuplicateCluster(cluster app.DuplicateCluster) jsonDuplicateCluster {
	members := make([]jsonDuplicateMember, 0, len(cluster.Members))
	for _, member := range cluster.Members {
		members = append(members, jsonDuplicateMember{ID: member.ID, Title: member.Title, CreatedAt: jsonTime(member.CreatedAt)})
	}
	return jsonDuplicateCluster{
		ProjectID: cluster.ProjectID, Kind: cluster.Kind, Reason: cluster.Reason,
		Similarity: cluster.Similarity, Members: members,
	}
}

// jsonConsolidateRollback is the machine-readable rollback result.
type jsonConsolidateRollback struct {
	Manifest          string   `json:"manifest"`
	Merges            int      `json:"merges"`
	Archived          int      `json:"archived"`
	Conflicts         []string `json:"conflicts"`
	AlreadyRolledBack bool     `json:"already_rolled_back"`
	Reindexed         bool     `json:"reindexed"`
}

func toJSONConsolidateRollback(result app.ConsolidateRollbackResult) jsonConsolidateRollback {
	return jsonConsolidateRollback{
		Manifest: result.Manifest, Merges: result.Merges, Archived: result.Archived,
		Conflicts: nonNilStrings(result.Conflicts), AlreadyRolledBack: result.AlreadyRolledBack,
		Reindexed: result.Reindexed,
	}
}
