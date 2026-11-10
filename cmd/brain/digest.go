package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/app"
	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
)

const (
	digestDefaultSince = "24h"
	digestDirName      = "digests"
	digestDateLayout   = "2006-01-02"
)

// digestNeeds is everything that waits for the owner, across projects.
type digestNeeds struct {
	ProposedDecisions []digestNeedItem    `json:"proposed_decisions"`
	OpenQuestions     []digestNeedItem    `json:"open_questions"`
	Blocked           []digestNeedItem    `json:"blocked"`
	ReviewFindings    []digestFindingItem `json:"review_findings"`
}

type digestNeedItem struct {
	Project string `json:"project"`
	ID      string `json:"id,omitempty"`
	Text    string `json:"text"`
	Status  string `json:"status,omitempty"`
}

type digestFindingItem struct {
	Project string         `json:"project"`
	Count   int            `json:"count"`
	Kinds   map[string]int `json:"kinds"`
}

// runDigest implements `brain digest`: a morning summary across registered
// projects (or one with --project) of what changed in the window and what
// still needs the owner. With --source it previews proposals from the stored
// raw items of one connection and never runs the source.
func runDigest(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("digest", stderr)
	since := fs.String("since", digestDefaultSince, "window to report: Go duration (90m, 24h) or days (7d); with --source the default is 30d")
	project := fs.String("project", "", "report one registered project only")
	source := fs.String("source", "", "preview proposals from the stored items of one connection (never runs the source)")
	all := fs.Bool("all", false, "with --source: ignore --since and scan all stored items")
	limit := fs.Int("limit", app.DefaultProposalDigestLimit, "with --source: maximum stored items to scan")
	rule := fs.String("rule", "", "with --source: comma-separated rules ("+strings.Join(app.ProposalRuleNames(), ", ")+")")
	asJSON := fs.Bool("json", false, "print JSON")
	write := fs.Bool("write", false, "save the human digest to <data_dir>/digests/YYYY-MM-DD.md; with --source: persist proposals")
	dryRun := fs.Bool("dry-run", false, "with --source: preview only (the default; cannot be combined with --write)")
	cfgPath := configFlag(fs)
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) > 0 {
		fmt.Fprintf(stderr, "brain digest: unexpected arguments: %s\n", strings.Join(positional, " "))
		return 2
	}
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if !seen["source"] {
		if seen["all"] || seen["limit"] || seen["rule"] || seen["dry-run"] {
			fmt.Fprintln(stderr, "brain digest: --all, --limit, --rule and --dry-run need --source <connection>")
			return 2
		}
		return runActivityDigest(*since, *project, *cfgPath, *asJSON, *write, stdout, stderr)
	}
	return runProposalDigest(*source, *project, *since, *cfgPath, *all, *limit, *rule, *write, *dryRun, *asJSON, seen, stdout, stderr)
}

// runActivityDigest is the original cross-project digest.
func runActivityDigest(sinceValue, project, cfgPath string, asJSON, write bool, stdout, stderr io.Writer) int {
	window, err := parseDigestWindow(sinceValue)
	if err != nil {
		fmt.Fprintf(stderr, "brain digest: %v\n", err)
		return 2
	}
	ctx := context.Background()
	cfg, err := loadRequiredConfig(cfgPath)
	if err != nil {
		return reportError(stderr, "digest", err)
	}
	if write && cfg.Mode == config.ModeServer {
		fmt.Fprintln(stderr, "brain digest: --write needs local or git mode; run it on the machine that owns the data dir")
		return 2
	}
	runtime, err := buildRuntime(cfg)
	if err != nil {
		return reportError(stderr, "digest", err)
	}
	defer func() { _ = runtime.Close() }()

	generated := time.Now()
	digest, err := runtime.App.Digest(ctx, app.DigestOptions{ProjectID: project, Since: generated.Add(-window)})
	if err != nil {
		return reportError(stderr, "digest", err)
	}
	human := renderDigest(digest, sinceValue)
	var writtenTo string
	if write {
		status, err := runtime.App.Status(ctx)
		if err != nil {
			return reportError(stderr, "digest", err)
		}
		writtenTo, err = writeDigestFile(status.DataDir, digest.GeneratedAt, human)
		if err != nil {
			return reportError(stderr, "digest", err)
		}
	}
	if asJSON {
		return writeJSONReport(stdout, stderr, "digest", toJSONDigest(digest, sinceValue, writtenTo))
	}
	_, _ = io.WriteString(stdout, human)
	if writtenTo != "" {
		fmt.Fprintf(stdout, "wrote %s\n", writtenTo)
	}
	return 0
}

// runProposalDigest previews or writes proposals from stored raw items. It is
// deliberately offline: only the item store, connection catalogue and proposal
// store are read.
func runProposalDigest(source, project, sinceValue, cfgPath string, all bool, limit int, rules string, write, dryRun, asJSON bool, seen map[string]bool, stdout, stderr io.Writer) int {
	if strings.TrimSpace(source) == "" {
		fmt.Fprintln(stderr, "brain digest: --source needs a connection id")
		return 2
	}
	if write && dryRun {
		fmt.Fprintln(stderr, "brain digest: --dry-run cannot be combined with --write")
		return 2
	}
	if seen["limit"] && limit <= 0 {
		fmt.Fprintln(stderr, "brain digest: --limit must be positive")
		return 2
	}
	window := app.DefaultProposalDigestWindow
	windowLabel := "30d"
	if seen["since"] {
		parsed, err := parseDigestWindow(sinceValue)
		if err != nil {
			fmt.Fprintf(stderr, "brain digest: %v\n", err)
			return 2
		}
		window = parsed
		windowLabel = strings.TrimSpace(sinceValue)
	}
	if all {
		windowLabel = "all"
	}
	cfg, err := loadRequiredConfig(cfgPath)
	if err != nil {
		return reportError(stderr, "digest", err)
	}
	if cfg.Mode == config.ModeServer {
		fmt.Fprintln(stderr, "brain digest: --source needs local or git mode; run it on the machine that owns the data dir")
		return 2
	}
	runtime, err := buildRuntime(cfg)
	if err != nil {
		return reportError(stderr, "digest", err)
	}
	defer func() { _ = runtime.Close() }()

	opts := app.ProposalDigestOptions{
		ConnectionID: source,
		ProjectID:    project,
		All:          all,
		Limit:        limit,
		Rules:        splitList(rules),
		Write:        write,
		DryRun:       dryRun,
	}
	if seen["since"] && !all {
		opts.Since = time.Now().Add(-window)
	}
	result, err := runtime.App.ProposalDigest(context.Background(), opts)
	if err != nil {
		if errors.Is(err, domain.ErrValidation) {
			fmt.Fprintf(stderr, "brain digest: %v\n", err)
			return 2
		}
		return reportError(stderr, "digest", err)
	}
	if asJSON {
		if err := writeJSONMode(stdout, "proposals", toJSONProposalDigest(result, windowLabel, dryRun)); err != nil {
			return reportError(stderr, "digest", err)
		}
		return 0
	}
	_, _ = io.WriteString(stdout, renderProposalDigest(result, windowLabel))
	return 0
}

// renderProposalDigest is the human output of `brain digest --source`.
func renderProposalDigest(result app.ProposalDigest, window string) string {
	var b bytes.Buffer
	mode := "preview"
	if result.Write {
		mode = "write"
	}
	fmt.Fprintf(&b, "brain digest --source %s — %s (%s)\n", result.ConnectionID, result.ProjectID, mode)
	if result.All || result.Since == nil {
		b.WriteString("window: all\n")
	} else {
		fmt.Fprintf(&b, "window: %s (since %s)\n", window, formatCatchupTime(*result.Since))
	}
	fmt.Fprintf(&b, "scanned: %d\n", result.Counts.Scanned)
	fmt.Fprintf(&b, "created: %d\n", result.Counts.Created)
	fmt.Fprintf(&b, "updated: %d\n", result.Counts.Updated)
	fmt.Fprintf(&b, "unchanged: %d\n", result.Counts.Unchanged)
	fmt.Fprintf(&b, "skipped: %d\n", result.Counts.Skipped)
	if result.Counts.Truncated {
		b.WriteString("truncated: true (raise --limit to scan more)\n")
	} else {
		b.WriteString("truncated: false\n")
	}
	b.WriteString("\nproposals:\n")
	if len(result.Entries) == 0 {
		b.WriteString("  none\n")
	} else {
		for _, entry := range result.Entries {
			fmt.Fprintf(&b, "  %-9s %-8s %-15s %.2f  %s\n",
				entry.Action, entry.Proposal.Kind, entry.Proposal.Rule,
				entry.Proposal.Confidence, catchupLine(entry.Proposal.Title))
		}
	}
	if result.Write {
		fmt.Fprintf(&b, "\nwritten: %d\n", result.Written())
	} else {
		b.WriteString("\npreview: no proposal files written; rerun with --write to persist\n")
	}
	return b.String()
}

// parseDigestWindow accepts Go durations plus a whole number of days (7d).
func parseDigestWindow(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if days, ok := strings.CutSuffix(value, "d"); ok {
		count, err := strconv.Atoi(days)
		if err != nil || count <= 0 {
			return 0, fmt.Errorf("--since %q: days must be a positive whole number, e.g. 7d", value)
		}
		return time.Duration(count) * 24 * time.Hour, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("--since %q: use a positive Go duration (90m, 24h) or days (7d)", value)
	}
	return parsed, nil
}

// writeDigestFile saves the digest as <dataDir>/digests/YYYY-MM-DD.md, one
// file per local day; a second run the same day overwrites it.
func writeDigestFile(dataDir string, generated time.Time, content string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("data dir is not set; cannot write the digest")
	}
	dir := filepath.Join(dataDir, digestDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, generated.Local().Format(digestDateLayout)+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// renderDigest is the human digest; --write saves exactly this text.
func renderDigest(digest app.Digest, window string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "brain digest — last %s (since %s)\n", window, formatCatchupTime(digest.Since))
	fmt.Fprintf(&b, "generated %s\n", formatCatchupTime(digest.GeneratedAt))
	if len(digest.Projects) == 0 {
		b.WriteString("\nnothing to report\n")
		return b.String()
	}
	for _, project := range digest.Projects {
		fmt.Fprintf(&b, "\n## %s — %d change(s)\n", project.Project.ID, project.ActivityCount())
		renderDigestProject(&b, project)
	}
	needs := collectDigestNeeds(digest)
	b.WriteString("\n## Needs you\n")
	if needsEmpty(needs) {
		b.WriteString("nothing\n")
		return b.String()
	}
	writeNeedSection(&b, "proposed decisions", mapAll(needs.ProposedDecisions, func(item digestNeedItem) string {
		return fmt.Sprintf("%s %s [%s]", item.ID, catchupLine(item.Text), item.Project)
	}))
	writeNeedSection(&b, "open questions", mapAll(needs.OpenQuestions, needLine))
	writeNeedSection(&b, "blocked", mapAll(needs.Blocked, needLine))
	writeNeedSection(&b, "review findings", mapAll(needs.ReviewFindings, func(item digestFindingItem) string {
		return fmt.Sprintf("[%s] %d (%s)", item.Project, item.Count, formatKindCounts(item.Kinds))
	}))
	return b.String()
}

func renderDigestProject(b *bytes.Buffer, project app.DigestProject) {
	activity := project.Activity
	fmt.Fprintf(b, "notes %d, decisions %d, attempts %d, handoffs %d, commits %d\n",
		len(activity.Notes), len(activity.Decisions), len(activity.Attempts), len(activity.Handoffs), project.Commits)
	if project.LatestHandoff != nil {
		next := valueOr(catchupLine(project.LatestHandoff.NextAction), "none")
		fmt.Fprintf(b, "latest handoff: %s %s -> next: %s\n",
			formatCatchupTime(project.LatestHandoff.CreatedAt), catchupLine(project.LatestHandoff.Summary), next)
	}
	printSection(b, "new decisions", mapAll(activity.Decisions, decisionLine))
	printSection(b, "attempts", mapAll(activity.Attempts, attemptLine))
	if activity.StateChanged {
		fmt.Fprintf(b, "state: goal=%s next=%s\n", valueOr(activity.State.Goal, "not set"), valueOr(activity.State.NextAction, "not set"))
	}
}

// collectDigestNeeds flattens the per-project needs into one owner list.
func collectDigestNeeds(digest app.Digest) digestNeeds {
	needs := digestNeeds{
		ProposedDecisions: []digestNeedItem{},
		OpenQuestions:     []digestNeedItem{},
		Blocked:           []digestNeedItem{},
		ReviewFindings:    []digestFindingItem{},
	}
	for _, project := range digest.Projects {
		id := project.Project.ID
		for _, decision := range project.ProposedDecisions {
			needs.ProposedDecisions = append(needs.ProposedDecisions, digestNeedItem{
				Project: id, ID: decision.ID, Text: decision.Title, Status: decision.Status,
			})
		}
		for _, question := range project.OpenQuestions {
			needs.OpenQuestions = append(needs.OpenQuestions, digestNeedItem{Project: id, Text: question})
		}
		for _, blocked := range project.Blocked {
			needs.Blocked = append(needs.Blocked, digestNeedItem{Project: id, Text: blocked})
		}
		if count := project.FindingCount(); count > 0 {
			kinds := map[string]int{}
			for kind, n := range project.Findings {
				kinds[string(kind)] = n
			}
			needs.ReviewFindings = append(needs.ReviewFindings, digestFindingItem{Project: id, Count: count, Kinds: kinds})
		}
	}
	return needs
}

func needsEmpty(needs digestNeeds) bool {
	return len(needs.ProposedDecisions) == 0 && len(needs.OpenQuestions) == 0 &&
		len(needs.Blocked) == 0 && len(needs.ReviewFindings) == 0
}

func needLine(item digestNeedItem) string {
	return fmt.Sprintf("[%s] %s", item.Project, catchupLine(item.Text))
}

func writeNeedSection(b *bytes.Buffer, label string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(b, "%s (%d):\n", label, len(lines))
	for _, line := range lines {
		fmt.Fprintf(b, "- %s\n", line)
	}
}

func formatKindCounts(kinds map[string]int) string {
	names := make([]string, 0, len(kinds))
	for name := range kinds {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s %d", name, kinds[name]))
	}
	return strings.Join(parts, ", ")
}

// jsonDigest is `brain digest --json`.
type jsonDigest struct {
	Window      string              `json:"window"`
	Since       *string             `json:"since"`
	GeneratedAt *string             `json:"generated_at"`
	WrittenTo   *string             `json:"written_to"`
	Projects    []jsonDigestProject `json:"projects"`
	NeedsYou    digestNeeds         `json:"needs_you"`
}

type jsonDigestProject struct {
	Project       string         `json:"project"`
	ChangeCount   int            `json:"change_count"`
	Notes         int            `json:"notes"`
	Decisions     []jsonDecision `json:"decisions"`
	Attempts      []jsonAttempt  `json:"attempts"`
	Handoffs      int            `json:"handoffs"`
	Commits       int            `json:"commits"`
	LatestHandoff *jsonHandoff   `json:"latest_handoff"`
	StateChanged  bool           `json:"state_changed"`
	State         jsonState      `json:"state"`
}

func toJSONDigest(digest app.Digest, window, writtenTo string) jsonDigest {
	out := jsonDigest{
		Window:      window,
		Since:       jsonTime(digest.Since),
		GeneratedAt: jsonTime(digest.GeneratedAt),
		Projects:    mapAll(digest.Projects, toJSONDigestProject),
		NeedsYou:    collectDigestNeeds(digest),
	}
	if writtenTo != "" {
		out.WrittenTo = &writtenTo
	}
	return out
}

func toJSONDigestProject(project app.DigestProject) jsonDigestProject {
	out := jsonDigestProject{
		Project:      project.Project.ID,
		ChangeCount:  project.ActivityCount(),
		Notes:        len(project.Activity.Notes),
		Decisions:    mapAll(project.Activity.Decisions, toJSONDecision),
		Attempts:     mapAll(project.Activity.Attempts, toJSONAttempt),
		Handoffs:     len(project.Activity.Handoffs),
		Commits:      project.Commits,
		StateChanged: project.Activity.StateChanged,
		State:        toJSONState(project.Activity.State),
	}
	if project.LatestHandoff != nil {
		latest := toJSONHandoff(*project.LatestHandoff)
		out.LatestHandoff = &latest
	}
	return out
}
