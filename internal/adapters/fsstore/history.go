package fsstore

import (
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// historyHeading is the level-two heading that separates the current body of a
// topic record from its previous bodies. It is part of the Markdown body, not
// frontmatter, so a reader of the raw file sees the whole evolution.
const historyHeading = "## History"

// renderHistorySection renders history newest first. Each entry carries its
// update instant as a level-three heading so the parser can tell entries apart
// even when a previous body has headings of its own.
func renderHistorySection(history []domain.HistoryEntry) string {
	history = domain.BoundedHistory(history)
	if len(history) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(historyHeading)
	builder.WriteString("\n")
	for _, entry := range history {
		builder.WriteString("\n")
		if at := strings.TrimSpace(entry.At); at != "" {
			builder.WriteString("### ")
			builder.WriteString(at)
			builder.WriteString("\n\n")
		}
		builder.WriteString(strings.TrimSpace(entry.Body))
		builder.WriteString("\n")
	}
	return builder.String()
}

// splitHistorySection separates the current body from its bounded history. A
// body without the heading is returned unchanged. The heading is recognized
// case-insensitively so a hand-written "## history" is preserved instead of
// being duplicated on the next topic update.
func splitHistorySection(body string) (string, []domain.HistoryEntry) {
	lines := strings.Split(body, "\n")
	start := -1
	for i, line := range lines {
		if strings.EqualFold(strings.TrimSpace(line), historyHeading) {
			start = i
			break
		}
	}
	if start < 0 {
		return body, nil
	}
	current := strings.TrimRight(strings.Join(lines[:start], "\n"), "\n")
	return current, parseHistorySection(strings.Join(lines[start+1:], "\n"))
}

// parseHistorySection reads timestamped entries. Text before the first heading
// (or without any heading) is kept as one entry with no timestamp, so a
// hand-written history section survives a later update instead of vanishing.
func parseHistorySection(section string) []domain.HistoryEntry {
	entries := make([]domain.HistoryEntry, 0, 4)
	var at string
	var lines []string
	flush := func() {
		body := strings.TrimSpace(strings.Join(lines, "\n"))
		if body != "" {
			entries = append(entries, domain.HistoryEntry{At: at, Body: body})
		}
		lines = nil
	}
	for _, line := range strings.Split(section, "\n") {
		if heading, ok := parseHistoryHeading(line); ok {
			flush()
			at = heading
			continue
		}
		if at == "" && len(lines) == 0 && strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	flush()
	return domain.BoundedHistory(entries)
}

// parseHistoryHeading recognizes "### <RFC3339>" and returns the canonical
// timestamp. Anything else is body text.
func parseHistoryHeading(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "### ") {
		return "", false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(trimmed, "### "))
	if raw == "" {
		return "", false
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", false
	}
	return at.UTC().Format(time.RFC3339), true
}

// appendHistory renders the current body plus its history section.
func appendHistory(body string, history []domain.HistoryEntry) string {
	section := renderHistorySection(history)
	body = strings.TrimRight(body, "\n")
	if section == "" {
		return body
	}
	if body == "" {
		return section
	}
	return body + "\n\n" + section
}
