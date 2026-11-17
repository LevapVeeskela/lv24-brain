package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

// maxConsolidateOps bounds one brain_judge(action=consolidate) apply; it mirrors
// the engine's plan cap.
const maxConsolidateOps = app.ConsolidateMaxOps

// ConsolidateBackend is implemented by backends that can plan and apply the
// proposal-only consolidation job.
type ConsolidateBackend interface {
	Consolidate(ctx context.Context, in ConsolidateInput) (ConsolidateResult, error)
}

// ConsolidateInput is one brain_judge(action=consolidate) request. Phase plan
// is read-only; phase apply executes only the named op ids and refuses a
// stale fingerprint. The plan cap is the engine default (50 ops).
type ConsolidateInput struct {
	ProjectID string `json:"project_id"`
	// Phase is plan (default) or apply.
	Phase string `json:"phase"`
	// IDs are the op ids to apply (phase=apply, required): the tool never
	// applies a whole plan blind.
	IDs []string `json:"ids"`
	// Fingerprint is the plan fingerprint from phase=plan; required for apply.
	Fingerprint string `json:"fingerprint"`
}

// ConsolidateOp is one proposed operation.
type ConsolidateOp struct {
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

// ConsolidateCluster is one reported near cluster (never merged by the job).
type ConsolidateCluster struct {
	ProjectID  string                  `json:"project_id"`
	Kind       string                  `json:"kind"`
	Reason     string                  `json:"reason"`
	Similarity float64                 `json:"similarity"`
	Members    []ConsolidateClusterMem `json:"members"`
}

// ConsolidateClusterMem is one member of a reported near cluster.
type ConsolidateClusterMem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ConsolidatePlan is the proposal-only plan.
type ConsolidatePlan struct {
	Projects    int                  `json:"projects"`
	Ops         []ConsolidateOp      `json:"ops"`
	Near        []ConsolidateCluster `json:"near"`
	Fingerprint string               `json:"fingerprint"`
	Truncated   bool                 `json:"truncated"`
}

// ConsolidateApply reports what an apply wrote.
type ConsolidateApply struct {
	Manifest string                 `json:"manifest"`
	Merged   []ConsolidateMerged    `json:"merged"`
	Archived []ConsolidateArchived  `json:"archived"`
	Skipped  []ConsolidateApplySkip `json:"skipped"`
}

// ConsolidateMerged is one applied merge.
type ConsolidateMerged struct {
	Op       string `json:"op"`
	Survivor string `json:"survivor"`
	Manifest string `json:"manifest"`
}

// ConsolidateArchived is one applied archive.
type ConsolidateArchived struct {
	Op   string `json:"op"`
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ConsolidateApplySkip is an op that was not applied.
type ConsolidateApplySkip struct {
	Op     string `json:"op"`
	Reason string `json:"reason"`
}

// ConsolidateResult is the phase-dependent result: the plan is always set,
// the apply outcome only after phase=apply.
type ConsolidateResult struct {
	Phase   string            `json:"phase"`
	Plan    ConsolidatePlan   `json:"plan"`
	Applied *ConsolidateApply `json:"applied,omitempty"`
}

// judgeConsolidate runs the proposal-only consolidation job. Plan never
// writes; apply re-derives the plan from the records, so a stale op id or
// fingerprint is refused instead of replayed.
func (s *Server) judgeConsolidate(ctx context.Context, args judgeArgs) (toolResult, error) {
	backend, ok := s.backend.(ConsolidateBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_judge(action=consolidate)", "brain consolidate")
	}
	phase := strings.TrimSpace(args.Phase)
	if phase == "" {
		phase = "plan"
	}
	if phase != "plan" && phase != "apply" {
		return toolResult{}, refusalInvalidf("unknown consolidation phase %q (plan or apply)", args.Phase)
	}
	in := ConsolidateInput{
		ProjectID: args.Project, Phase: phase, IDs: args.IDs,
		Fingerprint: strings.TrimSpace(args.Fingerprint),
	}
	if phase == "apply" {
		if in.Fingerprint == "" {
			return toolResult{}, refusalInvalidWith(
				"fingerprint is required with phase=apply: run phase=plan first and pass the plan fingerprint",
				"brain_judge action=consolidate phase=plan, then repeat the plan fingerprint with the op ids")
		}
		if len(in.IDs) == 0 {
			return toolResult{}, refusalInvalidWith(
				"ids is required with phase=apply: name the ops you reviewed",
				"brain_judge action=consolidate phase=plan, then apply with the op ids you accept")
		}
		if len(in.IDs) > maxConsolidateOps {
			return toolResult{}, refusalInvalidf("ids accepts at most %d ops", maxConsolidateOps)
		}
	}
	result, err := backend.Consolidate(ctx, in)
	if err != nil {
		return toolResult{}, err
	}
	return toolResult{
		Text:       consolidateText(result),
		Structured: result,
	}, nil
}

// consolidateText renders the human-readable half of the tool result.
func consolidateText(result ConsolidateResult) string {
	var out strings.Builder
	if result.Phase == "apply" && result.Applied != nil {
		fmt.Fprintf(&out, "consolidate: %d merged, %d archived, %d skipped; manifest: %s\n",
			len(result.Applied.Merged), len(result.Applied.Archived), len(result.Applied.Skipped), result.Applied.Manifest)
	} else {
		fmt.Fprintf(&out, "consolidate: %d ops proposed (plan-only, nothing written)\n", len(result.Plan.Ops))
	}
	for _, op := range result.Plan.Ops {
		fmt.Fprintf(&out, "- [%s] %s %s (%s): %s\n", op.Kind, op.ID, strings.Join(op.IDs, ", "), op.Risk, op.Reason)
	}
	if result.Plan.Truncated {
		out.WriteString("plan truncated at the op cap\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

// confirmConsolidate asks the user before an apply writes.
func confirmConsolidate(args json.RawMessage) *confirmation {
	var probe struct {
		Phase string   `json:"phase"`
		IDs   []string `json:"ids"`
	}
	if err := json.Unmarshal(argsOrDefault(args), &probe); err != nil {
		return nil
	}
	if strings.TrimSpace(probe.Phase) != "apply" {
		return nil
	}
	if len(probe.IDs) == 0 {
		return &confirmation{Message: "Apply every executable consolidation op of the fresh plan? Plan is proposal-only; apply writes merges and archives, all reversible."}
	}
	return &confirmation{Message: fmt.Sprintf("Apply %d consolidation op(s)? Plan is proposal-only; apply writes merges and archives, all reversible.", len(probe.IDs))}
}
