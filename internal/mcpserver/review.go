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
	Kind     string `json:"kind"`
	Message  string `json:"message"`
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
	if _, ok := s.backend.(ReviewBackend); !ok {
		return nil
	}
	return []tool{{
		Name: "brain_review",
		Description: "Find stale or contradictory knowledge in a project: a next action behind the latest handoff or already done by a commit, " +
			"open questions answered by later decisions, decisions contradicted by later failed attempts, expired state. " +
			"Run `brain review --fix` in a shell to apply the safe fixes.",
		InputSchema: objectSchema(nil, map[string]any{
			"project": stringProp("Project id, alias or checkout path. Omit to use the project of the working directory."),
		}),
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
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
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
		fmt.Fprintf(&b, "- [%s] %s", finding.Kind, finding.Message)
		if finding.Evidence != "" {
			fmt.Fprintf(&b, " (evidence: %s)", finding.Evidence)
		}
		if finding.Fix != "" {
			fmt.Fprintf(&b, " -> fix: %s", finding.Fix)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
