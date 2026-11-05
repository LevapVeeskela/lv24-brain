package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Bootstrap builds a compact agent context from state, decisions, attempts and
// the newest handoff. The summary is bounded by opts.Budget in runes.
func (a *App) Bootstrap(ctx context.Context, opts BootstrapOptions) (BootstrapResult, error) {
	if err := a.requireStore(); err != nil {
		return BootstrapResult{}, err
	}
	project, err := a.resolveProject(ctx, opts.ProjectID)
	if err != nil {
		return BootstrapResult{}, err
	}
	state, err := a.loadState(ctx, project.ID)
	if err != nil {
		return BootstrapResult{}, err
	}
	decisions, err := a.listDecisions(ctx, project.ID)
	if err != nil {
		return BootstrapResult{}, err
	}
	attempts, err := a.listAttempts(ctx, project.ID)
	if err != nil {
		return BootstrapResult{}, err
	}
	handoffs, err := a.listHandoffs(ctx, project.ID)
	if err != nil {
		return BootstrapResult{}, err
	}

	now := a.now()
	var out strings.Builder
	fmt.Fprintf(&out, "# %s — bootstrap\n", project.Name)
	fmt.Fprintf(&out, "project_id: %s\n", project.ID)
	fmt.Fprintf(&out, "mode: %s\n", a.cfg.Mode)
	if revision, revisionErr := a.deps.Store.CurrentRevision(ctx); revisionErr == nil {
		fmt.Fprintf(&out, "revision: %s\n", revision.Value)
	}
	fmt.Fprintf(&out, "state_updated: %s (age %s)\n", formatTime(state.UpdatedAt), stateAge(now, state))
	fmt.Fprintf(&out, "freshness: %s\n", freshness(now, state))
	fmt.Fprintf(&out, "goal: %s\n", valueOr(state.Goal, "not set"))
	fmt.Fprintf(&out, "next_action: %s\n", valueOr(state.NextAction, "not set"))
	writePromptList(&out, "active", state.Active)
	writePromptList(&out, "blocked", state.Blocked)
	writePromptList(&out, "open_questions", state.OpenQuestions)

	out.WriteString("\n## Recent decisions\n")
	if len(decisions) == 0 {
		out.WriteString("- none recorded\n")
	} else {
		for i, decision := range decisions {
			if i >= maxListedItems {
				break
			}
			fmt.Fprintf(&out, "- [%s] %s (%s)\n", decision.Status, decision.Title, formatDate(decision.CreatedAt))
			if line := firstLine(decision.Context); line != "" {
				fmt.Fprintf(&out, "  %s\n", line)
			}
		}
	}

	out.WriteString("\n## Recent attempts\n")
	if len(attempts) == 0 {
		out.WriteString("- none recorded\n")
	} else {
		for i, attempt := range attempts {
			if i >= maxListedItems {
				break
			}
			fmt.Fprintf(&out, "- %s (%s)\n", attempt.Title, formatDate(attempt.CreatedAt))
			if line := firstLine(attempt.Reason); line != "" {
				fmt.Fprintf(&out, "  why: %s\n", line)
			} else if line := firstLine(attempt.Result); line != "" {
				fmt.Fprintf(&out, "  result: %s\n", line)
			}
		}
	}

	out.WriteString("\n## Latest handoff\n")
	if len(handoffs) == 0 {
		out.WriteString("- none recorded\n")
	} else {
		handoff := handoffs[0]
		fmt.Fprintf(&out, "- %s\n", formatTime(handoff.CreatedAt))
		if line := firstLine(handoff.Summary); line != "" {
			fmt.Fprintf(&out, "  summary: %s\n", line)
		}
		if line := firstLine(handoff.NextAction); line != "" {
			fmt.Fprintf(&out, "  next: %s\n", line)
		}
	}

	budget := opts.Budget
	if budget <= 0 {
		budget = DefaultBootstrapBudget
	}
	return BootstrapResult{
		ProjectID: project.ID,
		Summary:   truncateSummary(out.String(), budget),
	}, nil
}

func (a *App) loadState(ctx context.Context, projectID string) (domain.State, error) {
	state, err := a.deps.Store.GetState(ctx, projectID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.State{ProjectID: projectID}, nil
	}
	if err != nil {
		return domain.State{}, err
	}
	return state, nil
}

func writePromptList(out *strings.Builder, label string, values []string) {
	if len(values) == 0 {
		fmt.Fprintf(out, "%s: none\n", label)
		return
	}
	fmt.Fprintf(out, "%s:\n", label)
	for _, value := range values {
		line := firstLine(value)
		if line == "" {
			continue
		}
		fmt.Fprintf(out, "- %s\n", line)
	}
}

func valueOr(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func formatDate(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format("2006-01-02")
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339)
}

const truncationSuffix = "\n… (truncated)\n"

func truncateSummary(summary string, budget int) string {
	if budget <= 0 {
		return ""
	}
	runes := []rune(summary)
	if len(runes) <= budget {
		return summary
	}
	suffix := []rune(truncationSuffix)
	if budget <= len(suffix) {
		return string(suffix[:budget])
	}
	limit := budget - len(suffix)
	return strings.TrimSpace(string(runes[:limit])) + truncationSuffix
}
