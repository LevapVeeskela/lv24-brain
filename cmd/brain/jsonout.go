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
	ProjectID string `json:"project_id"`
	Summary   string `json:"summary"`
	Budget    int    `json:"budget"`
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
	ID        string  `json:"id"`
	ProjectID string  `json:"project_id"`
	Kind      string  `json:"kind"`
	Title     string  `json:"title"`
	Snippet   string  `json:"snippet"`
	Score     float64 `json:"score"`
}

// jsonSearch is `brain search --json`.
type jsonSearch struct {
	Query string    `json:"query"`
	Hits  []jsonHit `json:"hits"`
}

func toJSONHits(hits []ports.Hit) []jsonHit {
	out := make([]jsonHit, 0, len(hits))
	for _, hit := range hits {
		out = append(out, jsonHit{
			ID:        hit.ID,
			ProjectID: hit.ProjectID,
			Kind:      hit.Kind,
			Title:     hit.Title,
			Snippet:   hit.Snippet,
			Score:     hit.Score,
		})
	}
	return out
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

// jsonReview is `brain review --json`: the review text plus its items.
type jsonReview struct {
	Summary string   `json:"summary"`
	Items   []string `json:"items"`
}

// jsonDoctorCheck is one compatibility probe. OK is false only for a failed
// probe; Status keeps the "warn" distinction.
type jsonDoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Status string `json:"status"`
	Detail string `json:"detail"`
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
