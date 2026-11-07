package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	catchupDefaultLimit = 20
	catchupTimeLayout   = "2006-01-02 15:04"
	catchupSummaryRunes = 100
)

// runCatchup implements `brain catchup`: what changed in a project since its
// latest handoff, so a session starts from the delta instead of re-reading.
func runCatchup(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("catchup", stderr)
	since := fs.Duration("since", 0, "look back this far instead of to the latest handoff, e.g. 24h")
	limit := fs.Int("limit", catchupDefaultLimit, "maximum items per list")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if *limit < 0 || *since < 0 {
		fmt.Fprintln(stderr, "brain catchup: --limit and --since must not be negative")
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, "", positional, stderr, "catchup")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	opts := app.CatchupOptions{ProjectID: target.Project.ID, Limit: *limit}
	if *since > 0 {
		opts.Since = time.Now().Add(-*since)
	}
	result, err := runtime.App.Catchup(ctx, opts)
	if err != nil {
		return reportError(stderr, "catchup", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "catchup", toJSONCatchup(result))
	}
	printCatchup(stdout, result)
	return 0
}

// printCatchup renders the human catchup report, or the "nothing new" line
// when the window holds no changes.
func printCatchup(w io.Writer, result app.CatchupResult) {
	if result.Empty() {
		fmt.Fprintf(w, "catchup %s: nothing new since %s (%s)\n", result.Project.ID, formatCatchupTime(result.Since), result.SinceSource)
		return
	}
	fmt.Fprintf(w, "catchup %s since %s (%s)\n", result.Project.ID, formatCatchupTime(result.Since), catchupSourceLabel(result))
	if result.StateChanged {
		fmt.Fprintf(w, "state changed: goal=%s next=%s\n", valueOr(result.State.Goal, "not set"), valueOr(result.State.NextAction, "not set"))
	}
	printSection(w, "notes", mapAll(result.Notes, noteLine))
	printSection(w, "decisions", mapAll(result.Decisions, decisionLine))
	printSection(w, "attempts", mapAll(result.Attempts, attemptLine))
	printSection(w, "handoffs", mapAll(result.Handoffs, handoffLine))
	printSection(w, "activity", mapAll(result.Journal, formatJournalEntry))
}

func catchupSourceLabel(result app.CatchupResult) string {
	if result.Baseline == nil {
		return result.SinceSource
	}
	return result.SinceSource + ": " + catchupLine(result.Baseline.Summary)
}

func printSection(w io.Writer, label string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "%s (%d):\n", label, len(lines))
	for _, line := range lines {
		fmt.Fprintf(w, "- %s\n", line)
	}
}

func noteLine(note domain.Note) string {
	return formatCatchupTime(note.CreatedAt) + " " + catchupLine(firstNonEmpty(note.Title, note.Body))
}

func decisionLine(decision domain.Decision) string {
	return fmt.Sprintf("%s %s [%s]", formatCatchupTime(decision.UpdatedAt), catchupLine(decision.Title), decision.Status)
}

func attemptLine(attempt domain.Attempt) string {
	return formatCatchupTime(attempt.CreatedAt) + " " + catchupLine(attempt.Title)
}

func handoffLine(handoff domain.Handoff) string {
	next := valueOr(catchupLine(handoff.NextAction), "none")
	return fmt.Sprintf("%s %s -> next: %s", formatCatchupTime(handoff.CreatedAt), catchupLine(handoff.Summary), next)
}

func formatCatchupTime(value time.Time) string {
	return value.Local().Format(catchupTimeLayout)
}

// catchupLine shortens free text to one listing line.
func catchupLine(text string) string {
	return app.SummarizeLine(text, catchupSummaryRunes)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// mapAll converts every item; the result is never nil, so JSON lists stay [].
func mapAll[T, U any](items []T, convert func(T) U) []U {
	out := make([]U, 0, len(items))
	for _, item := range items {
		out = append(out, convert(item))
	}
	return out
}

// jsonNote is one note written after the catchup baseline.
type jsonNote struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	CreatedAt *string `json:"created_at"`
}

// jsonDecision is one decision changed after the baseline.
type jsonDecision struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Status    string  `json:"status"`
	UpdatedAt *string `json:"updated_at"`
}

// jsonAttempt is one attempt recorded after the baseline.
type jsonAttempt struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	CreatedAt *string `json:"created_at"`
}

// jsonHandoff is a handoff; the baseline and the list entries share it.
type jsonHandoff struct {
	ID         string  `json:"id"`
	Summary    string  `json:"summary"`
	NextAction string  `json:"next_action"`
	CreatedAt  *string `json:"created_at"`
}

// jsonCatchup is `brain catchup --json`.
type jsonCatchup struct {
	Project      string         `json:"project"`
	Since        *string        `json:"since"`
	SinceSource  string         `json:"since_source"`
	Baseline     *jsonHandoff   `json:"baseline"`
	StateChanged bool           `json:"state_changed"`
	State        jsonState      `json:"state"`
	Notes        []jsonNote     `json:"notes"`
	Decisions    []jsonDecision `json:"decisions"`
	Attempts     []jsonAttempt  `json:"attempts"`
	Handoffs     []jsonHandoff  `json:"handoffs"`
	Journal      jsonJournal    `json:"journal"`
}

func toJSONCatchup(result app.CatchupResult) jsonCatchup {
	out := jsonCatchup{
		Project:      result.Project.ID,
		Since:        jsonTime(result.Since),
		SinceSource:  result.SinceSource,
		StateChanged: result.StateChanged,
		State:        toJSONState(result.State),
		Notes:        mapAll(result.Notes, toJSONNote),
		Decisions:    mapAll(result.Decisions, toJSONDecision),
		Attempts:     mapAll(result.Attempts, toJSONAttempt),
		Handoffs:     mapAll(result.Handoffs, toJSONHandoff),
		Journal:      toJSONJournal(result.Journal),
	}
	if result.Baseline != nil {
		baseline := toJSONHandoff(*result.Baseline)
		out.Baseline = &baseline
	}
	return out
}

func toJSONNote(note domain.Note) jsonNote {
	return jsonNote{ID: note.ID, Title: note.Title, Body: note.Body, CreatedAt: jsonTime(note.CreatedAt)}
}

func toJSONDecision(decision domain.Decision) jsonDecision {
	return jsonDecision{ID: decision.ID, Title: decision.Title, Status: string(decision.Status), UpdatedAt: jsonTime(decision.UpdatedAt)}
}

func toJSONAttempt(attempt domain.Attempt) jsonAttempt {
	return jsonAttempt{ID: attempt.ID, Title: attempt.Title, CreatedAt: jsonTime(attempt.CreatedAt)}
}

func toJSONHandoff(handoff domain.Handoff) jsonHandoff {
	return jsonHandoff{ID: handoff.ID, Summary: handoff.Summary, NextAction: handoff.NextAction, CreatedAt: jsonTime(handoff.CreatedAt)}
}
