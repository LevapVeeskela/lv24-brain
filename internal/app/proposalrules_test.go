package app

import (
	"strings"
	"testing"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func ruleItem(kind domain.ItemKind, title string) domain.Item {
	return domain.Item{
		Ref:   domain.ItemRef{ConnectionID: "gh", ExternalID: "item-1"},
		Kind:  kind,
		Title: title,
	}
}

func ruleConnection() domain.Connection {
	return domain.Connection{ID: "gh", Provider: "github", Projects: []string{"demo"}}
}

func TestProposalRulePrecedenceAndConfidence(t *testing.T) {
	closedIssue := ruleItem(domain.ItemIssue, "Serialize writes")
	closedIssue.State = domain.ItemStateClosed
	closedIssue.Labels = []string{"bug"}

	resolvedDecision := ruleItem(domain.ItemIssue, "Adopt SQLite")
	resolvedDecision.State = domain.ItemStateResolved
	resolvedDecision.Labels = []string{"architecture", "incident", "production"}

	labeledDecision := ruleItem(domain.ItemIssue, "Choose queue")
	labeledDecision.Labels = []string{"RFC", "incident"}

	incident := ruleItem(domain.ItemIssue, "API outage")
	incident.Labels = []string{"incident", "production"}

	sentry := ruleItem(domain.ItemIssue, "Unhandled panic")
	sentry.Ref.ExternalID = "sentry-1"

	mergedPR := ruleItem(domain.ItemPullRequest, "Merge parser fix")
	mergedPR.State = domain.ItemStateMerged
	mergedPR.Body = "The parser now handles nested arrays."

	mergedMR := ruleItem(domain.ItemMergeRequest, "Merge cache fix")
	mergedMR.State = domain.ItemStateMerged

	commit := ruleItem(domain.ItemCommit, "fix: parser")

	cases := []struct {
		name       string
		connection domain.Connection
		item       domain.Item
		wantRule   string
		wantKind   domain.ProposalKind
		minConf    float64
		maxConf    float64
	}{
		{"closed issue", ruleConnection(), closedIssue, ProposalRuleClosedIssue, domain.ProposalDecision, 0.6, 0.8},
		{"closed issue beats labels and incident", ruleConnection(), resolvedDecision, ProposalRuleClosedIssue, domain.ProposalDecision, 0.6, 0.8},
		{"decision label beats incident", ruleConnection(), labeledDecision, ProposalRuleDecisionLabel, domain.ProposalDecision, 0.7, 0.7},
		{"incident label", ruleConnection(), incident, ProposalRuleIncident, domain.ProposalAttempt, 0.55, 0.75},
		{"sentry provider", domain.Connection{ID: "sentry", Provider: "sentry", Projects: []string{"demo"}}, sentry, ProposalRuleIncident, domain.ProposalAttempt, 0.55, 0.75},
		{"merged pull request", ruleConnection(), mergedPR, ProposalRuleMergedChange, domain.ProposalNote, 0.6, 0.65},
		{"merged merge request", ruleConnection(), mergedMR, ProposalRuleMergedChange, domain.ProposalNote, 0.6, 0.65},
		{"commit", ruleConnection(), commit, ProposalRuleCommit, domain.ProposalNote, 0.5, 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			match, ok := matchProposalRule(tc.connection, tc.item)
			if !ok {
				t.Fatalf("item did not match any rule")
			}
			if match.rule != tc.wantRule || match.kind != tc.wantKind {
				t.Fatalf("match = %+v, want rule %s kind %s", match, tc.wantRule, tc.wantKind)
			}
			if match.confidence < tc.minConf || match.confidence > tc.maxConf {
				t.Fatalf("confidence = %v, want [%v,%v]", match.confidence, tc.minConf, tc.maxConf)
			}
		})
	}

	openIssue := ruleItem(domain.ItemIssue, "Still open")
	if _, ok := matchProposalRule(ruleConnection(), openIssue); ok {
		t.Fatal("an open unlabeled issue matched a rule")
	}
}

func TestProposalRuleFilter(t *testing.T) {
	filter, err := proposalRuleFilter([]string{"commit", "sentry", "LABEL-DECISION"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{ProposalRuleCommit, ProposalRuleIncident, ProposalRuleDecisionLabel} {
		if !proposalRuleEnabled(filter, want) {
			t.Fatalf("rule %q not enabled by filter %v", want, filter)
		}
	}
	if proposalRuleEnabled(filter, ProposalRuleClosedIssue) {
		t.Fatal("closed-issue should be filtered out")
	}
	all, err := proposalRuleFilter(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range ProposalRuleNames() {
		if !proposalRuleEnabled(all, rule) {
			t.Fatalf("empty filter did not enable %q", rule)
		}
	}
	if _, err := proposalRuleFilter([]string{"nope"}); err == nil {
		t.Fatal("unknown rule was accepted")
	}

	// A filter disables higher-precedence rules, so a lower enabled rule can
	// still produce the item's single proposal.
	resolved := ruleItem(domain.ItemIssue, "Adopt SQLite")
	resolved.State = domain.ItemStateResolved
	resolved.Labels = []string{"architecture", "incident"}
	incidentOnly, err := proposalRuleFilter([]string{"incident"})
	if err != nil {
		t.Fatal(err)
	}
	match, ok := matchProposalRuleWithFilter(ruleConnection(), resolved, incidentOnly)
	if !ok || match.rule != ProposalRuleIncident || match.kind != domain.ProposalAttempt {
		t.Fatalf("filtered match = %+v, %v", match, ok)
	}
}

func TestBuildProposalBoundsAndCitations(t *testing.T) {
	observed := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	item := ruleItem(domain.ItemIssue, strings.Repeat("Long title ", 100))
	item.Body = strings.Repeat("payload line\n", 2000)
	item.URL = "https://github.com/acme/widgets/issues/1"
	item.Labels = []string{"Decision", "RFC"}
	item.CreatedAt = observed.Add(-48 * time.Hour)
	item.UpdatedAt = observed.Add(-time.Hour)
	item.ObservedAt = observed
	item.Hash = item.ComputeHash()

	match, ok := matchProposalRule(ruleConnection(), item)
	if !ok {
		t.Fatal("closed issue did not match")
	}
	proposal, err := buildProposal("demo", ruleConnection(), item, match)
	if err != nil {
		t.Fatal(err)
	}
	if err := proposal.Validate(); err != nil {
		t.Fatalf("proposal does not validate: %v", err)
	}
	if proposal.ID != domain.ProposalID("demo", "gh", item.Ref.ExternalID) {
		t.Fatalf("id = %q, want the deterministic id", proposal.ID)
	}
	if len([]byte(proposal.Title)) > 512 {
		t.Fatalf("title is %d bytes, want <= 512", len([]byte(proposal.Title)))
	}
	if strings.Contains(proposal.Title, "\n") {
		t.Fatalf("title kept a newline: %q", proposal.Title)
	}
	if len(proposal.Citations) != 1 {
		t.Fatalf("citations = %+v", proposal.Citations)
	}
	citation := proposal.Citations[0]
	if citation.ConnectionID != "gh" || citation.ExternalID != item.Ref.ExternalID ||
		citation.ItemKey != item.Key() || citation.URL != item.URL || !citation.ObservedAt.Equal(observed) {
		t.Fatalf("citation = %+v", citation)
	}
	if len(proposal.Sections) == 0 {
		t.Fatal("proposal has no sections")
	}
	for _, section := range proposal.Sections {
		if got := len([]rune(section.Body)); got > maxProposalExcerptRunes {
			t.Fatalf("section %q is %d runes, want <= %d", section.Heading, got, maxProposalExcerptRunes)
		}
	}
	evidence := ""
	for _, section := range proposal.Sections {
		if section.Heading == "Evidence" {
			evidence = section.Body
		}
	}
	if evidence == "" {
		t.Fatalf("no Evidence section: %+v", proposal.Sections)
	}
	if !strings.Contains(evidence, "[excerpt truncated]") {
		t.Fatalf("long body was not excerpted: %q", evidence[:min(len(evidence), 200)])
	}
	if strings.Contains(evidence, item.Body) {
		t.Fatal("proposal copied the full raw body")
	}
	if got := len([]rune(proposalExcerpt(item.Body, maxProposalExcerptRunes))); got > maxProposalExcerptRunes {
		t.Fatalf("excerpt is %d runes, want <= %d", got, maxProposalExcerptRunes)
	}

	// ObservedAt is informational: re-observing the same item must not change
	// the payload hash and therefore must not look like an update.
	reobserved := item
	reobserved.ObservedAt = observed.Add(6 * time.Hour)
	again, err := buildProposal("demo", ruleConnection(), reobserved, match)
	if err != nil {
		t.Fatal(err)
	}
	if again.PayloadHash != proposal.PayloadHash {
		t.Fatal("re-observing changed the payload hash")
	}

	// An invalid URL is omitted instead of failing the whole digest.
	noURL := item
	noURL.URL = "not a url"
	noURLProposal, err := buildProposal("demo", ruleConnection(), noURL, match)
	if err != nil {
		t.Fatalf("invalid source URL blocked the proposal: %v", err)
	}
	if len(noURLProposal.Citations) != 1 || noURLProposal.Citations[0].URL != "" {
		t.Fatalf("invalid URL was cited: %+v", noURLProposal.Citations)
	}

	// A note candidate keeps its bounded source text in Body; Evidence carries
	// only metadata, so the excerpt is not duplicated.
	pr := ruleItem(domain.ItemPullRequest, "Merge parser fix")
	pr.State = domain.ItemStateMerged
	pr.Body = strings.Repeat("note line\n", 500)
	pr.ObservedAt = observed
	noteMatch, ok := matchProposalRule(ruleConnection(), pr)
	if !ok {
		t.Fatal("merged pull request did not match")
	}
	note, err := buildProposal("demo", ruleConnection(), pr, noteMatch)
	if err != nil {
		t.Fatal(err)
	}
	if note.Kind != domain.ProposalNote || !strings.Contains(note.Body, "note line") {
		t.Fatalf("note body = %q", note.Body)
	}
	if got := len([]rune(note.Body)); got > maxProposalExcerptRunes {
		t.Fatalf("note body is %d runes, want <= %d", got, maxProposalExcerptRunes)
	}
	for _, section := range note.Sections {
		if strings.Contains(section.Body, "note line") {
			t.Fatalf("note excerpt duplicated in %q", section.Heading)
		}
	}
}
