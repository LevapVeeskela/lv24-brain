package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Catchup is what changed in a project since its baseline.
type Catchup struct {
	ProjectID       string        `json:"project_id"`
	Since           time.Time     `json:"since"`
	SinceSource     string        `json:"since_source"`
	BaselineSummary string        `json:"baseline_summary,omitempty"`
	StateChanged    bool          `json:"state_changed"`
	State           *State        `json:"state,omitempty"`
	Notes           []CatchupItem `json:"notes"`
	Decisions       []CatchupItem `json:"decisions"`
	Attempts        []CatchupItem `json:"attempts"`
	Handoffs        []CatchupItem `json:"handoffs"`
	// Quarantined lists the ids of records excluded from the report.
	Quarantined []string `json:"quarantined"`
	Journal     []string `json:"journal"`
}

// CatchupItem is one record written after the baseline. CreatedAt is the
// write time; for decisions it is the last update.
type CatchupItem struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

// CatchupBackend is implemented by backends that can report changes since a
// baseline. A zero since means the latest handoff. The tool is listed only
// for such backends.
type CatchupBackend interface {
	Catchup(ctx context.Context, projectID string, since time.Time, limit int) (Catchup, error)
}

func (s *Server) catchupTools() []tool {
	if _, ok := s.backend.(CatchupBackend); !ok && !s.catalog {
		return nil
	}
	return []tool{{
		Name:        "brain_catchup",
		Annotations: readOnlyAnnotations("Catch up since handoff"),
		Description: "What changed in a project since its latest handoff (or the last since_hours): notes, decisions, attempts, " +
			"handoffs, state changes and commits from other sessions and machines. Call at session start after brain_open when continuing work.",
		InputSchema: objectSchema(nil, map[string]any{
			"project":     stringProp("Project id, alias or checkout path. Omit to use the project of the working directory."),
			"since_hours": intProp("Look back this many hours instead of to the latest handoff; 0 uses the latest handoff.", 0),
			"limit":       intProp("Maximum items per list; 0 selects the default of 20.", 0),
		}),
		OutputSchema: outputObject(map[string]any{
			"project_id":       stringProp("Project the changes belong to."),
			"since":            stringProp("RFC3339 baseline the changes are measured from."),
			"since_source":     stringProp("How the baseline was chosen, for example handoff or hours."),
			"baseline_summary": stringProp("Summary of the baseline handoff, when there is one."),
			"state_changed":    boolProp("Whether the project state changed after the baseline."),
			"state":            anyProp("Current state, when it changed."),
			"notes":            arrayProp("Notes written after the baseline.", catchupItemSchema()),
			"decisions":        arrayProp("Decisions written or updated after the baseline.", catchupItemSchema()),
			"attempts":         arrayProp("Attempts written after the baseline.", catchupItemSchema()),
			"handoffs":         arrayProp("Handoffs written after the baseline.", catchupItemSchema()),
			"quarantined":      arrayProp("Ids of quarantined records excluded from the report.", stringProp("Record id.")),
			"journal":          arrayProp("Activity lines after the baseline.", stringProp("One journal line.")),
		}, "project_id", "since", "since_source", "state_changed", "notes", "decisions", "attempts", "handoffs", "quarantined", "journal"),
		run: s.catchup,
	}}
}

// catchupItemSchema is one changed record in the catch-up report.
func catchupItemSchema() map[string]any {
	return outputObject(map[string]any{
		"kind":       stringProp("Record kind."),
		"id":         stringProp("Record id."),
		"title":      stringProp("Human summary."),
		"created_at": stringProp("RFC3339 write time."),
	}, "kind", "id", "title", "created_at")
}

func (s *Server) catchup(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args struct {
		Project    string `json:"project"`
		SinceHours int    `json:"since_hours"`
		Limit      int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	if args.SinceHours < 0 {
		return toolResult{}, refusalInvalid("since_hours must not be negative")
	}
	if args.Limit < 0 {
		return toolResult{}, refusalInvalid("limit must not be negative")
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	var since time.Time
	if args.SinceHours > 0 {
		since = time.Now().Add(-time.Duration(args.SinceHours) * time.Hour)
	}
	result, err := s.backend.(CatchupBackend).Catchup(ctx, target.ID, since, args.Limit)
	if err != nil {
		return toolResult{}, err
	}
	if result.Quarantined == nil {
		result.Quarantined = []string{}
	}
	return toolResult{Text: catchupText(result), Structured: result}, nil
}

// empty reports whether nothing changed since the baseline.
func (c Catchup) empty() bool {
	return !c.StateChanged && len(c.Notes) == 0 && len(c.Decisions) == 0 &&
		len(c.Attempts) == 0 && len(c.Handoffs) == 0 && len(c.Journal) == 0
}

func catchupText(c Catchup) string {
	since := c.Since.UTC().Format(time.RFC3339)
	quarantine := catchupQuarantineLine(c.Quarantined)
	if c.empty() {
		text := fmt.Sprintf("nothing new in %s since %s (%s)", c.ProjectID, since, c.SinceSource)
		if quarantine != "" {
			text += "\n" + quarantine
		}
		return text
	}
	var b strings.Builder
	source := c.SinceSource
	if c.BaselineSummary != "" {
		source += ": " + c.BaselineSummary
	}
	fmt.Fprintf(&b, "project: %s\nsince: %s (%s)\n", c.ProjectID, since, source)
	if quarantine != "" {
		b.WriteString(quarantine + "\n")
	}
	if c.State != nil {
		fmt.Fprintf(&b, "state changed: goal=%s next=%s\n", orNone(c.State.Goal), orNone(c.State.NextAction))
	}
	writeCatchupSection(&b, "notes", c.Notes)
	writeCatchupSection(&b, "decisions", c.Decisions)
	writeCatchupSection(&b, "attempts", c.Attempts)
	writeCatchupSection(&b, "handoffs", c.Handoffs)
	if len(c.Journal) > 0 {
		fmt.Fprintf(&b, "activity (%d):\n", len(c.Journal))
		for _, line := range c.Journal {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// catchupQuarantineLine renders the `quarantined: N (ids)` header, capped at
// ten ids. It never carries a record body.
func catchupQuarantineLine(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	const listed = 10
	shown := ids
	suffix := ""
	if len(shown) > listed {
		suffix = fmt.Sprintf(", … %d more", len(shown)-listed)
		shown = shown[:listed]
	}
	return fmt.Sprintf("quarantined: %d (%s%s)", len(ids), strings.Join(shown, ", "), suffix)
}

func writeCatchupSection(b *strings.Builder, label string, items []CatchupItem) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s (%d):\n", label, len(items))
	for _, item := range items {
		fmt.Fprintf(b, "- [%s] %s (%s, %s)\n", item.Kind, item.Title, item.ID, item.CreatedAt.UTC().Format(time.RFC3339))
	}
}
