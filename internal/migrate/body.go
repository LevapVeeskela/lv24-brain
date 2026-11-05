package migrate

import (
	"strconv"
	"strings"
	"time"
)

type section struct {
	title string
	body  string
}

// splitH2Sections splits a Markdown body at level-two headings. The preamble is
// everything before the first level-two heading.
func splitH2Sections(body string) (string, []section) {
	lines := strings.Split(body, "\n")
	preamble := make([]string, 0, 4)
	sections := make([]section, 0, 6)
	title := ""
	content := make([]string, 0, 8)
	flush := func() {
		if title == "" {
			return
		}
		sections = append(sections, section{title: title, body: strings.TrimSpace(strings.Join(content, "\n"))})
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			flush()
			title = strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))
			content = content[:0]
			continue
		}
		if title == "" {
			preamble = append(preamble, line)
		} else {
			content = append(content, line)
		}
	}
	flush()
	return strings.TrimSpace(strings.Join(preamble, "\n")), sections
}

func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		}
	}
	return ""
}

func normalizeHeading(value string) string {
	normalized := singleLine(strings.ToLower(value))
	return strings.TrimSuffix(normalized, ":")
}

func sectionText(sections []section, names ...string) string {
	wanted := make(map[string]struct{}, len(names))
	for _, name := range names {
		wanted[normalizeHeading(name)] = struct{}{}
	}
	parts := make([]string, 0, len(names))
	for _, current := range sections {
		if _, ok := wanted[normalizeHeading(current.title)]; !ok || current.body == "" {
			continue
		}
		parts = append(parts, current.body)
	}
	return strings.Join(parts, "\n\n")
}

// parseList turns a section body into list items. Markdown bullets and numbered
// items are split; free-form prose becomes one item so no content is dropped.
func parseList(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	items := make([]string, 0, len(lines))
	current := make([]string, 0, 1)
	hasMarker := false
	flush := func() {
		if len(current) == 0 {
			return
		}
		item := strings.TrimSpace(strings.Join(current, "\n"))
		if item != "" && !isListPlaceholder(item) {
			items = append(items, item)
		}
		current = current[:0]
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if marker, ok := listItemText(trimmed); ok {
			hasMarker = true
			flush()
			current = append(current, marker)
			continue
		}
		current = append(current, trimmed)
	}
	flush()
	if !hasMarker {
		if isListPlaceholder(text) {
			return nil
		}
		return []string{text}
	}
	return items
}

func isListPlaceholder(item string) bool {
	switch item {
	case "_None recorded._", "_None._", "_Nothing active._", "_Nothing blocked._":
		return true
	default:
		return false
	}
}

func listItemText(line string) (string, bool) {
	if len(line) >= 2 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ' {
		return strings.TrimSpace(line[2:]), true
	}
	index := 0
	for index < len(line) && line[index] >= '0' && line[index] <= '9' {
		index++
	}
	if index > 0 && index+1 < len(line) && (line[index] == '.' || line[index] == ')') && line[index+1] == ' ' {
		return strings.TrimSpace(line[index+2:]), true
	}
	return "", false
}

func writeMarkdownList(out *strings.Builder, items []string) {
	if len(items) == 0 {
		out.WriteString("_None recorded._\n")
		return
	}
	for _, item := range items {
		lines := strings.Split(strings.TrimRight(item, "\n"), "\n")
		out.WriteString("- ")
		out.WriteString(lines[0])
		out.WriteByte('\n')
		for _, line := range lines[1:] {
			out.WriteString("  ")
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
}

// writeSubsection appends content under a level-three heading so it stays inside
// the enclosing level-two field while remaining readable.
func writeSubsection(out *strings.Builder, heading, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	out.WriteString("### ")
	out.WriteString(singleLine(heading))
	out.WriteString("\n\n")
	out.WriteString(content)
	out.WriteString("\n\n")
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

const maxSlugBytes = 60

// slug produces the lowercase dash-separated file-name stem used for generated
// note identifiers. Non-ASCII text becomes a separator; legacy notes carry no
// stored identifiers, so generated identifiers only need to be stable.
func slug(value string) string {
	var out strings.Builder
	lastDash := true
	for _, char := range strings.ToLower(value) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			out.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(out.String(), "-")
	if len(result) > maxSlugBytes {
		result = strings.Trim(result[:maxSlugBytes], "-")
	}
	if result == "" {
		return "entry"
	}
	return result
}

func uniqueName(used map[string]struct{}, base string) string {
	if _, exists := used[base]; !exists {
		used[base] = struct{}{}
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := base + "-" + strconv.Itoa(suffix)
		if _, exists := used[candidate]; !exists {
			used[candidate] = struct{}{}
			return candidate
		}
	}
}

// parseClock reads an "HH:MM" or "HH:MM:SS" note heading.
func parseClock(value string) (int, int, int, bool) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, 0, 0, false
	}
	numbers := make([]int, len(parts))
	for i, part := range parts {
		if part == "" || len(part) > 2 {
			return 0, 0, 0, false
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return 0, 0, 0, false
		}
		numbers[i] = number
	}
	if numbers[0] > 23 || numbers[1] > 59 {
		return 0, 0, 0, false
	}
	seconds := 0
	if len(numbers) == 3 {
		if numbers[2] > 59 {
			return 0, 0, 0, false
		}
		seconds = numbers[2]
	}
	return numbers[0], numbers[1], seconds, true
}

var legacyTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02 15:04:05.999999999 -07:00",
	"2006-01-02 15:04:05 -07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func parseLegacyTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"'`)
	if value == "" || strings.EqualFold(value, "null") {
		return time.Time{}, false
	}
	for _, layout := range legacyTimeLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func firstTime(values []string, fallback time.Time) time.Time {
	for _, value := range values {
		if parsed, ok := parseLegacyTime(value); ok {
			return parsed.UTC()
		}
	}
	return fallback.UTC().Truncate(time.Second)
}

func dateOf(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}
