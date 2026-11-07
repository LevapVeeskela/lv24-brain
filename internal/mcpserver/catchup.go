package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
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
	Journal         []string      `json:"journal"`
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
	if _, ok := s.backend.(CatchupBackend); !ok {
		return nil
	}
	return []tool{{
		Name: "brain_catchup",
		Description: "What changed in a project since its latest handoff (or the last since_hours): notes, decisions, attempts, " +
			"handoffs, state changes and commits from other sessions and machines. Call at session start after brain_open when continuing work.",
		InputSchema: objectSchema(nil, map[string]any{
			"project":     stringProp("Project id, alias or checkout path. Omit to use the project of the working directory."),
			"since_hours": intProp("Look back this many hours instead of to the latest handoff; 0 uses the latest handoff.", 0),
			"limit":       intProp("Maximum items per list; 0 selects the default of 20.", 0),
		}),
		run: s.catchup,
	}}
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
		return toolResult{}, errors.New("since_hours must not be negative")
	}
	if args.Limit < 0 {
		return toolResult{}, errors.New("limit must not be negative")
	}
	target, err := s.backend.Resolve(ctx, strings.TrimSpace(args.Project))
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
	return toolResult{Text: catchupText(result), Structured: result}, nil
}

// empty reports whether nothing changed since the baseline.
func (c Catchup) empty() bool {
	return !c.StateChanged && len(c.Notes) == 0 && len(c.Decisions) == 0 &&
		len(c.Attempts) == 0 && len(c.Handoffs) == 0 && len(c.Journal) == 0
}

func catchupText(c Catchup) string {
	since := c.Since.UTC().Format(time.RFC3339)
	if c.empty() {
		return fmt.Sprintf("nothing new in %s since %s (%s)", c.ProjectID, since, c.SinceSource)
	}
	var b strings.Builder
	source := c.SinceSource
	if c.BaselineSummary != "" {
		source += ": " + c.BaselineSummary
	}
	fmt.Fprintf(&b, "project: %s\nsince: %s (%s)\n", c.ProjectID, since, source)
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

func writeCatchupSection(b *strings.Builder, label string, items []CatchupItem) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "%s (%d):\n", label, len(items))
	for _, item := range items {
		fmt.Fprintf(b, "- [%s] %s (%s, %s)\n", item.Kind, item.Title, item.ID, item.CreatedAt.UTC().Format(time.RFC3339))
	}
}
