package app

import (
	"context"
	"fmt"

	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// FindingBrokenLink flags a [[...]] reference to a record or project that
// does not exist. Report-only: the link text needs a person or agent.
const FindingBrokenLink FindingKind = "broken-link"

// linkFindings reports every broken link of the reviewed records. Links into
// other projects are resolved with one listing per referenced project.
func (a *App) linkFindings(ctx context.Context, in reviewInputs) []Finding {
	records := in.records()
	if len(records) == 0 {
		return nil
	}
	projectID := records[0].Summary().ProjectID
	var findings []Finding
	for _, broken := range brokenLinksIn(ctx, a, projectID, records) {
		findings = append(findings, Finding{
			Kind: FindingBrokenLink,
			Message: fmt.Sprintf("%s %q links to [[%s]] in %s: %s", broken.Source.Kind,
				SummarizeLine(broken.Source.Title, 80), broken.Link.Ref, broken.Link.Field, broken.Link.Detail),
			Evidence: broken.Source.ID,
		})
	}
	return findings
}

// records returns the reviewed records as one list.
func (in reviewInputs) records() []Record {
	var out []Record
	for i := range in.notes {
		out = append(out, Record{Kind: ports.RecordNote, Note: &in.notes[i]})
	}
	for i := range in.decisions {
		out = append(out, Record{Kind: ports.RecordDecision, Decision: &in.decisions[i]})
	}
	for i := range in.attempts {
		out = append(out, Record{Kind: ports.RecordAttempt, Attempt: &in.attempts[i]})
	}
	for i := range in.handoffs {
		out = append(out, Record{Kind: ports.RecordHandoff, Handoff: &in.handoffs[i]})
	}
	return out
}
