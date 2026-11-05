package app

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Review reports state age, open questions, decision evidence and recent
// attempts. Since accepts 30m, 24h, 7d, or 2w windows.
func (a *App) Review(ctx context.Context, opts ReviewOptions) (ReviewResult, error) {
	if err := a.requireStore(); err != nil {
		return ReviewResult{}, err
	}
	project, err := a.resolveProject(ctx, opts.ProjectID)
	if err != nil {
		return ReviewResult{}, err
	}
	state, err := a.loadState(ctx, project.ID)
	if err != nil {
		return ReviewResult{}, err
	}
	decisions, err := a.listDecisions(ctx, project.ID)
	if err != nil {
		return ReviewResult{}, err
	}
	attempts, err := a.listAttempts(ctx, project.ID)
	if err != nil {
		return ReviewResult{}, err
	}
	now := a.now()
	cutoff, filtered, err := parseSince(opts.Since, now)
	if err != nil {
		return ReviewResult{}, err
	}

	items := make([]string, 0, 16)
	var out strings.Builder
	fmt.Fprintf(&out, "# Review — %s\n", project.Name)
	fmt.Fprintf(&out, "state_age: %s\n", stateAge(now, state))
	fmt.Fprintf(&out, "freshness: %s\n", freshness(now, state))
	fmt.Fprintf(&out, "goal: %s\n", valueOr(state.Goal, "not set"))
	fmt.Fprintf(&out, "next_action: %s\n", valueOr(state.NextAction, "not set"))
	items = append(items, "state_age: "+stateAge(now, state))
	items = append(items, "freshness: "+freshness(now, state))

	if len(state.Blocked) > 0 {
		out.WriteString("blocked:\n")
		for _, blocked := range state.Blocked {
			line := firstLine(blocked)
			if line == "" {
				continue
			}
			fmt.Fprintf(&out, "- %s\n", line)
			items = append(items, "blocked: "+line)
		}
	} else {
		out.WriteString("blocked: none\n")
	}

	out.WriteString("open_questions:\n")
	if len(state.OpenQuestions) == 0 {
		out.WriteString("- none\n")
	} else {
		for _, question := range state.OpenQuestions {
			line := firstLine(question)
			if line == "" {
				continue
			}
			fmt.Fprintf(&out, "- %s\n", line)
			items = append(items, "open_question: "+line)
		}
	}

	out.WriteString("evidence:\n")
	evidenceCount := 0
	for _, decision := range decisions {
		if filtered && !withinWindow(decision.CreatedAt, decision.UpdatedAt, cutoff) {
			continue
		}
		for _, evidence := range decision.Evidence {
			line := firstLine(evidence)
			if line == "" {
				continue
			}
			if evidenceCount >= maxListedItems*2 {
				break
			}
			fmt.Fprintf(&out, "- [%s] %s: %s\n", decision.Status, decision.Title, line)
			items = append(items, "evidence: ["+string(decision.Status)+"] "+decision.Title+": "+line)
			evidenceCount++
		}
		if evidenceCount >= maxListedItems*2 {
			break
		}
	}
	if evidenceCount == 0 {
		out.WriteString("- none\n")
	}

	out.WriteString("attempts:\n")
	attemptCount := 0
	for _, attempt := range attempts {
		if filtered && !withinWindow(attempt.CreatedAt, attempt.UpdatedAt, cutoff) {
			continue
		}
		if attemptCount >= maxListedItems {
			break
		}
		line := firstLine(attempt.Reason)
		if line == "" {
			line = firstLine(attempt.Result)
		}
		if line == "" {
			line = firstLine(attempt.Hypothesis)
		}
		fmt.Fprintf(&out, "- %s (%s)", attempt.Title, formatDate(attempt.CreatedAt))
		if line != "" {
			fmt.Fprintf(&out, ": %s", line)
		}
		out.WriteString("\n")
		items = append(items, "attempt: "+attempt.Title+firstLineSuffix(line))
		attemptCount++
	}
	if attemptCount == 0 {
		out.WriteString("- none\n")
	}

	return ReviewResult{Summary: out.String(), Items: items}, nil
}

func withinWindow(created, updated time.Time, cutoff time.Time) bool {
	timestamp := created
	if timestamp.IsZero() {
		timestamp = updated
	}
	if timestamp.IsZero() {
		return true
	}
	return !timestamp.Before(cutoff)
}

func firstLineSuffix(value string) string {
	if value == "" {
		return ""
	}
	return ": " + value
}
