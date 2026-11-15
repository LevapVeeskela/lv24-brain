package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
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
	now := a.now()
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
	notes, err := a.listNotes(ctx, project.ID)
	if err != nil {
		return BootstrapResult{}, err
	}
	quarantined := quarantinedIDs(notes, decisions, attempts, handoffs)
	if !opts.IncludeUnreviewed {
		decisions = trustedDecisions(decisions)
		attempts = trustedAttempts(attempts)
		handoffs = trustedHandoffs(handoffs)
	}
	filter := validityFilter(opts.AsOf, opts.IncludeSuperseded)
	decisions = filterDecisions(decisions, filter, now)
	attempts = filterAttempts(attempts, filter, now)
	handoffs = filterHandoffs(handoffs, filter, now)

	if opts.TokenBudget > 0 {
		return a.bootstrapPacked(ctx, opts, project, state, decisions, attempts, handoffs, quarantined, now)
	}

	var out strings.Builder
	fmt.Fprintf(&out, "# %s — bootstrap\n", project.Name)
	fmt.Fprintf(&out, "project_id: %s\n", project.ID)
	fmt.Fprintf(&out, "mode: %s\n", a.cfg.Mode)
	if revision, revisionErr := a.deps.Store.CurrentRevision(ctx); revisionErr == nil {
		fmt.Fprintf(&out, "revision: %s\n", revision.Value)
	}
	fmt.Fprintf(&out, "state_updated: %s (age %s)\n", formatTime(state.UpdatedAt), stateAge(now, state))
	fmt.Fprintf(&out, "freshness: %s\n", freshness(now, state))
	if !opts.AsOf.IsZero() {
		fmt.Fprintf(&out, "as_of: %s\n", formatTime(opts.AsOf))
	}
	if line := quarantineLine(quarantined, opts.IncludeUnreviewed); line != "" {
		out.WriteString(line + "\n")
	}
	fmt.Fprintf(&out, "goal: %s\n", valueOr(state.Goal, "not set"))
	fmt.Fprintf(&out, "next_action: %s\n", valueOr(state.NextAction, "not set"))
	writePromptList(&out, "active", state.Active)
	writePromptList(&out, "blocked", state.Blocked)
	writePromptList(&out, "open_questions", state.OpenQuestions)

	// The latest handoff comes before decisions and attempts: truncation to
	// the budget cuts from the end, and the newest handoff matters most.
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

	out.WriteString("\n## Recent decisions\n")
	if len(decisions) == 0 {
		out.WriteString("- none recorded\n")
	} else {
		for i, decision := range decisions {
			if i >= maxListedItems {
				break
			}
			fmt.Fprintf(&out, "- [%s] %s (%s)\n", decision.Status, SummarizeLine(decision.Title, promptLineRunes), formatDate(decision.CreatedAt))
			if line := SummarizeLine(decision.Context, promptLineRunes); line != "" {
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

	budget := opts.Budget
	if budget <= 0 {
		budget = DefaultBootstrapBudget
	}
	return BootstrapResult{
		ProjectID:   project.ID,
		Summary:     truncateSummary(out.String(), budget),
		Quarantined: quarantined,
	}, nil
}

// bootstrapPacked builds the `brain open` summary with the layered packer:
// state and the latest handoff are core, current decisions and recent attempts
// follow under their own caps.
func (a *App) bootstrapPacked(
	ctx context.Context, opts BootstrapOptions, project domain.Project, state domain.State,
	decisions []domain.Decision, attempts []domain.Attempt, handoffs []domain.Handoff,
	quarantined []string, now time.Time,
) (BootstrapResult, error) {
	budget, err := a.contextTokenBudget(opts.TokenBudget)
	if err != nil {
		return BootstrapResult{}, err
	}
	header := renderBootstrapHeader(project, a.cfg.Mode, a.revision(ctx), state, opts, quarantined, now)
	coreText, coreRecords := renderBootstrapCore(state, handoffs, now)

	decisionRecords := make([]ContextRecord, 0, len(decisions))
	for i := range decisions {
		record := contextRecordFromRecord(Record{Kind: ports.RecordDecision, Decision: &decisions[i]})
		record.Score = decisionContextScore(record.Status)
		decisionRecords = append(decisionRecords, record)
	}
	attemptRecords := make([]ContextRecord, 0, len(attempts))
	for i := range attempts {
		attemptRecords = append(attemptRecords, contextRecordFromRecord(Record{Kind: ports.RecordAttempt, Attempt: &attempts[i]}))
	}

	pack, err := buildContextPack(contextPackInput{
		Budget: budget, Header: header, CoreText: coreText, CoreRecords: coreRecords,
		Decisions: decisionRecords, Attempts: attemptRecords, Quarantined: len(quarantined),
	})
	if err != nil {
		return BootstrapResult{}, err
	}

	var out strings.Builder
	out.WriteString(header)
	if core, ok := contextSection(pack, ContextLayerCore); ok {
		out.WriteString(core.Text)
	} else {
		out.WriteString(coreText)
	}
	if records := sectionRecords(pack, ContextLayerDecisions); len(records) > 0 {
		out.WriteString("\n## Recent decisions\n")
		writeContextRecords(&out, records, "")
	}
	if records := sectionRecords(pack, ContextLayerAttempts); len(records) > 0 {
		out.WriteString("\n## Recent attempts\n")
		writeContextRecords(&out, records, "")
	}
	summary := out.String()
	if opts.Budget > 0 {
		summary = truncateSummary(summary, opts.Budget)
	}
	return BootstrapResult{ProjectID: project.ID, Summary: summary, Quarantined: quarantined, Pack: pack}, nil
}

// renderBootstrapHeader writes the identity and state metadata of a packed
// open summary.
func renderBootstrapHeader(
	project domain.Project, mode, revision string, state domain.State, opts BootstrapOptions,
	quarantined []string, now time.Time,
) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s — bootstrap\n", project.Name)
	fmt.Fprintf(&out, "project_id: %s\n", project.ID)
	fmt.Fprintf(&out, "mode: %s\n", mode)
	if revision != "" {
		fmt.Fprintf(&out, "revision: %s\n", revision)
	}
	fmt.Fprintf(&out, "state_updated: %s (age %s)\n", formatTime(state.UpdatedAt), stateAge(now, state))
	fmt.Fprintf(&out, "freshness: %s\n", freshness(now, state))
	if !opts.AsOf.IsZero() {
		fmt.Fprintf(&out, "as_of: %s\n", formatTime(opts.AsOf))
	}
	if line := quarantineLine(quarantined, opts.IncludeUnreviewed); line != "" {
		out.WriteString(line + "\n")
	}
	return out.String()
}

// renderBootstrapCore renders the state and the latest handoff, the two blocks
// that are always kept.
func renderBootstrapCore(state domain.State, handoffs []domain.Handoff, now time.Time) (string, []ContextSectionRecord) {
	var out strings.Builder
	writeContextState(&out, state, now)
	stateTokens := textbudget.Tokens(out.String())
	records := []ContextSectionRecord{{
		ID: "state", Kind: "state", Title: "Project state", Path: "state.md", Tokens: stateTokens, Score: 1,
	}}
	start := out.Len()
	if len(handoffs) == 0 {
		out.WriteString("\n## Latest handoff\n- none recorded\n")
	} else {
		writeContextHandoff(&out, &handoffs[0])
		records = append(records, ContextSectionRecord{
			ID: handoffs[0].ID, Kind: "handoff", Title: handoffTitle(handoffs[0]),
			Path:      recordDataPath(handoffs[0].ProjectID, "handoff", handoffs[0].ID+".md"),
			CreatedAt: handoffs[0].CreatedAt, UpdatedAt: handoffs[0].CreatedAt,
			Tokens: textbudget.Tokens(out.String()[start:]), Score: 1,
		})
	}
	return out.String(), records
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

// promptLineRunes bounds one line of injected context.
const promptLineRunes = 200

// maxPromptListItems bounds a state list in injected context.
const maxPromptListItems = 10

func writePromptList(out *strings.Builder, label string, values []string) {
	if len(values) == 0 {
		fmt.Fprintf(out, "%s: none\n", label)
		return
	}
	fmt.Fprintf(out, "%s:\n", label)
	shown := 0
	for _, value := range values {
		line := SummarizeLine(value, promptLineRunes)
		if line == "" {
			continue
		}
		if shown == maxPromptListItems {
			fmt.Fprintf(out, "- … %d more (brain state get)\n", len(values)-shown)
			break
		}
		fmt.Fprintf(out, "- %s\n", line)
		shown++
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
