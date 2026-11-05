package migrate

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"gopkg.in/yaml.v3"
)

const (
	noteMarkerPrefix = "<!-- brain:note "
	noteMarkerSuffix = " -->"
)

// legacyFrontmatter accepts both the original Company Brain keys and the
// schema-v1 keys. Timestamps are strings so legacy layouts parse leniently.
type legacyFrontmatter struct {
	ID         string           `yaml:"id"`
	ProjectID  string           `yaml:"project_id"`
	Kind       string           `yaml:"kind"`
	Title      string           `yaml:"title"`
	Status     string           `yaml:"status"`
	UpdatedAt  string           `yaml:"updated_at"`
	CreatedAt  string           `yaml:"created_at"`
	ObservedAt string           `yaml:"observed_at"`
	ValidFrom  string           `yaml:"valid_from"`
	Date       string           `yaml:"date"`
	Permalink  string           `yaml:"permalink"`
	Branch     string           `yaml:"branch"`
	Head       string           `yaml:"head"`
	DirtyFiles string           `yaml:"dirty_files"`
	SourceRefs legacyStringList `yaml:"source_refs"`
	Evidence   legacyStringList `yaml:"evidence"`
	Files      legacyStringList `yaml:"files"`
}

// legacyStringList accepts both a YAML sequence and a single scalar value,
// because hand-edited legacy frontmatter often uses a bare path.
type legacyStringList []string

func (l *legacyStringList) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var value string
		if err := node.Decode(&value); err != nil {
			return err
		}
		value = strings.TrimSpace(value)
		if value != "" && !strings.EqualFold(value, "null") {
			*l = append(*l, value)
		}
		return nil
	case yaml.SequenceNode:
		var values []string
		if err := node.Decode(&values); err != nil {
			return err
		}
		*l = values
		return nil
	case yaml.AliasNode:
		return l.UnmarshalYAML(node.Alias)
	default:
		return nil
	}
}

type noteFileFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	Date                 string `yaml:"date,omitempty"`
}

type decisionFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	ID                   string                `yaml:"id,omitempty"`
	Title                string                `yaml:"title,omitempty"`
	Status               domain.DecisionStatus `yaml:"status,omitempty"`
	CreatedAt            time.Time             `yaml:"created_at,omitempty"`
}

type attemptFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	ID                   string    `yaml:"id,omitempty"`
	Title                string    `yaml:"title,omitempty"`
	CreatedAt            time.Time `yaml:"created_at,omitempty"`
}

type handoffFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	ID                   string    `yaml:"id,omitempty"`
	CreatedAt            time.Time `yaml:"created_at,omitempty"`
}

type noteMetadata struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Tags      []string  `json:"tags,omitempty"`
	Permalink string    `json:"permalink,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type noteEntry struct {
	id        string
	title     string
	body      string
	createdAt time.Time
}

func parseLegacyDocument(raw []byte, sourceRel string) (legacyFrontmatter, string, error) {
	var frontmatter legacyFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		return legacyFrontmatter{}, "", fmt.Errorf("%s: %w", sourceRel, err)
	}
	return frontmatter, body, nil
}

func checkProjectID(frontmatter legacyFrontmatter, projectID, sourceRel string) error {
	if frontmatter.ProjectID != "" && frontmatter.ProjectID != projectID {
		return fmt.Errorf("%s belongs to project %q, not %q", sourceRel, frontmatter.ProjectID, projectID)
	}
	return nil
}

func baseFrontmatter(projectID, kind, permalink string, updatedAt time.Time) markdown.Frontmatter {
	return markdown.Frontmatter{
		ProjectID:     projectID,
		Kind:          kind,
		UpdatedAt:     updatedAt.UTC(),
		SchemaVersion: SchemaVersion,
		Permalink:     permalink,
	}
}

func convertState(projectID string, _ string, raw []byte, modTime time.Time, sourceRel string) ([]byte, error) {
	frontmatter, body, err := parseLegacyDocument(raw, sourceRel)
	if err != nil {
		return nil, err
	}
	if err := checkProjectID(frontmatter, projectID, sourceRel); err != nil {
		return nil, err
	}
	updatedAt := firstTime([]string{frontmatter.UpdatedAt}, modTime)
	return markdown.Render(baseFrontmatter(projectID, "state", frontmatter.Permalink, updatedAt), strings.TrimSpace(body))
}

func convertNotes(projectID, fileName string, raw []byte, modTime time.Time, sourceRel string) ([]byte, error) {
	frontmatter, body, err := parseLegacyDocument(raw, sourceRel)
	if err != nil {
		return nil, err
	}
	if err := checkProjectID(frontmatter, projectID, sourceRel); err != nil {
		return nil, err
	}
	date := noteFileDate(frontmatter, fileName, modTime)
	entries := parseNoteEntries(body, date)
	updatedAt := date
	for _, entry := range entries {
		if entry.createdAt.After(updatedAt) {
			updatedAt = entry.createdAt
		}
	}
	out := noteFileFrontmatter{
		Frontmatter: baseFrontmatter(projectID, "note", frontmatter.Permalink, updatedAt),
		Date:        date.Format("2006-01-02"),
	}
	return markdown.Render(out, renderNotesBody(entries))
}

func noteFileDate(frontmatter legacyFrontmatter, fileName string, modTime time.Time) time.Time {
	if parsed, ok := parseLegacyTime(frontmatter.Date); ok {
		return dateOf(parsed)
	}
	stem := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	if parsed, err := time.Parse("2006-01-02", stem); err == nil {
		return dateOf(parsed)
	}
	if parsed, ok := parseLegacyTime(frontmatter.UpdatedAt); ok {
		return dateOf(parsed)
	}
	return dateOf(modTime.UTC())
}

func parseNoteEntries(body string, date time.Time) []noteEntry {
	preamble, sections := splitH2Sections(body)
	entries := make([]noteEntry, 0, len(sections)+1)
	if text, title := notePreamble(preamble); text != "" {
		entries = append(entries, noteEntry{title: title, body: text})
	}
	for _, current := range sections {
		entries = append(entries, noteEntry{title: current.title, body: current.body})
	}
	used := make(map[string]struct{}, len(entries))
	day := date.Format("2006-01-02")
	for index := range entries {
		entries[index].title = singleLine(entries[index].title)
		if entries[index].title == "" {
			entries[index].title = "Note"
		}
		entries[index].createdAt = noteCreatedAt(date, entries[index].title)
		entries[index].id = uniqueName(used, "note-"+day+"-"+slug(entries[index].title))
	}
	return entries
}

// notePreamble returns the text before the first level-two heading, excluding
// the document title.
func notePreamble(preamble string) (string, string) {
	lines := strings.Split(preamble, "\n")
	title := ""
	found := false
	rest := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !found && strings.HasPrefix(trimmed, "# ") {
			title = strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
			found = true
			continue
		}
		rest = append(rest, line)
	}
	return strings.TrimSpace(strings.Join(rest, "\n")), title
}

func preambleBody(preamble string) string {
	body, _ := notePreamble(preamble)
	return body
}

func noteCreatedAt(date time.Time, title string) time.Time {
	hour, minute, second, ok := parseClock(title)
	if !ok {
		return date
	}
	return time.Date(date.Year(), date.Month(), date.Day(), hour, minute, second, 0, time.UTC)
}

func renderNotesBody(entries []noteEntry) string {
	var out strings.Builder
	for _, entry := range entries {
		rawMetadata, err := json.Marshal(noteMetadata{
			ID:        entry.id,
			Title:     entry.title,
			CreatedAt: entry.createdAt.UTC(),
		})
		if err != nil {
			continue
		}
		out.WriteString(noteMarkerPrefix)
		out.Write(rawMetadata)
		out.WriteString(noteMarkerSuffix)
		out.WriteByte('\n')
		out.WriteString("## ")
		out.WriteString(singleLine(entry.title))
		out.WriteString("\n\n")
		out.WriteString(strings.TrimRight(entry.body, "\n"))
		out.WriteString("\n\n")
	}
	return out.String()
}

func convertDecision(projectID, fileName string, raw []byte, modTime time.Time, sourceRel string) ([]byte, error) {
	frontmatter, body, err := parseLegacyDocument(raw, sourceRel)
	if err != nil {
		return nil, err
	}
	if err := checkProjectID(frontmatter, projectID, sourceRel); err != nil {
		return nil, err
	}
	title := firstHeading(body)
	if title == "" {
		title = singleLine(frontmatter.Title)
	}
	if title == "" {
		title = titleFromFileName(fileName)
	}
	if title == "" {
		return nil, fmt.Errorf("%s: decision has no title", sourceRel)
	}
	preamble, sections := splitH2Sections(body)
	context := joinText(preambleBody(preamble), decisionContext(sections))
	alternatives := parseList(sectionText(sections, "alternatives considered", "alternatives"))
	for _, item := range parseList(sectionText(sections, "rejected approaches", "rejected")) {
		alternatives = append(alternatives, "Rejected: "+item)
	}
	consequences := parseList(sectionText(sections, "consequences"))
	for _, item := range parseList(sectionText(sections, "revisit triggers", "revisit trigger")) {
		consequences = append(consequences, "Revisit: "+item)
	}
	evidence := make([]string, 0, len(frontmatter.SourceRefs)+len(frontmatter.Evidence))
	evidence = appendNonEmpty(evidence, frontmatter.SourceRefs...)
	evidence = appendNonEmpty(evidence, frontmatter.Evidence...)
	evidence = append(evidence, parseList(sectionText(sections, "evidence"))...)

	createdAt := firstTime([]string{frontmatter.CreatedAt, frontmatter.ValidFrom}, modTime)
	id := strings.TrimSpace(frontmatter.ID)
	if id == "" {
		id = "decision-" + strings.TrimSuffix(fileName, filepath.Ext(fileName))
	}
	status := normalizeDecisionStatus(frontmatter.Status)
	out := decisionFrontmatter{
		Frontmatter: baseFrontmatter(projectID, "decision", frontmatter.Permalink, createdAt),
		ID:          id,
		Title:       title,
		Status:      status,
		CreatedAt:   createdAt,
	}
	return markdown.Render(out, renderDecisionBody(title, status, context, alternatives, consequences, evidence))
}

func decisionContext(sections []section) string {
	parts := make([]string, 0, 4)
	if text := sectionText(sections, "context"); text != "" {
		parts = append(parts, text)
	}
	if text := sectionText(sections, "decision"); text != "" {
		parts = append(parts, "### Decision\n\n"+text)
	}
	for _, current := range sections {
		if current.body == "" || isKnownDecisionSection(current.title) {
			continue
		}
		parts = append(parts, "### "+singleLine(current.title)+"\n\n"+current.body)
	}
	return strings.Join(parts, "\n\n")
}

func isKnownDecisionSection(title string) bool {
	switch normalizeHeading(title) {
	case "context", "decision", "alternatives considered", "alternatives",
		"rejected approaches", "rejected", "consequences", "evidence",
		"revisit triggers", "revisit trigger":
		return true
	default:
		return false
	}
}

func normalizeDecisionStatus(value string) domain.DecisionStatus {
	switch normalizeHeading(value) {
	case "accepted", "approved", "active", "decided":
		return domain.DecisionAccepted
	case "rejected", "declined":
		return domain.DecisionRejected
	case "superseded", "deprecated", "obsolete":
		return domain.DecisionSuperseded
	default:
		return domain.DecisionProposed
	}
}

func convertAttempt(projectID, fileName string, raw []byte, modTime time.Time, sourceRel string) ([]byte, error) {
	frontmatter, body, err := parseLegacyDocument(raw, sourceRel)
	if err != nil {
		return nil, err
	}
	if err := checkProjectID(frontmatter, projectID, sourceRel); err != nil {
		return nil, err
	}
	title := firstHeading(body)
	if title == "" {
		title = singleLine(frontmatter.Title)
	}
	if title == "" {
		title = titleFromFileName(fileName)
	}
	if title == "" {
		return nil, fmt.Errorf("%s: attempt has no title", sourceRel)
	}
	preamble, sections := splitH2Sections(body)
	hypothesis := sectionText(sections, "hypothesis")
	result := joinText(preambleBody(preamble), attemptResult(sections))
	reason := attemptReason(sections)
	if len(frontmatter.Files) > 0 {
		var extra strings.Builder
		writeSubsection(&extra, "Files", strings.Join(frontmatter.Files, "\n"))
		result = joinText(result, strings.TrimSpace(extra.String()))
	}
	if len(frontmatter.Evidence) > 0 {
		var extra strings.Builder
		writeSubsection(&extra, "Evidence", strings.Join(frontmatter.Evidence, "\n"))
		result = joinText(result, strings.TrimSpace(extra.String()))
	}

	createdAt := firstTime([]string{frontmatter.CreatedAt, frontmatter.ObservedAt}, modTime)
	id := strings.TrimSpace(frontmatter.ID)
	if id == "" {
		id = "attempt-" + strings.TrimSuffix(fileName, filepath.Ext(fileName))
	}
	out := attemptFrontmatter{
		Frontmatter: baseFrontmatter(projectID, "attempt", frontmatter.Permalink, createdAt),
		ID:          id,
		Title:       title,
		CreatedAt:   createdAt,
	}
	return markdown.Render(out, renderAttemptBody(title, hypothesis, result, reason))
}

func attemptResult(sections []section) string {
	parts := make([]string, 0, 4)
	if text := sectionText(sections, "symptom"); text != "" {
		parts = append(parts, "### Symptom\n\n"+text)
	}
	if text := sectionText(sections, "what we tried"); text != "" {
		parts = append(parts, "### What we tried\n\n"+text)
	}
	for _, current := range sections {
		if current.body == "" || isKnownAttemptSection(current.title) {
			continue
		}
		parts = append(parts, "### "+singleLine(current.title)+"\n\n"+current.body)
	}
	return strings.Join(parts, "\n\n")
}

func attemptReason(sections []section) string {
	parts := make([]string, 0, 2)
	if text := sectionText(sections, "outcome / why it failed", "outcome", "why it failed"); text != "" {
		parts = append(parts, text)
	}
	if text := sectionText(sections, "what to try next"); text != "" {
		parts = append(parts, "### What to try next\n\n"+text)
	}
	return strings.Join(parts, "\n\n")
}

func isKnownAttemptSection(title string) bool {
	switch normalizeHeading(title) {
	case "symptom", "hypothesis", "what we tried", "outcome / why it failed",
		"outcome", "why it failed", "what to try next":
		return true
	default:
		return false
	}
}

func convertHandoff(projectID string, _ string, raw []byte, modTime time.Time, sourceRel string) ([]byte, error) {
	frontmatter, body, err := parseLegacyDocument(raw, sourceRel)
	if err != nil {
		return nil, err
	}
	if err := checkProjectID(frontmatter, projectID, sourceRel); err != nil {
		return nil, err
	}
	preamble, sections := splitH2Sections(body)
	summary := joinText(preambleBody(preamble), handoffSummary(sections, firstHeading(body)))
	gitContext := handoffGitContext(frontmatter, sections)
	nextAction := sectionText(sections, "next action", "next")

	createdAt := firstTime([]string{frontmatter.CreatedAt}, modTime)
	id := strings.TrimSpace(frontmatter.ID)
	if id == "" {
		id = "handoff-" + createdAt.UTC().Format("2006-01-02-150405")
	}
	out := handoffFrontmatter{
		Frontmatter: baseFrontmatter(projectID, "handoff", frontmatter.Permalink, createdAt),
		ID:          id,
		CreatedAt:   createdAt,
	}
	return markdown.Render(out, renderHandoffBody(createdAt, summary, gitContext, nextAction))
}

func handoffSummary(sections []section, fallback string) string {
	var out strings.Builder
	out.WriteString(sectionText(sections, "objective"))
	if out.Len() > 0 {
		out.WriteString("\n\n")
	}
	writeSubsection(&out, "Completed", sectionText(sections, "completed"))
	writeSubsection(&out, "Unfinished", sectionText(sections, "unfinished"))
	writeSubsection(&out, "Blockers", sectionText(sections, "blockers"))
	writeSubsection(&out, "Evidence", sectionText(sections, "evidence"))
	for _, current := range sections {
		if current.body == "" || isKnownHandoffSection(current.title) {
			continue
		}
		writeSubsection(&out, current.title, current.body)
	}
	summary := strings.TrimSpace(out.String())
	if summary == "" {
		summary = singleLine(fallback)
	}
	return summary
}

func handoffGitContext(frontmatter legacyFrontmatter, sections []section) string {
	var out strings.Builder
	if frontmatter.Branch != "" {
		out.WriteString("branch: ")
		out.WriteString(frontmatter.Branch)
		out.WriteByte('\n')
	}
	if frontmatter.Head != "" {
		out.WriteString("head: ")
		out.WriteString(frontmatter.Head)
		out.WriteByte('\n')
	}
	if frontmatter.DirtyFiles != "" {
		out.WriteString("dirty_files: ")
		out.WriteString(frontmatter.DirtyFiles)
		out.WriteByte('\n')
	}
	if frontmatter.Status != "" {
		out.WriteString("status: ")
		out.WriteString(frontmatter.Status)
		out.WriteByte('\n')
	}
	gitLog := sectionText(sections, "git log")
	if gitLog != "" {
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString("### Git log\n\n")
		out.WriteString(gitLog)
	}
	return strings.TrimSpace(out.String())
}

func isKnownHandoffSection(title string) bool {
	switch normalizeHeading(title) {
	case "objective", "completed", "unfinished", "blockers",
		"next action", "next", "evidence", "git log":
		return true
	default:
		return false
	}
}

func appendNonEmpty(values []string, candidates ...string) []string {
	for _, candidate := range candidates {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	return values
}

func joinText(left, right string) string {
	switch {
	case left == "":
		return right
	case right == "":
		return left
	default:
		return left + "\n\n" + right
	}
}

func titleFromFileName(fileName string) string {
	stem := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	if len(stem) > 11 && stem[10] == '-' {
		if _, err := time.Parse("2006-01-02", stem[:10]); err == nil {
			stem = stem[11:]
		}
	}
	return strings.TrimSpace(strings.ReplaceAll(stem, "-", " "))
}

func renderDecisionBody(title string, status domain.DecisionStatus, context string, alternatives, consequences, evidence []string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", singleLine(title))
	fmt.Fprintf(&out, "Status: %s\n\n", status)
	out.WriteString("## Context\n\n")
	out.WriteString(strings.TrimSpace(context))
	out.WriteString("\n\n## Alternatives\n\n")
	writeMarkdownList(&out, alternatives)
	out.WriteString("\n## Consequences\n\n")
	writeMarkdownList(&out, consequences)
	out.WriteString("\n## Evidence\n\n")
	writeMarkdownList(&out, evidence)
	return out.String()
}

func renderAttemptBody(title, hypothesis, result, reason string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", singleLine(title))
	out.WriteString("## Hypothesis\n\n")
	out.WriteString(strings.TrimSpace(hypothesis))
	out.WriteString("\n\n## Result\n\n")
	out.WriteString(strings.TrimSpace(result))
	out.WriteString("\n\n## Why it failed\n\n")
	out.WriteString(strings.TrimSpace(reason))
	return out.String()
}

func renderHandoffBody(createdAt time.Time, summary, gitContext, nextAction string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Handoff — %s\n\n", createdAt.UTC().Format(time.RFC3339))
	out.WriteString("## Summary\n\n")
	out.WriteString(strings.TrimSpace(summary))
	out.WriteString("\n\n## Git context\n\n")
	out.WriteString(strings.TrimSpace(gitContext))
	out.WriteString("\n\n## Next action\n\n")
	out.WriteString(strings.TrimSpace(nextAction))
	return out.String()
}
