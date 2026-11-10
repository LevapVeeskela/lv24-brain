package app

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Proposal digest rules (Phase 4.1). Rules are deterministic: one item yields
// at most one candidate record, chosen by the first matching rule in
// proposalRuleOrder. Confidence is a heuristic in [0,1] and never auto-accepts
// anything; a human promotes the proposal through `brain promote`.
const (
	ProposalRuleClosedIssue   = "closed-issue"
	ProposalRuleDecisionLabel = "decision-label"
	ProposalRuleIncident      = "incident"
	ProposalRuleMergedChange  = "merged-change"
	ProposalRuleCommit        = "commit"

	// maxProposalExcerptRunes bounds copied source text in a proposal section.
	maxProposalExcerptRunes = 2000
	// maxProposalTitleRunes keeps a title well below the 512-byte domain cap
	// even for four-byte runes (121 runes is at most 484 bytes plus "…").
	maxProposalTitleRunes = 120
	// maxProposalCitationTitleRunes uses the same bound for citation titles.
	maxProposalCitationTitleRunes = 120
)

// proposalRuleOrder is the precedence of the digest rules, top-down.
var proposalRuleOrder = []string{
	ProposalRuleClosedIssue,
	ProposalRuleDecisionLabel,
	ProposalRuleIncident,
	ProposalRuleMergedChange,
	ProposalRuleCommit,
}

// proposalRuleAliases maps accepted --rule spellings to canonical rule names.
var proposalRuleAliases = map[string]string{
	"closed-issue":   ProposalRuleClosedIssue,
	"closed":         ProposalRuleClosedIssue,
	"issue-closed":   ProposalRuleClosedIssue,
	"decision-label": ProposalRuleDecisionLabel,
	"label-decision": ProposalRuleDecisionLabel,
	"decision":       ProposalRuleDecisionLabel,
	"adr":            ProposalRuleDecisionLabel,
	"incident":       ProposalRuleIncident,
	"sentry":         ProposalRuleIncident,
	"merged-change":  ProposalRuleMergedChange,
	"merged":         ProposalRuleMergedChange,
	"merged-pr":      ProposalRuleMergedChange,
	"merged-mr":      ProposalRuleMergedChange,
	"commit":         ProposalRuleCommit,
}

var (
	decisionLabels = map[string]struct{}{
		"decision": {}, "adr": {}, "rfc": {}, "architecture": {},
	}
	incidentLabels = map[string]struct{}{
		"incident": {}, "outage": {}, "sev0": {}, "sev1": {}, "sev2": {},
		"sev3": {}, "production": {},
	}
	closedStateLabels = map[string]struct{}{
		"closed": {}, "done": {}, "resolved": {}, "fixed": {}, "completed": {},
	}
)

// ProposalRuleNames returns the canonical rule names in precedence order.
func ProposalRuleNames() []string {
	return append([]string(nil), proposalRuleOrder...)
}

// proposalMatch is the rule verdict for one item.
type proposalMatch struct {
	rule       string
	kind       domain.ProposalKind
	confidence float64
}

// matchProposalRule returns the highest-precedence rule matching item.
func matchProposalRule(connection domain.Connection, item domain.Item) (proposalMatch, bool) {
	return matchProposalRuleWithFilter(connection, item, nil)
}

// matchProposalRuleWithFilter returns the highest-precedence enabled rule
// matching item. A nil or empty filter enables every rule.
func matchProposalRuleWithFilter(connection domain.Connection, item domain.Item, filter map[string]struct{}) (proposalMatch, bool) {
	labels := normalizedProposalLabels(item.Labels)
	switch {
	case proposalRuleEnabled(filter, ProposalRuleClosedIssue) && item.Kind == domain.ItemIssue && itemIsClosed(item, labels):
		return proposalMatch{
			rule:       ProposalRuleClosedIssue,
			kind:       domain.ProposalDecision,
			confidence: closedIssueConfidence(item, labels),
		}, true
	case proposalRuleEnabled(filter, ProposalRuleDecisionLabel) && intersects(labels, decisionLabels):
		return proposalMatch{
			rule:       ProposalRuleDecisionLabel,
			kind:       domain.ProposalDecision,
			confidence: 0.7,
		}, true
	case proposalRuleEnabled(filter, ProposalRuleIncident) &&
		(strings.EqualFold(connection.Provider, "sentry") || intersects(labels, incidentLabels)):
		return proposalMatch{
			rule:       ProposalRuleIncident,
			kind:       domain.ProposalAttempt,
			confidence: incidentConfidence(connection.Provider, labels),
		}, true
	case proposalRuleEnabled(filter, ProposalRuleMergedChange) && isMergedChange(item, labels):
		return proposalMatch{
			rule:       ProposalRuleMergedChange,
			kind:       domain.ProposalNote,
			confidence: mergedChangeConfidence(item),
		}, true
	case proposalRuleEnabled(filter, ProposalRuleCommit) && item.Kind == domain.ItemCommit:
		return proposalMatch{
			rule:       ProposalRuleCommit,
			kind:       domain.ProposalNote,
			confidence: 0.5,
		}, true
	default:
		return proposalMatch{}, false
	}
}

// proposalRuleFilter returns the enabled canonical rules; empty means all.
func proposalRuleFilter(rules []string) (map[string]struct{}, error) {
	filter := make(map[string]struct{}, len(rules))
	for _, raw := range rules {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		canonical, ok := proposalRuleAliases[name]
		if !ok {
			return nil, domain.NewValidationError("rule",
				fmt.Sprintf("unknown proposal rule %q; want %s", raw, strings.Join(ProposalRuleNames(), ", ")))
		}
		filter[canonical] = struct{}{}
	}
	return filter, nil
}

func proposalRuleEnabled(filter map[string]struct{}, rule string) bool {
	if len(filter) == 0 {
		return true
	}
	_, ok := filter[rule]
	return ok
}

func itemIsClosed(item domain.Item, labels []string) bool {
	switch item.State {
	case domain.ItemStateClosed, domain.ItemStateResolved:
		return true
	}
	return intersects(labels, closedStateLabels)
}

func isMergedChange(item domain.Item, labels []string) bool {
	if item.Kind != domain.ItemPullRequest && item.Kind != domain.ItemMergeRequest {
		return false
	}
	return item.State == domain.ItemStateMerged || containsString(labels, "merged")
}

// closedIssueConfidence stays in [0.6,0.8]: state supplies the base, and
// resolution-like labels add evidence of a settled outcome.
func closedIssueConfidence(item domain.Item, labels []string) float64 {
	confidence := 0.6
	switch item.State {
	case domain.ItemStateClosed:
		confidence = 0.7
	case domain.ItemStateResolved:
		confidence = 0.75
	}
	if intersects(labels, closedStateLabels) {
		confidence += 0.05
	}
	if confidence > 0.8 {
		confidence = 0.8
	}
	return confidence
}

// incidentConfidence stays in [0.55,0.75]: a Sentry item is a little stronger
// than a generic incident label, severity labels order the rest, and the
// production label adds context without dominating severity.
func incidentConfidence(provider string, labels []string) float64 {
	confidence := 0.55
	if strings.EqualFold(provider, "sentry") {
		confidence = 0.6
	}
	switch {
	case containsString(labels, "sev0"):
		confidence = 0.75
	case containsString(labels, "sev1"):
		confidence = 0.7
	case containsString(labels, "sev2"):
		confidence = 0.65
	case containsString(labels, "sev3"):
		confidence = 0.6
	}
	if containsString(labels, "production") && confidence < 0.75 {
		confidence += 0.05
	}
	if confidence > 0.75 {
		confidence = 0.75
	}
	return confidence
}

// mergedChangeConfidence stays in [0.6,0.65]: a captured body explains more
// than a bare merge title.
func mergedChangeConfidence(item domain.Item) float64 {
	if strings.TrimSpace(item.Body) != "" {
		return 0.65
	}
	return 0.6
}

// normalizedProposalLabels lowercases, trims, deduplicates and sorts labels,
// so matching and generated text are stable across reads.
func normalizedProposalLabels(labels []string) []string {
	seen := make(map[string]struct{}, len(labels))
	out := make([]string, 0, len(labels))
	for _, raw := range labels {
		label := strings.ToLower(strings.TrimSpace(raw))
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func intersects(values []string, set map[string]struct{}) bool {
	for _, value := range values {
		if _, ok := set[value]; ok {
			return true
		}
	}
	return false
}

func matchingLabels(labels []string, set map[string]struct{}) []string {
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if _, ok := set[label]; ok {
			out = append(out, label)
		}
	}
	return out
}

// buildProposal renders one bounded candidate record. It never copies a full
// raw body: the excerpt is capped and the provenance line stays short.
func buildProposal(projectID string, connection domain.Connection, item domain.Item, match proposalMatch) (domain.Proposal, error) {
	title := proposalLabel(item.Title, maxProposalTitleRunes)
	if title == "" {
		title = proposalLabel(item.Ref.ExternalID, maxProposalTitleRunes)
	}
	proposal := domain.Proposal{
		SchemaVersion: domain.ProposalSchemaVersion,
		ProjectID:     projectID,
		ID:            domain.ProposalID(projectID, connection.ID, item.Ref.ExternalID),
		Kind:          match.kind,
		Rule:          match.rule,
		Confidence:    match.confidence,
		ConnectionID:  connection.ID,
		ExternalID:    item.Ref.ExternalID,
		Title:         title,
		Body:          proposalBody(connection, item, match),
		Sections:      proposalSections(connection, item, match),
		Citations:     proposalCitations(connection, item),
	}
	proposal = proposal.WithPayloadHash()
	if err := proposal.Validate(); err != nil {
		return domain.Proposal{}, err
	}
	return proposal, nil
}

func proposalBody(connection domain.Connection, item domain.Item, match proposalMatch) string {
	if match.kind == domain.ProposalNote {
		// Note bodies are free-form: carry the bounded source text so a
		// promoted note keeps its content, and fall back to provenance when
		// the source item has no body.
		if excerpt := proposalExcerpt(item.Body, maxProposalExcerptRunes); excerpt != "" {
			return excerpt
		}
	}
	return proposalPreamble(connection, item, match.kind)
}

func proposalPreamble(connection domain.Connection, item domain.Item, kind domain.ProposalKind) string {
	return fmt.Sprintf("Candidate %s derived from %s %s %s.",
		kind, connection.Provider, item.Kind, item.Ref.ExternalID)
}

func proposalSections(connection domain.Connection, item domain.Item, match proposalMatch) []domain.ProposalSection {
	var sections []domain.ProposalSection
	switch match.rule {
	case ProposalRuleClosedIssue, ProposalRuleDecisionLabel:
		sections = decisionProposalSections(connection, item, match)
	case ProposalRuleIncident:
		sections = attemptProposalSections(connection, item)
	default:
		sections = noteProposalSections(connection, item, match)
	}
	for index := range sections {
		// Keep every section within the same excerpt budget, including its
		// source metadata, so no single section can grow unbounded.
		sections[index].Body = proposalExcerpt(sections[index].Body, maxProposalExcerptRunes)
	}
	return sections
}

func decisionProposalSections(connection domain.Connection, item domain.Item, match proposalMatch) []domain.ProposalSection {
	labels := normalizedProposalLabels(item.Labels)
	context := "Closed issue recorded as a decision candidate."
	if match.rule == ProposalRuleDecisionLabel {
		context = fmt.Sprintf("Decision signal(s): %s.", strings.Join(matchingLabels(labels, decisionLabels), ", "))
	} else if item.State != "" {
		context = fmt.Sprintf("Closed issue (%s) recorded as a decision candidate.", item.State)
	}
	return []domain.ProposalSection{
		{Heading: "Context", Body: context},
		{Heading: "Evidence", Body: proposalEvidence(connection, item, labels, true)},
	}
}

func attemptProposalSections(connection domain.Connection, item domain.Item) []domain.ProposalSection {
	labels := normalizedProposalLabels(item.Labels)
	result := fmt.Sprintf("Observed as %s %s.", item.Kind, valueOr("state unknown", string(item.State)))
	if len(labels) > 0 {
		result = fmt.Sprintf("Observed as %s %s; labels: %s.", item.Kind, valueOr("state unknown", string(item.State)), strings.Join(labels, ", "))
	}
	reason := proposalExcerpt(item.Body, maxProposalExcerptRunes)
	if reason == "" {
		reason = "No incident body was captured."
	}
	return []domain.ProposalSection{
		{Heading: "Hypothesis", Body: "The reported incident signals a failure or regression; confirm the expected behavior before promoting."},
		{Heading: "Result", Body: result},
		{Heading: "Reason", Body: reason},
		{Heading: "Evidence", Body: proposalEvidence(connection, item, labels, false)},
	}
}

func noteProposalSections(connection domain.Connection, item domain.Item, match proposalMatch) []domain.ProposalSection {
	labels := normalizedProposalLabels(item.Labels)
	context := fmt.Sprintf("Merged %s %s.", item.Kind, item.Ref.ExternalID)
	if match.rule == ProposalRuleCommit {
		context = fmt.Sprintf("Commit %s.", item.Ref.ExternalID)
	}
	return []domain.ProposalSection{
		{Heading: "Context", Body: context},
		{Heading: "Evidence", Body: proposalEvidence(connection, item, labels, false)},
	}
}

// proposalEvidence renders stable source metadata with a bounded body excerpt.
func proposalEvidence(connection domain.Connection, item domain.Item, labels []string, includeExcerpt bool) string {
	lines := []string{
		fmt.Sprintf("Source: %s %s %s", connection.Provider, item.Kind, item.Ref.ExternalID),
	}
	if item.State != "" {
		lines = append(lines, "State: "+string(item.State))
	}
	if len(labels) > 0 {
		lines = append(lines, "Labels: "+strings.Join(labels, ", "))
	}
	if item.Author != "" {
		lines = append(lines, "Author: "+proposalLabel(item.Author, maxProposalTitleRunes))
	}
	if item.URL != "" {
		lines = append(lines, "URL: "+item.URL)
	}
	if !item.CreatedAt.IsZero() {
		lines = append(lines, "Created: "+item.CreatedAt.UTC().Format(time.RFC3339))
	}
	if !item.UpdatedAt.IsZero() {
		lines = append(lines, "Updated: "+item.UpdatedAt.UTC().Format(time.RFC3339))
	}
	if includeExcerpt {
		if excerpt := proposalExcerpt(item.Body, maxProposalExcerptRunes); excerpt != "" {
			lines = append(lines, "", excerpt)
		}
	}
	return strings.Join(lines, "\n")
}

func proposalCitations(connection domain.Connection, item domain.Item) []domain.ProposalCitation {
	citation := domain.ProposalCitation{
		ConnectionID: connection.ID,
		ItemKey:      item.Key(),
		ExternalID:   item.Ref.ExternalID,
		Title:        proposalLabel(item.Title, maxProposalCitationTitleRunes),
		ObservedAt:   item.ObservedAt.UTC(),
	}
	if item.URL != "" && domain.ValidateSourceURL("citation.url", item.URL) == nil {
		citation.URL = item.URL
	}
	return []domain.ProposalCitation{citation}
}

// proposalLabel sanitizes a one-line display value and bounds it by runes.
// Control characters are replaced so the domain label checks never fail on a
// source title.
func proposalLabel(value string, limit int) string {
	cleaned := strings.Map(func(char rune) rune {
		if char == '\n' || char == '\r' || char == '\t' || unicode.IsControl(char) {
			return ' '
		}
		return char
	}, value)
	return truncateRunes(strings.Join(strings.Fields(cleaned), " "), limit)
}

// proposalExcerpt normalizes Markdown and keeps at most maxProposalExcerptRunes
// runes including the truncation marker.
func proposalExcerpt(body string, limit int) string {
	const marker = "\n\n[excerpt truncated]"
	body = domain.NormalizeMarkdown(body)
	if body == "" {
		return ""
	}
	body = strings.Map(func(char rune) rune {
		if char != '\n' && char != '\t' && unicode.IsControl(char) {
			return ' '
		}
		return char
	}, body)
	runes := []rune(body)
	if len(runes) <= limit {
		return body
	}
	keep := limit - len([]rune(marker))
	if keep < 0 {
		keep = 0
	}
	return strings.TrimSpace(string(runes[:keep])) + marker
}
