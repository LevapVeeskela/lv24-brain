package fsstore

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

type stateBody struct {
	goal          string
	active        []string
	blocked       []string
	nextAction    string
	openQuestions []string
}

func parseStateBody(body string) stateBody {
	sections := splitSections(body, "goal", "active", "blocked", "next action", "open questions")
	return stateBody{
		goal:          strings.TrimSpace(sections["goal"]),
		active:        parseListSection(sections["active"]),
		blocked:       parseListSection(sections["blocked"]),
		nextAction:    strings.TrimSpace(sections["next action"]),
		openQuestions: parseListSection(sections["open questions"]),
	}
}

// splitSections maps lowercased level-two headings to their trimmed content.
// Only known headings start a new section, so free text may contain its own
// level-two headings without being truncated.
func splitSections(body string, known ...string) map[string]string {
	knownHeadings := make(map[string]struct{}, len(known))
	for _, name := range known {
		knownHeadings[strings.ToLower(name)] = struct{}{}
	}
	sections := make(map[string]string)
	current := ""
	content := make([]string, 0, 8)
	flush := func() {
		if current != "" {
			sections[current] = strings.TrimSpace(strings.Join(content, "\n"))
		}
	}
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			heading := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")))
			if _, ok := knownHeadings[heading]; ok {
				flush()
				current = heading
				content = content[:0]
				continue
			}
		}
		if current != "" {
			content = append(content, line)
		}
	}
	flush()
	return sections
}

// parseListSection parses Markdown bullet items. Indented continuation lines are
// folded back into the item with newlines, matching writeListItem.
func parseListSection(section string) []string {
	items := make([]string, 0)
	current := make([]string, 0, 1)
	flush := func() {
		if len(current) > 0 {
			items = append(items, strings.TrimSpace(strings.Join(current, "\n")))
			current = current[:0]
		}
	}
	for _, line := range strings.Split(section, "\n") {
		switch {
		case strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* "):
			flush()
			current = append(current, strings.TrimRight(line[2:], " \t"))
		case len(current) > 0 && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")):
			continuation := strings.TrimPrefix(line, "  ")
			continuation = strings.TrimPrefix(continuation, "\t")
			current = append(current, strings.TrimRight(continuation, " \t"))
		default:
			if strings.TrimSpace(line) == "" {
				flush()
				continue
			}
			flush()
		}
	}
	flush()
	return items
}

func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimSpace(trimmed[2:])
		}
	}
	return ""
}

func bodyPreamble(body string, known ...string) string {
	knownHeadings := make(map[string]struct{}, len(known))
	for _, name := range known {
		knownHeadings[strings.ToLower(name)] = struct{}{}
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "## ") {
			continue
		}
		heading := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")))
		if _, ok := knownHeadings[heading]; ok {
			return strings.Join(lines[:i], "\n")
		}
	}
	return body
}

func parseStatusLine(preamble string) domain.DecisionStatus {
	for _, line := range strings.Split(preamble, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "status") {
			return domain.DecisionStatus(strings.TrimSpace(value))
		}
	}
	return ""
}

func parseDecisionBody(body string) (string, domain.DecisionStatus, string, []string, []string, []string) {
	sections := splitSections(body, "context", "alternatives", "consequences", "evidence")
	return firstHeading(body),
		parseStatusLine(bodyPreamble(body, "context", "alternatives", "consequences", "evidence")),
		strings.TrimSpace(sections["context"]),
		parseListSection(sections["alternatives"]),
		parseListSection(sections["consequences"]),
		parseListSection(sections["evidence"])
}

func parseAttemptBody(body string) (string, string, string, string) {
	sections := splitSections(body, "hypothesis", "result", "why it failed")
	return firstHeading(body),
		strings.TrimSpace(sections["hypothesis"]),
		strings.TrimSpace(sections["result"]),
		strings.TrimSpace(sections["why it failed"])
}

// legacyHandoffHeadings are the sections of handoffs migrated from the legacy
// tool. They bound the engine sections, so an empty "## Next action" never
// swallows the following heading, and fill summary and git context when the
// engine sections are absent.
var legacyHandoffHeadings = []string{"objective", "completed", "unfinished", "blockers", "evidence", "git log"}

func parseHandoffBody(body string) (string, string, string) {
	sections := splitSections(body, append([]string{"summary", "git context", "next action"}, legacyHandoffHeadings...)...)
	summary := strings.TrimSpace(sections["summary"])
	if summary == "" {
		summary = strings.TrimSpace(sections["objective"])
	}
	gitContext := strings.TrimSpace(sections["git context"])
	if gitContext == "" {
		gitContext = strings.TrimSpace(sections["git log"])
	}
	return summary, gitContext, strings.TrimSpace(sections["next action"])
}

func parseNoteMarker(line string) (noteMetadata, bool, error) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, noteMarkerPrefix) || !strings.HasSuffix(trimmed, noteMarkerSuffix) {
		return noteMetadata{}, false, nil
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(trimmed, noteMarkerPrefix), noteMarkerSuffix)
	var metadata noteMetadata
	if err := json.Unmarshal([]byte(payload), &metadata); err != nil {
		return noteMetadata{}, false, fmt.Errorf("invalid note metadata: %w", err)
	}
	return metadata, true, nil
}

// parseNotesBody reads marker-delimited entries written by AppendNote and also
// accepts hand-written level-two sections as notes. Hand-written notes get a
// deterministic fallback ID based on the file date.
func parseNotesBody(body string, defaultDate time.Time) ([]domain.Note, error) {
	lines := strings.Split(body, "\n")
	date := ""
	if !defaultDate.IsZero() {
		date = defaultDate.UTC().Format("2006-01-02")
	}
	used := make(map[string]struct{})
	notes := make([]domain.Note, 0)
	i := 0
	for i < len(lines) {
		metadata, isMarker, err := parseNoteMarker(lines[i])
		if err != nil {
			return nil, err
		}
		if isMarker {
			i++
			for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++
			}
			title := metadata.Title
			if i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
				title = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "## "))
				i++
			}
			if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++
			}
			start := i
			for i < len(lines) {
				if _, next, markerErr := parseNoteMarker(lines[i]); markerErr == nil && next {
					break
				}
				i++
			}
			id := metadata.ID
			if id == "" {
				id = nextNoteID(used, date, title)
			}
			used[id] = struct{}{}
			notes = append(notes, domain.Note{
				ID:        id,
				Title:     title,
				Body:      strings.TrimRight(strings.Join(lines[start:i], "\n"), "\n"),
				Tags:      nonNilStrings(metadata.Tags),
				Permalink: metadata.Permalink,
				CreatedAt: metadata.CreatedAt.UTC(),
			})
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "## ") {
			title := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "## "))
			i++
			if i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++
			}
			start := i
			for i < len(lines) {
				trimmed := strings.TrimSpace(lines[i])
				if strings.HasPrefix(trimmed, "## ") {
					break
				}
				if _, marker, markerErr := parseNoteMarker(lines[i]); markerErr == nil && marker {
					break
				}
				i++
			}
			id := nextNoteID(used, date, title)
			used[id] = struct{}{}
			createdAt := time.Time{}
			if !defaultDate.IsZero() {
				createdAt = time.Date(defaultDate.Year(), defaultDate.Month(), defaultDate.Day(), 0, 0, 0, 0, time.UTC)
			}
			notes = append(notes, domain.Note{
				ID:        id,
				Title:     title,
				Body:      strings.TrimRight(strings.Join(lines[start:i], "\n"), "\n"),
				CreatedAt: createdAt,
			})
			continue
		}
		i++
	}
	return notes, nil
}

func readNotesFile(path string) (noteFileFrontmatter, []domain.Note, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return noteFileFrontmatter{}, nil, err
	}
	var frontmatter noteFileFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		return noteFileFrontmatter{}, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, "note"); err != nil {
		return noteFileFrontmatter{}, nil, err
	}
	notes, err := parseNotesBody(body, frontmatter.UpdatedAt)
	if err != nil {
		return noteFileFrontmatter{}, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for i := range notes {
		notes[i].ProjectID = frontmatter.ProjectID
		notes[i].UpdatedAt = frontmatter.UpdatedAt.UTC()
	}
	return frontmatter, notes, nil
}

func readDecisionFile(path string) (domain.Decision, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Decision{}, err
	}
	var frontmatter decisionFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		return domain.Decision{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, "decision"); err != nil {
		return domain.Decision{}, err
	}
	title, status, contextText, alternatives, consequences, evidence := parseDecisionBody(body)
	if title == "" {
		title = frontmatter.Title
	}
	if status == "" {
		status = frontmatter.Status
	}
	if status == "" {
		status = domain.DecisionProposed
	}
	return domain.Decision{
		ID:           frontmatter.ID,
		ProjectID:    frontmatter.ProjectID,
		Title:        title,
		Status:       status,
		Context:      contextText,
		Alternatives: alternatives,
		Consequences: consequences,
		Evidence:     evidence,
		Permalink:    frontmatter.Permalink,
		CreatedAt:    frontmatter.CreatedAt.UTC(),
		UpdatedAt:    frontmatter.UpdatedAt.UTC(),
	}, nil
}

func readAttemptFile(path string) (domain.Attempt, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Attempt{}, err
	}
	var frontmatter attemptFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		return domain.Attempt{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, "attempt"); err != nil {
		return domain.Attempt{}, err
	}
	title, hypothesis, result, reason := parseAttemptBody(body)
	if title == "" {
		title = frontmatter.Title
	}
	return domain.Attempt{
		ID:         frontmatter.ID,
		ProjectID:  frontmatter.ProjectID,
		Title:      title,
		Hypothesis: hypothesis,
		Result:     result,
		Reason:     reason,
		Permalink:  frontmatter.Permalink,
		CreatedAt:  frontmatter.CreatedAt.UTC(),
		UpdatedAt:  frontmatter.UpdatedAt.UTC(),
	}, nil
}

func readHandoffFile(path string) (domain.Handoff, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Handoff{}, err
	}
	var frontmatter handoffFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		return domain.Handoff{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, "handoff"); err != nil {
		return domain.Handoff{}, err
	}
	summary, gitContext, nextAction := parseHandoffBody(body)
	return domain.Handoff{
		ID:         frontmatter.ID,
		ProjectID:  frontmatter.ProjectID,
		Summary:    summary,
		GitContext: gitContext,
		NextAction: nextAction,
		Permalink:  frontmatter.Permalink,
		CreatedAt:  frontmatter.CreatedAt.UTC(),
	}, nil
}
