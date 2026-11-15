package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
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
// tool only reports; fixing stays in the shell (`brain review --fix`).
type ReviewBackend interface {
	Review(ctx context.Context, projectID string) (Review, error)
}

// DueReview is one record or proposal whose review date has passed.
type DueReview struct {
	Kind        string    `json:"kind"`
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	ReviewAfter time.Time `json:"review_after"`
	OverdueDays int       `json:"overdue_days"`
	Source      string    `json:"source"`
}

// DueReviewBackend is implemented by backends that can report the review queue.
type DueReviewBackend interface {
	DueReviews(ctx context.Context, projectID string) ([]DueReview, error)
}

// DoctorCheck is one compatibility probe.
type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// DoctorBackend is implemented by backends that can run the doctor checks.
type DoctorBackend interface {
	Doctor(ctx context.Context) ([]DoctorCheck, error)
}

func (s *Server) reviewTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_review",
		Annotations: readOnlyAnnotations("Review stale knowledge"),
		Description: "Find stale or contradictory knowledge in a project: action=findings (default) reports a next action behind the latest handoff, " +
			"open questions answered by later decisions, decisions contradicted by later attempts and expired state; " +
			"action=due lists records and proposals whose review date passed; action=doctor runs the engine, data, index and lock checks. " +
			"Run `brain review --fix` in a shell to apply the safe fixes.",
		InputSchema: objectSchema(nil, map[string]any{
			"action":     enumProp("findings (default), due or doctor.", []string{"findings", "due", "doctor"}),
			"project":    project,
			"due_before": stringProp("With action=due, keep items whose review date is at or before this instant (RFC3339 or 2026-11-01)."),
			"severity":   enumProp("With action=findings, keep only this severity.", findingSeverities),
		}),
		OutputSchema: outputObject(map[string]any{
			"action":     stringProp("Action that ran."),
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
			"due": arrayProp("Items whose review date passed.", outputObject(map[string]any{
				"kind":         stringProp("note, decision, attempt, handoff, state or proposal."),
				"id":           stringProp("Record, state or proposal id."),
				"title":        stringProp("Human summary."),
				"review_after": stringProp("RFC3339 review date."),
				"overdue_days": intProp("Days past the review date.", 0),
				"source":       stringProp("frontmatter, default or proposal."),
			}, "kind", "id", "title", "review_after")),
			"checks": arrayProp("Doctor checks.", outputObject(map[string]any{
				"name":    stringProp("Check name."),
				"status":  stringProp("ok, warn or fail."),
				"message": stringProp("What the check observed."),
				"code":    stringProp("Machine code of a finding."),
			}, "name", "status", "message")),
		}),
		ReadActions: []string{"findings", "due", "doctor"},
		run:         s.review,
	}
}

// reviewArgs is one brain_review request.
type reviewArgs struct {
	Action    string `json:"action"`
	Project   string `json:"project"`
	DueBefore string `json:"due_before"`
	Severity  string `json:"severity"`
}

func (s *Server) review(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args reviewArgs
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	action := strings.TrimSpace(args.Action)
	if action == "" {
		action = "findings"
	}
	switch action {
	case "findings":
		return s.reviewFindings(ctx, args)
	case "due":
		return s.reviewDue(ctx, args)
	case "doctor":
		return s.reviewDoctor(ctx, args)
	default:
		return toolResult{}, refusalInvalidf("unknown action %q (findings, due, doctor)", action)
	}
}

func (s *Server) reviewFindings(ctx context.Context, args reviewArgs) (toolResult, error) {
	backend, ok := s.backend.(ReviewBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_review(action=findings)", "brain review")
	}
	severity := strings.TrimSpace(args.Severity)
	if severity != "" && !slices.Contains(findingSeverities, severity) {
		return toolResult{}, refusalInvalidf("unknown severity %q (%s)", severity, strings.Join(findingSeverities, ", "))
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	result, err := backend.Review(ctx, target.ID)
	if err != nil {
		return toolResult{}, err
	}
	if severity != "" {
		filtered := make([]ReviewFinding, 0, len(result.Findings))
		for _, finding := range result.Findings {
			if finding.Severity == severity {
				filtered = append(filtered, finding)
			}
		}
		result.Findings = filtered
	}
	if result.Findings == nil {
		result.Findings = []ReviewFinding{}
	}
	return toolResult{Text: reviewText(result), Structured: structuredReview{Action: "findings", Review: result}}, nil
}

func (s *Server) reviewDue(ctx context.Context, args reviewArgs) (toolResult, error) {
	backend, ok := s.backend.(DueReviewBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_review(action=due)", "brain review --due")
	}
	var before time.Time
	if strings.TrimSpace(args.DueBefore) != "" {
		parsed, err := parseInstant(args.DueBefore)
		if err != nil {
			return toolResult{}, refusalInvalid("due_before: " + err.Error())
		}
		before = parsed
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	due, err := backend.DueReviews(ctx, target.ID)
	if err != nil {
		return toolResult{}, err
	}
	if due == nil {
		due = []DueReview{}
	}
	if !before.IsZero() {
		filtered := make([]DueReview, 0, len(due))
		for _, item := range due {
			if !item.ReviewAfter.After(before) {
				filtered = append(filtered, item)
			}
		}
		due = filtered
	}
	var text strings.Builder
	if len(due) == 0 {
		text.WriteString("nothing due for review")
	}
	for _, item := range due {
		fmt.Fprintf(&text, "- %s %s %s (due %s, %dx)\n", item.Kind, item.ID, item.Title,
			item.ReviewAfter.UTC().Format(time.RFC3339), item.OverdueDays)
	}
	return toolResult{
		Text:       strings.TrimRight(text.String(), "\n"),
		Structured: structuredDue{Action: "due", ProjectID: target.ID, Due: due},
	}, nil
}

func (s *Server) reviewDoctor(ctx context.Context, args reviewArgs) (toolResult, error) {
	if strings.TrimSpace(args.DueBefore) != "" || strings.TrimSpace(args.Severity) != "" || strings.TrimSpace(args.Project) != "" {
		return toolResult{}, refusalInvalidWith("action=doctor checks the whole data directory and accepts no project, due_before or severity",
			"brain_review(action=doctor) without arguments")
	}
	backend, ok := s.backend.(DoctorBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_review(action=doctor)", "brain doctor")
	}
	checks, err := backend.Doctor(ctx)
	if err != nil {
		return toolResult{}, err
	}
	if checks == nil {
		checks = []DoctorCheck{}
	}
	var text strings.Builder
	fmt.Fprintf(&text, "doctor: %d checks", len(checks))
	for _, check := range checks {
		fmt.Fprintf(&text, "\n- [%s] %s: %s", check.Status, check.Name, check.Message)
	}
	return toolResult{
		Text:       text.String(),
		Structured: structuredDoctor{Action: "doctor", Checks: checks},
	}, nil
}

// structuredReview adds the action to the historical review result.
type structuredReview struct {
	Action string `json:"action"`
	Review
}

// structuredDue is the brain_review(action=due) result.
type structuredDue struct {
	Action    string      `json:"action"`
	ProjectID string      `json:"project_id"`
	Due       []DueReview `json:"due"`
}

// structuredDoctor is the brain_review(action=doctor) result.
type structuredDoctor struct {
	Action string        `json:"action"`
	Checks []DoctorCheck `json:"checks"`
}

// parseInstant accepts an RFC3339 time or a plain date.
func parseInstant(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at, nil
	}
	if at, err := time.Parse("2006-01-02", value); err == nil {
		return at, nil
	}
	return time.Time{}, fmt.Errorf("want RFC3339 or 2006-01-02, got %q", value)
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
