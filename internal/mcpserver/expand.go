package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/textbudget"
)

// Expansion bounds of brain_search. Progressively disclosed answers are what
// keep a search cheap: hits carry ids and previews, and only the records the
// model asks for are returned in full.
const (
	// ExpandDefaultMaxTokens is the token budget of one expanded record when
	// the caller gives none.
	ExpandDefaultMaxTokens = 400
	// ExpandMaxTokens bounds the budget of one expanded record.
	ExpandMaxTokens = 4000
	// ExpandMaxRecords bounds how many records one call expands.
	ExpandMaxRecords = 20
)

// ExpandedHit is one record returned in full by brain_search. Only the fields
// of its kind are set.
type ExpandedHit struct {
	RecordDetail
	// Truncated reports that a text field was cut to the token budget.
	Truncated bool `json:"truncated"`
	// Tokens is the approximate token count of the returned text.
	Tokens int `json:"tokens"`
}

// expandSearchRecords returns the full records of ids, which must be on the
// page the caller just read. Every text field is cut to maxTokens, so one long
// record cannot fill the caller's context.
func (s *Server) expandSearchRecords(ctx context.Context, pageHits []Hit, ids []string, maxTokens int) ([]ExpandedHit, error) {
	backend, ok := s.backend.(RecordBackend)
	if !ok || s.catalog {
		return nil, refusalUnsupported(
			"this backend cannot expand records",
			"read the record with brain_record",
		)
	}
	if maxTokens == 0 {
		maxTokens = ExpandDefaultMaxTokens
	}
	if maxTokens < 0 || maxTokens > ExpandMaxTokens {
		return nil, refusalInvalidf("max_tokens must be between 1 and %d", ExpandMaxTokens)
	}
	projects := make(map[string]string, len(pageHits))
	for _, hit := range pageHits {
		projects[hit.ID] = hit.ProjectID
	}
	seen := make(map[string]bool, len(ids))
	expanded := make([]ExpandedHit, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		projectID, ok := projects[id]
		if !ok {
			return nil, refusalInvalidf("id %q is not on this page; expand ids from hits of the same call", id)
		}
		detail, err := backend.GetRecord(ctx, projectID, id)
		if err != nil {
			return nil, err
		}
		expanded = append(expanded, truncateExpanded(detail, maxTokens))
	}
	if len(expanded) == 0 {
		return nil, refusalInvalid("expand_ids is empty; pass ids from the hits of this call")
	}
	return expanded, nil
}

// expandPageIDs returns the ids of every hit on the page, in rank order.
func expandPageIDs(hits []Hit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

// truncateExpanded applies the token budget to each free-text field of a
// record and reports whether anything was cut.
func truncateExpanded(detail RecordDetail, maxTokens int) ExpandedHit {
	truncated := false
	cut := func(text string) string {
		result, wasCut := textbudget.Truncate(text, maxTokens)
		if wasCut {
			truncated = true
		}
		return result
	}
	detail.Body = cut(detail.Body)
	detail.Context = cut(detail.Context)
	detail.Hypothesis = cut(detail.Hypothesis)
	detail.Result = cut(detail.Result)
	detail.Reason = cut(detail.Reason)
	detail.NextAction = cut(detail.NextAction)
	detail.GitContext = cut(detail.GitContext)
	tokens := 0
	for _, text := range []string{detail.Body, detail.Context, detail.Hypothesis, detail.Result, detail.Reason, detail.NextAction, detail.GitContext} {
		tokens += textbudget.Tokens(text)
	}
	for _, items := range [][]string{detail.Alternatives, detail.Consequences, detail.Evidence} {
		for _, item := range items {
			tokens += textbudget.Tokens(item)
		}
	}
	return ExpandedHit{RecordDetail: detail, Truncated: truncated, Tokens: tokens}
}

// expandedText renders expanded records as the text half of the tool result,
// so a host that shows text instead of structured content sees the same
// bodies.
func expandedText(expanded []ExpandedHit) string {
	var builder strings.Builder
	for _, record := range expanded {
		builder.WriteString(detailText(record.RecordDetail))
		if record.Truncated {
			fmt.Fprintf(&builder, "\ntruncated: tokens=%d", record.Tokens)
		}
		builder.WriteString("\n\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}
