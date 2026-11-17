package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// consolidateFake records the consolidation requests and answers with a canned
// result, or a canned error for the conflict case.
type consolidateFake struct {
	fullFake
	inputs []ConsolidateInput
	result ConsolidateResult
	err    error
}

func (f *consolidateFake) Consolidate(_ context.Context, in ConsolidateInput) (ConsolidateResult, error) {
	f.inputs = append(f.inputs, in)
	if f.err != nil {
		return ConsolidateResult{}, f.err
	}
	return f.result, nil
}

// consolidatePlanResult is a minimal plan result for the handler tests.
func consolidatePlanResult() ConsolidateResult {
	return ConsolidateResult{
		Phase: "plan",
		Plan: ConsolidatePlan{
			Projects:    1,
			Fingerprint: "fingerprint-1",
			Ops: []ConsolidateOp{{
				ID: "merge-note-1", Kind: "merge", ProjectID: "alpha", RecordKind: "note",
				IDs: []string{"note-1", "note-2"}, Reason: "exact duplicate", Risk: "low",
			}},
			Near: []ConsolidateCluster{{
				ProjectID: "alpha", Kind: "note", Similarity: 0.9,
				Members: []ConsolidateClusterMem{{ID: "note-3", Title: "left"}, {ID: "note-4", Title: "right"}},
			}},
		},
	}
}

func TestJudgeConsolidateDefaultsToPlanAndForwardsTheProject(t *testing.T) {
	backend := &consolidateFake{result: consolidatePlanResult()}
	result := toolCall(t, backend, "brain_judge", `{"action":"consolidate","project":"alpha"}`)
	if result["isError"] != false {
		t.Fatalf("plan refused: %v", result)
	}
	if len(backend.inputs) != 1 {
		t.Fatalf("backend inputs = %d", len(backend.inputs))
	}
	in := backend.inputs[0]
	if in.Phase != "plan" || in.ProjectID != "alpha" || in.Fingerprint != "" {
		t.Fatalf("input = %+v", in)
	}
	page := structured(t, result)
	if page["phase"] != "plan" {
		t.Fatalf("phase = %v", page["phase"])
	}
	if _, applied := page["applied"]; applied {
		t.Fatalf("plan returned an apply outcome: %v", page)
	}
	plan := page["plan"].(map[string]any)
	if plan["fingerprint"] != "fingerprint-1" {
		t.Fatalf("fingerprint = %v", plan["fingerprint"])
	}
	ops := plan["ops"].([]any)
	if len(ops) != 1 || ops[0].(map[string]any)["id"] != "merge-note-1" {
		t.Fatalf("ops = %v", ops)
	}
}

func TestJudgeConsolidateApplyForwardsIdsFingerprintAndCap(t *testing.T) {
	backend := &consolidateFake{result: func() ConsolidateResult {
		out := consolidatePlanResult()
		out.Phase = "apply"
		out.Applied = &ConsolidateApply{
			Manifest: "backups/consolidate-20260101T000000Z/manifest.json",
			Merged:   []ConsolidateMerged{{Op: "merge-note-1", Survivor: "note-1", Manifest: "merges/1.json"}},
			Archived: []ConsolidateArchived{{Op: "archive-note-9", Kind: "note", ID: "note-9"}},
		}
		return out
	}()}
	result := toolCall(t, backend, "brain_judge",
		`{"action":"consolidate","project":"alpha","phase":"apply","ids":["merge-note-1"],"fingerprint":"fingerprint-1"}`)
	if result["isError"] != false {
		t.Fatalf("apply refused: %v", result)
	}
	in := backend.inputs[0]
	if in.Phase != "apply" || in.Fingerprint != "fingerprint-1" {
		t.Fatalf("input = %+v", in)
	}
	if len(in.IDs) != 1 || in.IDs[0] != "merge-note-1" {
		t.Fatalf("ids = %v", in.IDs)
	}
	page := structured(t, result)
	applied := page["applied"].(map[string]any)
	if applied["manifest"] == "" {
		t.Fatalf("applied = %v", applied)
	}
	if merged := applied["merged"].([]any); len(merged) != 1 || merged[0].(map[string]any)["survivor"] != "note-1" {
		t.Fatalf("merged = %v", merged)
	}
}

func TestJudgeConsolidateApplyRequiresFingerprintAndIds(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"no fingerprint", `{"action":"consolidate","phase":"apply","ids":["merge-note-1"]}`},
		{"no ids", `{"action":"consolidate","phase":"apply","fingerprint":"fingerprint-1"}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			backend := &consolidateFake{result: consolidatePlanResult()}
			result := toolCall(t, backend, "brain_judge", test.args)
			if result["isError"] != true {
				t.Fatalf("accepted: %v", result)
			}
			if code := structured(t, result)["code"]; code != RefusalInvalidArguments {
				t.Fatalf("code = %v, want %s", code, RefusalInvalidArguments)
			}
			if len(backend.inputs) != 0 {
				t.Fatalf("a refused apply reached the backend: %+v", backend.inputs)
			}
		})
	}
}

func TestJudgeConsolidateRefusesUnknownPhaseAndCapsIds(t *testing.T) {
	ids := make([]string, 0, maxConsolidateOps+1)
	for i := 0; i < maxConsolidateOps+1; i++ {
		ids = append(ids, fmt.Sprintf("merge-note-%d", i))
	}
	idsPayload, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args string
	}{
		{"unknown phase", `{"action":"consolidate","phase":"go"}`},
		{"too many ids", fmt.Sprintf(`{"action":"consolidate","phase":"apply","ids":%s,"fingerprint":"fingerprint-1"}`, idsPayload)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			backend := &consolidateFake{result: consolidatePlanResult()}
			result := toolCall(t, backend, "brain_judge", test.args)
			if result["isError"] != true {
				t.Fatalf("accepted: %v", result)
			}
			if code := structured(t, result)["code"]; code != RefusalInvalidArguments {
				t.Fatalf("code = %v, want %s", code, RefusalInvalidArguments)
			}
			if len(backend.inputs) != 0 {
				t.Fatalf("a refused call reached the backend: %+v", backend.inputs)
			}
		})
	}
}

func TestJudgeConsolidateConflictsAreTyped(t *testing.T) {
	backend := &consolidateFake{err: fmt.Errorf("consolidation: the records changed: %w", domain.ErrConflict)}
	result := toolCall(t, backend, "brain_judge",
		`{"action":"consolidate","phase":"apply","ids":["merge-note-1"],"fingerprint":"stale"}`)
	if result["isError"] != true {
		t.Fatalf("conflict not an error: %v", result)
	}
	refusal := structured(t, result)
	if refusal["code"] != RefusalConflict {
		t.Fatalf("code = %v, want %s", refusal["code"], RefusalConflict)
	}
	if _, retryable := refusal["retryable"]; !retryable {
		t.Fatalf("conflict should be retryable: %v", refusal)
	}
}

func TestJudgeConsolidateWithoutBackendIsUnsupported(t *testing.T) {
	result := toolCall(t, &fullFake{}, "brain_judge", `{"action":"consolidate"}`)
	if result["isError"] != true {
		t.Fatalf("accepted without a backend: %v", result)
	}
	if code := structured(t, result)["code"]; code != RefusalUnsupported {
		t.Fatalf("code = %v, want %s", code, RefusalUnsupported)
	}
}

func TestJudgeConsolidateConfirmationOnlyOnApply(t *testing.T) {
	if confirmation := confirmJudgeAction(json.RawMessage(`{"action":"consolidate","phase":"plan"}`)); confirmation != nil {
		t.Fatalf("plan asked for confirmation: %+v", confirmation)
	}
	apply := confirmJudgeAction(json.RawMessage(`{"action":"consolidate","phase":"apply","ids":["merge-note-1"],"fingerprint":"fingerprint-1"}`))
	if apply == nil || !strings.Contains(apply.Message, "1 consolidation op") {
		t.Fatalf("apply confirmation = %+v", apply)
	}
	all := confirmJudgeAction(json.RawMessage(`{"action":"consolidate","phase":"apply","fingerprint":"fingerprint-1"}`))
	if all == nil || !strings.Contains(all.Message, "every executable") {
		t.Fatalf("apply-all confirmation = %+v", all)
	}
	other := confirmJudgeAction(json.RawMessage(`{"action":"judge","from":"a","to":"b","verdict":"relates_to","reason":"x"}`))
	if other == nil || strings.Contains(other.Message, "consolidation") {
		t.Fatalf("a non-consolidate action got a consolidate confirmation: %+v", other)
	}
}
