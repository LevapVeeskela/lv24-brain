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
// baseline. A zero since means the latest handoff.
type CatchupBackend interface {
	Catchup(ctx context.Context, projectID string, since time.Time, limit int) (Catchup, error)
}

// Timeline is one record with its neighbours in the same project and kind,
// oldest first.
type Timeline struct {
	Anchor      RecordSummary   `json:"anchor"`
	Records     []RecordSummary `json:"records"`
	AnchorIndex int             `json:"anchor_index"`
	Before      int             `json:"before"`
	After       int             `json:"after"`
	Total       int             `json:"total"`
}

// TimelineBackend is implemented by backends that can position a record in its
// kind's chronology.
type TimelineBackend interface {
	Timeline(ctx context.Context, projectID, id string, window int) (Timeline, error)
}

// TimelinePreview is one record of a timeline as the tool returns it.
type TimelinePreview struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Preview   string    `json:"preview"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	Anchor    bool      `json:"anchor,omitempty"`
}

// DefaultTimelineWindow is the number of neighbours on each side.
const DefaultTimelineWindow = 3

// maxTimelineWindow bounds the neighbours on each side.
const maxTimelineWindow = 5

func (s *Server) catchupTool() tool {
	project := stringProp("Project id, alias or checkout path. Omit to use the project of the working directory.")
	return tool{
		Name:        "brain_catchup",
		Annotations: readOnlyAnnotations("Catch up on recent activity"),
		Description: "What changed in a project: mode=changes (default) reports notes, decisions, attempts, handoffs, state changes " +
			"and commits since the latest handoff or since_hours; mode=journal lists the activity journal newest first; " +
			"mode=timeline places one record (record_id) among its neighbours of the same kind.",
		InputSchema: objectSchema(nil, map[string]any{
			"mode":        enumProp("changes (default), journal or timeline.", []string{"changes", "journal", "timeline"}),
			"project":     project,
			"since_hours": intProp("Look back this many hours instead of to the latest handoff; 0 uses the latest handoff.", 0),
			"record_id":   stringProp("Record to centre a timeline on (mode=timeline)."),
			"limit":       intProp("Maximum items per list (changes), page size (journal) or neighbours per side (timeline, at most 5).", 0),
			"cursor":      cursorProp(),
		}),
		OutputSchema: outputObject(pageOutput(map[string]any{
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
			"entries":          arrayProp("Journal entries, newest first (mode=journal).", journalEntrySchema()),
			"previews":         arrayProp("Timeline records, oldest first (mode=timeline).", timelinePreviewSchema()),
		})),
		Limits:      limitsOf(pageLimitsFor("brain_journal")),
		ReadActions: []string{"changes", "journal", "timeline"},
		run:         s.catchup,
	}
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

func journalEntrySchema() map[string]any {
	return outputObject(map[string]any{
		"time":    stringProp("RFC3339 time of the event."),
		"user":    stringProp("User or harness that recorded it."),
		"event":   stringProp("Event name, for example note or commit."),
		"project": stringProp("Project the event belongs to."),
		"id":      stringProp("Record id, when the event has one."),
		"title":   stringProp("Human summary of the event."),
		"fields":  anyProp("Event-specific fields."),
	}, "time", "user", "event")
}

func timelinePreviewSchema() map[string]any {
	return outputObject(map[string]any{
		"id":         stringProp("Record id."),
		"kind":       stringProp("Record kind."),
		"title":      stringProp("Record title."),
		"preview":    stringProp("At most 160 characters of the record."),
		"created_at": stringProp("RFC3339 creation time."),
		"anchor":     boolProp("True for the record the timeline is centred on."),
	}, "id", "kind", "title", "preview")
}

// catchupArgs is one brain_catchup request.
type catchupArgs struct {
	Mode       string `json:"mode"`
	Project    string `json:"project"`
	SinceHours int    `json:"since_hours"`
	RecordID   string `json:"record_id"`
	Limit      int    `json:"limit"`
	Cursor     string `json:"cursor"`
}

func (s *Server) catchup(ctx context.Context, raw json.RawMessage) (toolResult, error) {
	var args catchupArgs
	if err := decodeArgs(raw, &args); err != nil {
		return toolResult{}, err
	}
	mode := strings.TrimSpace(args.Mode)
	if mode == "" {
		mode = "changes"
	}
	switch mode {
	case "changes":
		return s.catchupChanges(ctx, args)
	case "journal":
		return s.journal(ctx, args)
	case "timeline":
		return s.timeline(ctx, args)
	default:
		return toolResult{}, refusalInvalidf("unknown mode %q (changes, journal, timeline)", mode)
	}
}

func (s *Server) catchupChanges(ctx context.Context, args catchupArgs) (toolResult, error) {
	if args.SinceHours < 0 {
		return toolResult{}, refusalInvalid("since_hours must not be negative")
	}
	if args.Limit < 0 {
		return toolResult{}, refusalInvalid("limit must not be negative")
	}
	backend, ok := s.backend.(CatchupBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_catchup(mode=changes)", "brain_open and brain_search")
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	var since time.Time
	if args.SinceHours > 0 {
		since = time.Now().Add(-time.Duration(args.SinceHours) * time.Hour)
	}
	result, err := backend.Catchup(ctx, target.ID, since, args.Limit)
	if err != nil {
		return toolResult{}, err
	}
	if result.Quarantined == nil {
		result.Quarantined = []string{}
	}
	return toolResult{Text: catchupText(result), Structured: result}, nil
}

// journal lists the activity journal with the pagination contract. The cursor
// tool name stays brain_journal so cursors issued before 0.17 still decode.
func (s *Server) journal(ctx context.Context, args catchupArgs) (toolResult, error) {
	projectID, err := s.optionalProject(ctx, args.Project)
	if err != nil {
		return toolResult{}, err
	}
	limits := pageLimitsFor("brain_journal")
	filter := filterFingerprint(projectID)
	offset, size, err := pageRequest("brain_journal", limits, args.Limit, args.Cursor, filter)
	if err != nil {
		return toolResult{}, err
	}
	requested := fetchLimit(offset, size, limits)
	entries, err := s.backend.Journal(ctx, JournalQuery{ProjectID: projectID, Limit: requested})
	if err != nil {
		return toolResult{}, err
	}
	pageEntries, page := paginate(entries, offset, size, requested, "brain_journal", filter)
	if pageEntries == nil {
		pageEntries = []JournalEntry{}
	}
	var text strings.Builder
	if len(pageEntries) == 0 {
		text.WriteString("no entries")
	}
	for _, entry := range pageEntries {
		line := fmt.Sprintf("%s  %s  %s", entry.Time.UTC().Format(time.RFC3339), entry.User, entry.Event)
		if entry.Project != "" {
			line += "  " + entry.Project
		}
		switch {
		case entry.Title != "":
			line += "  " + entry.Title
		case entry.ID != "":
			line += "  " + entry.ID
		}
		text.WriteString(line + "\n")
	}
	if footer := pageFooter(page); footer != "" {
		text.WriteString(footer)
	}
	return toolResult{
		Text: strings.TrimRight(text.String(), "\n"),
		Structured: struct {
			Entries []JournalEntry `json:"entries"`
			Page
		}{Entries: pageEntries, Page: page},
	}, nil
}

func (s *Server) timeline(ctx context.Context, args catchupArgs) (toolResult, error) {
	id, err := requireText("record_id", args.RecordID)
	if err != nil {
		return toolResult{}, err
	}
	if args.Limit < 0 {
		return toolResult{}, refusalInvalid("limit must not be negative")
	}
	window := args.Limit
	if window == 0 {
		window = DefaultTimelineWindow
	}
	if window > maxTimelineWindow {
		window = maxTimelineWindow
	}
	backend, ok := s.backend.(TimelineBackend)
	if !ok {
		return toolResult{}, unsupportedBackend("brain_catchup(mode=timeline)", "brain record show "+id+" --history")
	}
	target, err := s.resolveTarget(ctx, strings.TrimSpace(args.Project))
	if err != nil {
		return toolResult{}, err
	}
	result, err := backend.Timeline(ctx, target.ID, id, window)
	if err != nil {
		return toolResult{}, err
	}
	previews := make([]TimelinePreview, 0, len(result.Records))
	for index, record := range result.Records {
		preview := TimelinePreview{
			ID: record.ID, Kind: record.Kind, Title: record.Title,
			Preview: truncateRunes(record.Title, 160), CreatedAt: record.CreatedAt,
			Anchor: index == result.AnchorIndex,
		}
		previews = append(previews, preview)
	}
	var text strings.Builder
	fmt.Fprintf(&text, "timeline of %s %s: %d of %d", result.Anchor.Kind, result.Anchor.ID, result.AnchorIndex+1, result.Total)
	for _, preview := range previews {
		marker := " "
		if preview.Anchor {
			marker = "*"
		}
		fmt.Fprintf(&text, "\n%s %s %s %s", marker, preview.Kind, preview.ID, preview.Title)
	}
	return toolResult{
		Text: text.String(),
		Structured: struct {
			ProjectID string            `json:"project_id"`
			Anchor    RecordSummary     `json:"anchor"`
			Previews  []TimelinePreview `json:"previews"`
			Total     int               `json:"total"`
		}{ProjectID: target.ID, Anchor: result.Anchor, Previews: previews, Total: result.Total},
	}, nil
}

// truncateRunes keeps at most limit runes of value.
func truncateRunes(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit])
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
