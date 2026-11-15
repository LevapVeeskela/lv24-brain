package main

import (
	"context"
	"fmt"
	"io"

	"github.com/levapveeskela/lv24-brain/internal/app"
)

// jsonTimeline is `brain timeline --json`: the anchor and its neighbours in
// chronological order.
type jsonTimeline struct {
	Anchor      jsonRecordSummary   `json:"anchor"`
	AnchorIndex int                 `json:"anchor_index"`
	Before      int                 `json:"before"`
	After       int                 `json:"after"`
	Total       int                 `json:"total"`
	Records     []jsonRecordSummary `json:"records"`
}

// runTimeline prints the records of the same project and kind around one
// record, oldest first. It answers "what came before and after this decision"
// without loading either listing. A timeline is a historical view: records
// whose validity ended are included.
func runTimeline(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("timeline", stderr)
	project := fs.String("project", "", "project id or alias")
	window := fs.Int("window", app.DefaultTimelineWindow, "records to return on each side of the anchor")
	asJSON := fs.Bool("json", false, "print JSON")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if !expectArgs("timeline", positional, 1, "<id>", stderr) {
		return 2
	}
	if *window < 0 {
		fmt.Fprintln(stderr, "brain timeline: --window must not be negative")
		return 2
	}
	ctx := context.Background()
	runtime, target, code := stateTarget(ctx, *cfgPath, *project, nil, stderr, "timeline")
	if code != 0 {
		return code
	}
	defer func() { _ = runtime.Close() }()
	timeline, err := runtime.App.Timeline(ctx, target.Project.ID, positional[0], *window)
	if err != nil {
		return reportError(stderr, "timeline", err)
	}
	if *asJSON {
		return writeJSONReport(stdout, stderr, "timeline", jsonTimeline{
			Anchor:      toJSONRecordSummary(timeline.Anchor),
			AnchorIndex: timeline.AnchorIndex,
			Before:      timeline.Before,
			After:       timeline.After,
			Total:       timeline.Total,
			Records:     mapAll(timeline.Records, toJSONRecordSummary),
		})
	}
	for index, record := range timeline.Records {
		marker := " "
		if index == timeline.AnchorIndex {
			marker = ">"
		}
		fmt.Fprintf(stdout, "%s %s\n", marker, recordSummaryLine(record))
	}
	return 0
}
