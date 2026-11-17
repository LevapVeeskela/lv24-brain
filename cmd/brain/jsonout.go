package main

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/gitinfo"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// jsonSchemaVersion is the version of the --json envelope. Change it only for
// an incompatible change to a payload shape.
const jsonSchemaVersion = 1

// jsonEnvelope is the shared shape of every --json response on stdout.
type jsonEnvelope struct {
	Schema int `json:"schema"`
	Data   any `json:"data"`
}

// writeJSON prints data inside the {"schema": 1, "data": ...} envelope used by
// every --json command: indented, one document, nothing else on stdout.
func writeJSON(w io.Writer, data any) error {
	return encodeJSON(w, jsonEnvelope{Schema: jsonSchemaVersion, Data: data})
}

// encodeJSON prints value as indented JSON without the envelope. Only
// `project list --json` and `project show --json` use it: their output
// predates the envelope and agents already parse it.
func encodeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// writeJSONReport writes a --json payload and maps a write failure to the
// command's usual error report.
func writeJSONReport(stdout, stderr io.Writer, command string, data any) int {
	if err := writeJSON(stdout, data); err != nil {
		return reportError(stderr, command, err)
	}
	return 0
}

// jsonModeEnvelope is the shared schema envelope with an explicit mode, used
// when one command emits more than one payload shape (`digest --source`).
type jsonModeEnvelope struct {
	Schema int    `json:"schema"`
	Mode   string `json:"mode"`
	Data   any    `json:"data"`
}

// writeJSONMode prints data inside {schema, mode, data}: indented, one
// document, nothing else on stdout.
func writeJSONMode(w io.Writer, mode string, data any) error {
	return encodeJSON(w, jsonModeEnvelope{Schema: jsonSchemaVersion, Mode: mode, Data: data})
}

// jsonTime renders a timestamp as RFC 3339 in UTC, or null when unset.
func jsonTime(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339)
	return &formatted
}

// nonNilStrings keeps empty lists as [] rather than null in JSON.
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// appendUniqueStrings appends values not already present, preserving order.
func appendUniqueStrings(base []string, values ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(values))
	for _, value := range base {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		base = append(base, value)
	}
	return base
}

// jsonProject is the project as agents see it.
type jsonProject struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Paths   []string `json:"paths"`
	Aliases []string `json:"aliases"`
	RepoURL string   `json:"repo_url"`
}

func toJSONProject(project domain.Project) jsonProject {
	return jsonProject{
		ID:      project.ID,
		Name:    project.Name,
		Kind:    string(project.Kind),
		Paths:   nonNilStrings(project.Paths),
		Aliases: nonNilStrings(project.Aliases),
		RepoURL: project.RepoURL,
	}
}

// jsonState is the project's working picture as agents see it.
type jsonState struct {
	Goal          string   `json:"goal"`
	NextAction    string   `json:"next_action"`
	Active        []string `json:"active"`
	Blocked       []string `json:"blocked"`
	OpenQuestions []string `json:"open_questions"`
	UpdatedAt     *string  `json:"updated_at"`
	FreshUntil    *string  `json:"fresh_until"`
}

func toJSONState(state domain.State) jsonState {
	return jsonState{
		Goal:          state.Goal,
		NextAction:    state.NextAction,
		Active:        nonNilStrings(state.Active),
		Blocked:       nonNilStrings(state.Blocked),
		OpenQuestions: nonNilStrings(state.OpenQuestions),
		UpdatedAt:     jsonTime(state.UpdatedAt),
		FreshUntil:    jsonTime(state.FreshUntil),
	}
}

// jsonGit is the repository context of a project checkout.
type jsonGit struct {
	IsRepo bool     `json:"is_repo"`
	Branch string   `json:"branch"`
	Head   string   `json:"head"`
	Dirty  int      `json:"dirty"`
	Remote string   `json:"remote"`
	Log    []string `json:"log"`
}

func toJSONGit(summary gitinfo.Summary) jsonGit {
	return jsonGit{
		IsRepo: summary.IsRepo,
		Branch: summary.Branch,
		Head:   summary.Head,
		Dirty:  summary.Dirty,
		Remote: summary.Remote,
		Log:    nonNilStrings(summary.Log),
	}
}

// jsonGraph is one code-graph index found next to a project.
type jsonGraph struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

func toJSONGraphs(graphs []gitinfo.Graph) []jsonGraph {
	out := make([]jsonGraph, 0, len(graphs))
	for _, graph := range graphs {
		out = append(out, jsonGraph{Kind: graph.Kind, Path: graph.Path})
	}
	return out
}

// jsonOpenBrief is `brain open --brief --json`.
type jsonOpenBrief struct {
	Project jsonProject `json:"project"`
	State   jsonState   `json:"state"`
}

// jsonOpen is `brain open --json`: the brief fields plus git, graphs and the
// bootstrap summary text. The recent decisions, attempts and handoffs are not
// exposed by the app API yet, so they are not included.
type jsonOpen struct {
	Project jsonProject `json:"project"`
	State   jsonState   `json:"state"`
	Git     jsonGit     `json:"git"`
	Graphs  []jsonGraph `json:"graphs"`
	Summary string      `json:"summary"`
	Recent  jsonRecent  `json:"recent"`
}

// jsonRecent lists the newest records of a project in `brain open --json`.
type jsonRecent struct {
	Decisions []jsonDecision `json:"decisions"`
	Attempts  []jsonAttempt  `json:"attempts"`
	Handoffs  []jsonHandoff  `json:"handoffs"`
}

// jsonBootstrap is `brain bootstrap --json`.
type jsonBootstrap struct {
	ProjectID   string   `json:"project_id"`
	Summary     string   `json:"summary"`
	Budget      int      `json:"budget"`
	Quarantined []string `json:"quarantined"`
}

// effectiveBudget mirrors the app default applied when no positive budget is
// given, so the reported budget is the one actually used.
func effectiveBudget(budget int) int {
	if budget <= 0 {
		return app.DefaultBootstrapBudget
	}
	return budget
}

// jsonHit is one search result.
type jsonHit struct {
	ID           string       `json:"id"`
	ProjectID    string       `json:"project_id"`
	Kind         string       `json:"kind"`
	Title        string       `json:"title"`
	Snippet      string       `json:"snippet"`
	Score        float64      `json:"score"`
	Subtype      string       `json:"subtype,omitempty"`
	Scope        string       `json:"scope,omitempty"`
	TopicKey     string       `json:"topic_key,omitempty"`
	Occurrences  int          `json:"occurrences,omitempty"`
	MatchReasons []string     `json:"match_reasons,omitempty"`
	Explain      *jsonExplain `json:"explain,omitempty"`
	ValidFrom    *string      `json:"valid_from"`
	ValidTo      *string      `json:"valid_to"`
	SupersededBy string       `json:"superseded_by,omitempty"`
	// Body is the full record text; it is set only with --full.
	Body string `json:"body,omitempty"`
}

// jsonExplain is the weighted contribution of each ranking signal, printed by
// `brain search --explain --json`.
type jsonExplain struct {
	BM25       float64 `json:"bm25"`
	Recency    float64 `json:"recency"`
	Importance float64 `json:"importance"`
	Pin        float64 `json:"pin"`
	Link       float64 `json:"link"`
}

// jsonSearch is `brain search --json`. Relaxed reports that the strict term
// match found nothing and the terms were retried joined with OR; the page
// fields follow the MCP pagination contract.
type jsonSearch struct {
	Query      string    `json:"query"`
	Relaxed    bool      `json:"relaxed"`
	Page       int       `json:"page"`
	PageSize   int       `json:"page_size"`
	HasMore    bool      `json:"has_more"`
	NextCursor string    `json:"next_cursor,omitempty"`
	Hits       []jsonHit `json:"hits"`
}

func toJSONHits(hits []ports.Hit) []jsonHit {
	out := make([]jsonHit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, jsonHit{
			ID:           hit.ID,
			ProjectID:    hit.ProjectID,
			Kind:         hit.Kind,
			Title:        hit.Title,
			Snippet:      hit.Snippet,
			Score:        hit.Score,
			Subtype:      hit.Subtype,
			Scope:        hit.Scope,
			TopicKey:     hit.TopicKey,
			Occurrences:  hit.Occurrences,
			MatchReasons: hit.MatchReasons,
			Explain:      toJSONExplain(hit.Explain),
			ValidFrom:    jsonTime(hit.ValidFrom),
			ValidTo:      jsonTime(hit.ValidTo),
			SupersededBy: hit.SupersededBy,
			Body:         hit.Body,
		})
	}
	return out
}

func toJSONExplain(explain *ports.Explain) *jsonExplain {
	if explain == nil {
		return nil
	}
	return &jsonExplain{
		BM25:       explain.BM25,
		Recency:    explain.Recency,
		Importance: explain.Importance,
		Pin:        explain.Pin,
		Link:       explain.Link,
	}
}

// jsonProposalDigest is `brain digest --source --json`. Mode is "proposals" so
// a consumer can tell it apart from the activity digest payload.
type jsonProposalDigest struct {
	Mode        string              `json:"mode"`
	Project     string              `json:"project"`
	Connection  string              `json:"connection"`
	Window      string              `json:"window"`
	Since       *string             `json:"since"`
	GeneratedAt *string             `json:"generated_at"`
	All         bool                `json:"all"`
	Write       bool                `json:"write"`
	DryRun      bool                `json:"dry_run"`
	Counts      jsonProposalCounts  `json:"counts"`
	Proposals   []jsonProposalEntry `json:"proposals"`
}

// jsonProposalCounts is the digest counter block.
type jsonProposalCounts struct {
	Scanned   int  `json:"scanned"`
	Created   int  `json:"created"`
	Updated   int  `json:"updated"`
	Unchanged int  `json:"unchanged"`
	Skipped   int  `json:"skipped"`
	Truncated bool `json:"truncated"`
}

// jsonProposalEntry is one candidate record and what the digest did with it.
type jsonProposalEntry struct {
	ID          string                 `json:"id"`
	Project     string                 `json:"project"`
	Connection  string                 `json:"connection"`
	Kind        string                 `json:"kind"`
	Rule        string                 `json:"rule"`
	Confidence  float64                `json:"confidence"`
	Title       string                 `json:"title"`
	Body        string                 `json:"body,omitempty"`
	Action      string                 `json:"action"`
	Status      string                 `json:"status"`
	ExternalID  string                 `json:"external_id"`
	ItemKey     string                 `json:"item_key,omitempty"`
	URL         string                 `json:"url,omitempty"`
	PayloadHash string                 `json:"payload_hash"`
	Sections    []jsonProposalSection  `json:"sections"`
	Citations   []jsonProposalCitation `json:"citations"`
	CreatedAt   *string                `json:"created_at,omitempty"`
	UpdatedAt   *string                `json:"updated_at,omitempty"`
}

// jsonProposalSection is one ordered Markdown section.
type jsonProposalSection struct {
	Heading string `json:"heading"`
	Body    string `json:"body,omitempty"`
}

// jsonProposalCitation points back to the stored raw item.
type jsonProposalCitation struct {
	ConnectionID string  `json:"connection_id"`
	ItemKey      string  `json:"item_key,omitempty"`
	ExternalID   string  `json:"external_id,omitempty"`
	URL          string  `json:"url,omitempty"`
	Title        string  `json:"title,omitempty"`
	ObservedAt   *string `json:"observed_at"`
}

func toJSONProposalDigest(digest app.ProposalDigest, window string, dryRun bool) jsonProposalDigest {
	out := jsonProposalDigest{
		Mode:        "proposals",
		Project:     digest.ProjectID,
		Connection:  digest.ConnectionID,
		Window:      window,
		Since:       jsonOptionalTime(digest.Since),
		GeneratedAt: jsonTime(digest.GeneratedAt),
		All:         digest.All,
		Write:       digest.Write,
		DryRun:      dryRun,
		Counts: jsonProposalCounts{
			Scanned:   digest.Counts.Scanned,
			Created:   digest.Counts.Created,
			Updated:   digest.Counts.Updated,
			Unchanged: digest.Counts.Unchanged,
			Skipped:   digest.Counts.Skipped,
			Truncated: digest.Counts.Truncated,
		},
		Proposals: make([]jsonProposalEntry, 0, len(digest.Entries)),
	}
	for _, entry := range digest.Entries {
		proposal := entry.Proposal
		item := jsonProposalEntry{
			ID:          proposal.ID,
			Project:     proposal.ProjectID,
			Connection:  proposal.ConnectionID,
			Kind:        string(proposal.Kind),
			Rule:        proposal.Rule,
			Confidence:  proposal.Confidence,
			Title:       proposal.Title,
			Body:        proposal.Body,
			Action:      string(entry.Action),
			Status:      string(proposal.StatusOrPending()),
			ExternalID:  proposal.ExternalID,
			PayloadHash: proposal.PayloadHash,
			Sections:    make([]jsonProposalSection, 0, len(proposal.Sections)),
			Citations:   make([]jsonProposalCitation, 0, len(proposal.Citations)),
			CreatedAt:   jsonTime(proposal.CreatedAt),
			UpdatedAt:   jsonTime(proposal.UpdatedAt),
		}
		for _, section := range proposal.Sections {
			item.Sections = append(item.Sections, jsonProposalSection{Heading: section.Heading, Body: section.Body})
		}
		for _, citation := range proposal.Citations {
			if item.ItemKey == "" {
				item.ItemKey = citation.ItemKey
			}
			if item.URL == "" {
				item.URL = citation.URL
			}
			item.Citations = append(item.Citations, jsonProposalCitation{
				ConnectionID: citation.ConnectionID,
				ItemKey:      citation.ItemKey,
				ExternalID:   citation.ExternalID,
				URL:          citation.URL,
				Title:        citation.Title,
				ObservedAt:   jsonTime(citation.ObservedAt),
			})
		}
		out.Proposals = append(out.Proposals, item)
	}
	return out
}

// jsonOptionalTime renders an optional timestamp, or null when nil.
func jsonOptionalTime(value *time.Time) *string {
	if value == nil {
		return nil
	}
	return jsonTime(*value)
}

// jsonJournalEntry is one activity journal line.
type jsonJournalEntry struct {
	Time    string            `json:"time"`
	User    string            `json:"user"`
	Event   string            `json:"event"`
	Project string            `json:"project"`
	ID      string            `json:"id"`
	Title   string            `json:"title"`
	Fields  map[string]string `json:"fields"`
}

// jsonJournal is `brain journal --json`.
type jsonJournal struct {
	Entries []jsonJournalEntry `json:"entries"`
}

func toJSONJournal(entries []ports.JournalEntry) jsonJournal {
	out := jsonJournal{Entries: make([]jsonJournalEntry, 0, len(entries))}
	for _, entry := range entries {
		fields := entry.Fields
		if fields == nil {
			fields = map[string]string{}
		}
		out.Entries = append(out.Entries, jsonJournalEntry{
			Time:    entry.Time.UTC().Format(time.RFC3339),
			User:    entry.User,
			Event:   entry.Event,
			Project: entry.Project,
			ID:      entry.ID,
			Title:   entry.Title,
			Fields:  fields,
		})
	}
	return out
}

// jsonStatus is `brain status --json`.
type jsonStatus struct {
	Mode          string `json:"mode"`
	DataDir       string `json:"data_dir"`
	ServerURL     string `json:"server_url"`
	User          string `json:"user"`
	SchemaVersion int    `json:"schema_version"`
	EngineVersion string `json:"engine_version"`
	UpdateChannel string `json:"update_channel"`
}

// jsonReview is `brain review --json`: the review text, its items, the
// findings that remain, the ones --fix applied and, with --due, the queue.
type jsonReview struct {
	Summary   string               `json:"summary"`
	Items     []string             `json:"items"`
	Findings  []jsonFinding        `json:"findings"`
	Applied   []jsonFinding        `json:"applied"`
	Proposals []jsonReviewProposal `json:"proposals"`
	// SnoozedProposalCount counts pending candidates hidden by an active
	// snooze; they are excluded from Proposals and the pending count.
	SnoozedProposalCount int `json:"snoozed_proposal_count"`
	// Due is present only with --due; an empty list means nothing is due.
	Due *[]jsonDueReview `json:"due,omitempty"`
}

// jsonReviewProposal is one pending candidate in `brain review --json`.
type jsonReviewProposal struct {
	ID           string  `json:"id"`
	Kind         string  `json:"kind"`
	Title        string  `json:"title"`
	Rule         string  `json:"rule"`
	Confidence   float64 `json:"confidence"`
	Status       string  `json:"status"`
	ConnectionID string  `json:"connection_id"`
	CreatedAt    *string `json:"created_at"`
	UpdatedAt    *string `json:"updated_at"`
}

func toJSONReviewProposals(proposals []app.ProposalListItem) []jsonReviewProposal {
	out := make([]jsonReviewProposal, 0, len(proposals))
	for _, proposal := range proposals {
		out = append(out, jsonReviewProposal{
			ID:           proposal.ID,
			Kind:         string(proposal.Kind),
			Title:        proposal.Title,
			Rule:         proposal.Rule,
			Confidence:   proposal.Confidence,
			Status:       string(proposal.Status),
			ConnectionID: proposal.ConnectionID,
			CreatedAt:    jsonTime(proposal.CreatedAt),
			UpdatedAt:    jsonTime(proposal.UpdatedAt),
		})
	}
	return out
}

// jsonFinding is one stale or contradictory finding in `brain review --json`.
type jsonFinding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Why      string `json:"why"`
	Next     string `json:"next"`
	Confirm  string `json:"confirm"`
	Evidence string `json:"evidence"`
	Fixable  bool   `json:"fixable"`
	Fix      string `json:"fix"`
}

// jsonDueReview is one record, state or proposal past its review date.
type jsonDueReview struct {
	Kind        string  `json:"kind"`
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	ReviewAfter *string `json:"review_after"`
	OverdueDays int     `json:"overdue_days"`
	Source      string  `json:"source"`
}

func toJSONDueReviews(due []app.DueReview) []jsonDueReview {
	out := make([]jsonDueReview, 0, len(due))
	for _, review := range due {
		out = append(out, jsonDueReview{
			Kind: review.Kind, ID: review.ID, Title: review.Title,
			ReviewAfter: jsonTime(review.ReviewAfter),
			OverdueDays: int(review.Overdue.Hours() / 24),
			Source:      review.Source,
		})
	}
	return out
}

// jsonDuplicates is `brain duplicates --json`.
type jsonDuplicates struct {
	Scan     bool                   `json:"scan"`
	Projects int                    `json:"projects"`
	Exact    int                    `json:"exact"`
	Near     int                    `json:"near"`
	Clusters []jsonDuplicateCluster `json:"clusters"`
}

// jsonDuplicateCluster is one group of records that say the same thing.
type jsonDuplicateCluster struct {
	ProjectID  string                `json:"project_id"`
	Kind       string                `json:"kind"`
	Reason     string                `json:"reason"`
	Similarity float64               `json:"similarity"`
	Members    []jsonDuplicateMember `json:"members"`
}

// jsonDuplicateMember is one record of a duplicate cluster.
type jsonDuplicateMember struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	CreatedAt *string `json:"created_at"`
}

func toJSONDuplicates(report app.DuplicateReport, scan bool) jsonDuplicates {
	out := jsonDuplicates{Scan: scan, Projects: report.Projects, Exact: report.Exact, Near: report.Near,
		Clusters: make([]jsonDuplicateCluster, 0, len(report.Clusters))}
	for _, cluster := range report.Clusters {
		members := make([]jsonDuplicateMember, 0, len(cluster.Members))
		for _, member := range cluster.Members {
			members = append(members, jsonDuplicateMember{ID: member.ID, Title: member.Title, CreatedAt: jsonTime(member.CreatedAt)})
		}
		out.Clusters = append(out.Clusters, jsonDuplicateCluster{
			ProjectID: cluster.ProjectID, Kind: cluster.Kind, Reason: cluster.Reason,
			Similarity: cluster.Similarity, Members: members,
		})
	}
	return out
}

// jsonDoctorCheck is one compatibility probe. OK is false only for a failed
// probe; Status keeps the "warn" distinction.
type jsonDoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code,omitempty"`
}

// jsonDoctor is `brain doctor --json`.
type jsonDoctor struct {
	Checks []jsonDoctorCheck `json:"checks"`
}

// gitAndGraphsFor collects the git context and code-graph indexes that
// `brain open` reports for a resolved project.
func gitAndGraphsFor(ctx context.Context, resolved app.Resolved) (gitinfo.Summary, []gitinfo.Graph) {
	summary := gitinfo.Read(ctx, gitDir(resolved))
	roots := app.LocalPaths(resolved.Project.Paths)
	if len(roots) == 0 && resolved.Path != "" {
		roots = []string{resolved.Path}
	}
	return summary, gitinfo.FindGraphs(roots)
}
