package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Review is the stale-knowledge report of a project.
type Review struct {
	ProjectID string          `json:"project_id"`
	Summary   string          `json:"summary"`
	Findings  []ReviewFinding `json:"findings"`
}

// ReviewFinding is one piece of the brain that looks stale or contradictory.
type ReviewFinding struct {
	Kind string `json:"kind"`
	// Severity is error (context may be wrong), warn (may be stale) or info
	// (curation hint).
	Severity string `json:"severity"`
	// Message describes what was observed, with the ids involved.
	Message string `json:"message"`
	// Why is one sentence about the risk of leaving it alone.
	Why string `json:"why"`
	// Next is the safe next action for the caller.
	Next string `json:"next"`
	// Confirm is what must be verified before acting.
	Confirm  string `json:"confirm"`
	Evidence string `json:"evidence"`
	Fixable  bool   `json:"fixable"`
	Fix      string `json:"fix"`
}

// ReviewBackend is implemented by backends that can review a project. The
// tool only reports; fixing stays in the shell (`brain review --fix`). It is
// listed only for such backends.
type ReviewBackend interface {
	Review(ctx context.Context, projectID string) (Review, error)
}

func (s *Server) reviewTools() []tool {
	if _, ok := s.backend.(ReviewBackend); !ok && !s.catalog {
		return nil
	}
	return []tool{{
		Name:        "brain_review",
		Annotations: readOnlyAnnotations("Review stale knowledge"),
		Description: "Find stale or contradictory knowledge in a project: a next action behind the latest handoff or already done by a commit, " +
			"open questions answered by later decisions, decisions contradicted by later failed attempts, expired state. " +
			"Run `brain review --fix` in a shell to apply the safe fixes.",
		InputSchema: objectSchema(nil, map[string]any{
			"project": stringProp("Project id, alias or checkout path. Omit to use the project of the working directory."),
		}),
		OutputSchema: outputObject(map[string]any{
			"project_id": stringProp("Project that was reviewed."),
			"summary":    stringProp("Rendered review summary, the text content of the result."),
			"findings": arrayProp("Findings that still apply.", outputObject(map[string]any{
				"kind":     stringProp("Finding kind."),
				"severity": enumProp("error (context may be wrong), warn (may be stale) or info (curation hint).", findingSeverities),
				"message":  stringProp("What looks stale or contradictory, with the ids involved."),
				"why":      stringProp("One sentence about the risk of leaving the finding alone."),
				"next":     stringProp("The safe next action for the caller."),
				"confirm":  stringProp("What to verify before acting on the finding."),
				"evidence": stringProp("The record or commit that supports the finding."),
				"fixable":  boolProp("Whether `brain review --fix` repairs it."),
				"fix":      stringProp("The fix that applies."),
			}, "kind", "severity", "message", "why", "next", "confirm", "evidence", "fixable", "fix")),
		}, "project_id", "summary", "findings"),
		run: s.review,
	}}
}

func (s *Server) review(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Project string `json:"project"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	result, err := s.backend.(ReviewBackend).Review(ctx, target.ID)
	if err != nil {
		return toolResult{}, err
	}
	if result.Findings == nil {
		result.Findings = []ReviewFinding{}
	}
	return toolResult{Text: reviewText(result), Structured: result}, nil
}

func reviewText(review Review) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(review.Summary, "\n"))
	b.WriteString("\n## Findings\n")
	if len(review.Findings) == 0 {
		b.WriteString("- none\n")
	}
	for _, finding := range review.Findings {
		prefix := ""
		if finding.Severity != "" {
			prefix = "[" + finding.Severity + "] "
		}
		why := finding.Why
		if why == "" {
			why = finding.Message
		}
		fmt.Fprintf(&b, "- %s%s: %s", prefix, finding.Kind, strings.TrimSuffix(why, "."))
		if finding.Next != "" {
			fmt.Fprintf(&b, " -> %s", finding.Next)
		}
		b.WriteString("\n")
		if finding.Why != "" && finding.Message != "" {
			fmt.Fprintf(&b, "  found: %s\n", finding.Message)
		}
		if finding.Evidence != "" {
			fmt.Fprintf(&b, "  evidence: %s\n", finding.Evidence)
		}
		if finding.Confirm != "" {
			fmt.Fprintf(&b, "  confirm: %s\n", finding.Confirm)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// findingSeverities is the closed severity set of a review finding.
var findingSeverities = []string{"error", "warn", "info"}
