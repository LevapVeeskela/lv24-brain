package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/textbudget"
)

// DefaultRecordBodyTokens is the token budget of brain_get and of an expanded
// MCP hit when the caller gives none. It fits a whole decision or attempt while
// staying far below a full record listing.
const DefaultRecordBodyTokens = 600

// RecordBody is one record's readable text under a token budget.
type RecordBody struct {
	Kind      string
	ID        string
	ProjectID string
	Title     string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Body is the rendered text of the record's kind, truncated to MaxTokens.
	Body string
	// Truncated reports that the record continues past the budget.
	Truncated bool
	// Tokens is the approximate token count of Body.
	Tokens int
	// MaxTokens is the budget that was applied; zero means unlimited.
	MaxTokens int
}

// RecordBody returns the readable text of one record, cut to maxTokens. A
// non-positive budget returns the whole record. This is the progressive
// disclosure step after a search hit: the preview says whether the record is
// relevant, the body says what it decided.
func (a *App) RecordBody(ctx context.Context, projectID, id string, maxTokens int) (RecordBody, error) {
	record, err := a.GetRecord(ctx, projectID, id)
	if err != nil {
		return RecordBody{}, err
	}
	summary := record.Summary()
	text := RecordText(record)
	body, truncated := textbudget.Truncate(text, maxTokens)
	return RecordBody{
		Kind:      summary.Kind,
		ID:        summary.ID,
		ProjectID: summary.ProjectID,
		Title:     summary.Title,
		Status:    summary.Status,
		CreatedAt: summary.CreatedAt,
		UpdatedAt: summary.UpdatedAt,
		Body:      body,
		Truncated: truncated,
		Tokens:    textbudget.Tokens(body),
		MaxTokens: maxTokens,
	}, nil
}

// RecordText renders the text fields of a record with their labels, in the
// order a reader needs them. It is the body a search hit expands into.
func RecordText(record Record) string {
	var builder strings.Builder
	writeSection := func(label, text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(label + ":\n" + text)
	}
	writeItems := func(label string, items []string) {
		if len(items) == 0 {
			return
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(label + ":")
		for _, item := range items {
			builder.WriteString("\n- " + item)
		}
	}
	switch {
	case record.Note != nil:
		// A note is one text; a label would only repeat the JSON field name.
		builder.WriteString(record.Note.Body)
	case record.Decision != nil:
		writeSection("Context", record.Decision.Context)
		writeItems("Alternatives", record.Decision.Alternatives)
		writeItems("Consequences", record.Decision.Consequences)
		writeItems("Evidence", record.Decision.Evidence)
	case record.Attempt != nil:
		writeSection("Hypothesis", record.Attempt.Hypothesis)
		writeSection("Result", record.Attempt.Result)
		writeSection("Reason", record.Attempt.Reason)
	case record.Handoff != nil:
		writeSection("Summary", record.Handoff.Summary)
		writeSection("Next action", record.Handoff.NextAction)
	}
	return builder.String()
}

// timelineScanLimit bounds how many records of one kind a timeline listing
// reads. A project with more than this many records of a single kind cannot be
// positioned around an anchor deterministically, and the command says so.
const timelineScanLimit = 10000

// DefaultTimelineWindow is the number of neighbours returned on each side of
// the anchor.
const DefaultTimelineWindow = 3

// TimelineResult is one record with its neighbours in the same project and
// kind, in chronological order.
type TimelineResult struct {
	// Anchor is the record the timeline is centred on.
	Anchor RecordSummary
	// Records is the returned window, oldest first; it contains the anchor.
	Records []RecordSummary
	// AnchorIndex is the position of Anchor in Records.
	AnchorIndex int
	// Before and After count the returned neighbours on each side.
	Before int
	After  int
	// Total is the number of records of that kind in the project.
	Total int
}

// Timeline returns up to window records of the same project and kind on each
// side of id, oldest first. A timeline is a historical view: records whose
// validity ended are included, so a superseded decision still shows where it
// sat. The anchor itself must be a live record.
func (a *App) Timeline(ctx context.Context, projectID, id string, window int) (TimelineResult, error) {
	if window < 0 {
		return TimelineResult{}, domain.NewValidationError("timeline.window", "must not be negative")
	}
	if window == 0 {
		window = DefaultTimelineWindow
	}
	record, err := a.GetRecord(ctx, projectID, id)
	if err != nil {
		return TimelineResult{}, err
	}
	summary := record.Summary()
	records, err := a.ListRecords(ctx, ListRecordsOptions{
		ProjectID:         summary.ProjectID,
		Kinds:             []string{summary.Kind},
		Limit:             timelineScanLimit,
		IncludeSuperseded: true,
	})
	if err != nil {
		return TimelineResult{}, err
	}
	// ListRecords returns newest first; a timeline reads oldest first.
	anchor := -1
	for index := range records {
		if records[index].ID == summary.ID {
			anchor = index
			break
		}
	}
	if anchor < 0 {
		return TimelineResult{}, fmt.Errorf("record %q: %w", summary.ID, domain.ErrNotFound)
	}
	oldestFirst := make([]RecordSummary, len(records))
	for index := range records {
		oldestFirst[index] = records[len(records)-1-index]
	}
	anchor = len(oldestFirst) - 1 - anchor
	start := anchor - window
	if start < 0 {
		start = 0
	}
	end := anchor + window + 1
	if end > len(oldestFirst) {
		end = len(oldestFirst)
	}
	windowRecords := oldestFirst[start:end]
	return TimelineResult{
		Anchor:      summary,
		Records:     windowRecords,
		AnchorIndex: anchor - start,
		Before:      anchor - start,
		After:       end - anchor - 1,
		Total:       len(oldestFirst),
	}, nil
}
