package mergedriver

import (
	"fmt"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

// stateSection is one level-2 section of state.md.
type stateSection struct {
	name string
	body string
}

type stateDocument struct {
	frontmatter map[string]any
	preamble    string
	sections    []stateSection
}

type stateFields struct {
	Goal          string
	Active        []string
	Blocked       []string
	NextAction    string
	OpenQuestions []string
}

var stateFieldNames = []string{"Goal", "Active", "Blocked", "Next action", "Open questions"}

func parseState(data []byte) (stateDocument, error) {
	doc := stateDocument{frontmatter: map[string]any{}}
	if len(strings.TrimSpace(string(data))) == 0 {
		return doc, nil
	}
	body, err := markdown.Parse(data, &doc.frontmatter)
	if err != nil {
		return stateDocument{}, err
	}
	lines := strings.Split(body, "\n")
	var preamble []string
	index := 0
	for ; index < len(lines); index++ {
		if isSectionHeading(lines[index]) {
			break
		}
		preamble = append(preamble, lines[index])
	}
	doc.preamble = strings.TrimRight(strings.Join(preamble, "\n"), "\n")
	for index < len(lines) {
		if !isSectionHeading(lines[index]) {
			index++
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(lines[index], "##"))
		index++
		var content []string
		for index < len(lines) && !isSectionHeading(lines[index]) {
			content = append(content, lines[index])
			index++
		}
		doc.sections = append(doc.sections, stateSection{
			name: name,
			body: strings.Trim(strings.Join(content, "\n"), "\n"),
		})
	}
	return doc, nil
}

func isSectionHeading(line string) bool {
	return strings.HasPrefix(strings.TrimRight(line, " \t"), "## ")
}

func sectionByName(doc stateDocument, name string) (stateSection, bool) {
	for _, section := range doc.sections {
		if strings.EqualFold(section.name, name) {
			return section, true
		}
	}
	return stateSection{}, false
}

func stateFieldsOf(doc stateDocument) stateFields {
	var fields stateFields
	if section, ok := sectionByName(doc, "Goal"); ok {
		fields.Goal = strings.TrimSpace(section.body)
	}
	if section, ok := sectionByName(doc, "Active"); ok {
		fields.Active = parseStateItems(section.body)
	}
	if section, ok := sectionByName(doc, "Blocked"); ok {
		fields.Blocked = parseStateItems(section.body)
	}
	if section, ok := sectionByName(doc, "Next action"); ok {
		fields.NextAction = strings.TrimSpace(section.body)
	}
	if section, ok := sectionByName(doc, "Open questions"); ok {
		fields.OpenQuestions = parseStateItems(section.body)
	}
	return fields
}

// parseStateItems reads the "- item" list of a state section. Continuation
// lines are indented by two spaces. The "_Nothing ..._" fallbacks mean empty.
func parseStateItems(body string) []string {
	items := make([]string, 0)
	var current []string
	flush := func() {
		if len(current) == 0 {
			return
		}
		text := strings.TrimSpace(strings.Join(current, "\n"))
		if text != "" && !isStateFallback(text) {
			items = append(items, text)
		}
		current = nil
	}
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "- "):
			flush()
			current = []string{strings.TrimPrefix(line, "- ")}
		case strings.HasPrefix(line, "  ") && len(current) > 0:
			current = append(current, strings.TrimPrefix(line, "  "))
		case strings.TrimSpace(line) == "":
			continue
		case len(current) > 0:
			current = append(current, line)
		}
	}
	flush()
	return items
}

func isStateFallback(text string) bool {
	switch strings.TrimSpace(text) {
	case "_Nothing active._", "_Nothing blocked._", "_None._":
		return true
	}
	return false
}

func mergeScalar(field, base, ours, theirs string, opts Options, conflicts []Conflict) (string, []Conflict) {
	switch {
	case ours == theirs:
		return ours, conflicts
	case ours == base:
		return theirs, conflicts
	case theirs == base:
		return ours, conflicts
	default:
		conflicts = append(conflicts, Conflict{
			Kind:   "state",
			Field:  field,
			Base:   base,
			Ours:   ours,
			Theirs: theirs,
			Path:   opts.Path,
		})
		return ours, conflicts
	}
}

// mergeItems merges an ordered list as a set: additions from both sides are
// kept in ours' order, and a removal wins over an unchanged copy. An item that
// both sides keep stays once.
func mergeItems(base, ours, theirs []string) []string {
	if equalItems(ours, theirs) {
		return ours
	}
	if equalItems(ours, base) {
		return theirs
	}
	if equalItems(theirs, base) {
		return ours
	}
	baseSet := itemSet(base)
	theirsSet := itemSet(theirs)
	oursSet := itemSet(ours)

	result := make([]string, 0, len(ours)+len(theirs))
	add := func(item string) {
		for _, existing := range result {
			if existing == item {
				return
			}
		}
		result = append(result, item)
	}
	for _, item := range ours {
		if _, inTheirs := theirsSet[item]; inTheirs {
			add(item)
			continue
		}
		if _, inBase := baseSet[item]; !inBase {
			// Added on our side only.
			add(item)
		}
		// In base but gone from theirs: theirs removed it.
	}
	for _, item := range theirs {
		if _, inOurs := oursSet[item]; inOurs {
			add(item)
			continue
		}
		if _, inBase := baseSet[item]; !inBase {
			// Added on their side only.
			add(item)
		}
	}
	return result
}

func equalItems(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func itemSet(items []string) map[string]struct{} {
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	return set
}

func mergeExtraSections(base, ours, theirs stateDocument, opts Options) ([]stateSection, []Conflict) {
	known := make(map[string]struct{}, len(stateFieldNames))
	for _, name := range stateFieldNames {
		known[strings.ToLower(name)] = struct{}{}
	}
	extraFrom := func(doc stateDocument) []stateSection {
		var out []stateSection
		for _, section := range doc.sections {
			if _, ok := known[strings.ToLower(section.name)]; ok {
				continue
			}
			out = append(out, section)
		}
		return out
	}
	oursExtras := extraFrom(ours)
	theirsExtras := extraFrom(theirs)
	baseExtras := extraFrom(base)

	baseByName := map[string]stateSection{}
	for _, section := range baseExtras {
		baseByName[strings.ToLower(section.name)] = section
	}
	theirsByName := map[string]stateSection{}
	for _, section := range theirsExtras {
		theirsByName[strings.ToLower(section.name)] = section
	}
	seen := map[string]struct{}{}
	result := make([]stateSection, 0, len(oursExtras)+len(theirsExtras))
	conflicts := make([]Conflict, 0)

	for _, section := range oursExtras {
		key := strings.ToLower(section.name)
		seen[key] = struct{}{}
		baseSection, inBase := baseByName[key]
		theirsSection, inTheirs := theirsByName[key]
		switch {
		case inTheirs && section.body == theirsSection.body:
		case inTheirs && inBase && theirsSection.body == baseSection.body:
			// Incoming side is unchanged; ours wins.
		case inTheirs && inBase && section.body == baseSection.body:
			result = append(result, theirsSection)
		case inTheirs:
			conflicts = append(conflicts, Conflict{
				Kind:   "state",
				Field:  slug(section.name),
				Base:   baseSection.body,
				Ours:   section.body,
				Theirs: theirsSection.body,
				Path:   opts.Path,
			})
			result = append(result, section)
		case inBase && section.body == baseSection.body:
			// Removed on the incoming side; the removal wins.
		default:
			result = append(result, section)
		}
	}
	for _, section := range theirsExtras {
		key := strings.ToLower(section.name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		baseSection, inBase := baseByName[key]
		if inBase && section.body == baseSection.body {
			// Removed on our side; the removal wins.
			continue
		}
		result = append(result, section)
	}
	return result, conflicts
}

func renderState(preamble string, fields stateFields, extras []stateSection) (string, error) {
	var out strings.Builder
	if text := strings.TrimSpace(preamble); text != "" {
		out.WriteString(text)
		out.WriteString("\n\n")
	}
	writeStateSection(&out, "Goal", strings.TrimSpace(fields.Goal))
	writeStateSection(&out, "Active", renderStateItems(fields.Active, "_Nothing active._"))
	writeStateSection(&out, "Blocked", renderStateItems(fields.Blocked, "_Nothing blocked._"))
	writeStateSection(&out, "Next action", strings.TrimSpace(fields.NextAction))
	writeStateSection(&out, "Open questions", renderStateItems(fields.OpenQuestions, "_None._"))
	for _, section := range extras {
		writeStateSection(&out, section.name, section.body)
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func writeStateSection(out *strings.Builder, name, body string) {
	fmt.Fprintf(out, "## %s\n\n", name)
	if body != "" {
		out.WriteString(body)
		out.WriteString("\n")
	}
	out.WriteString("\n")
}

func renderStateItems(items []string, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	var out strings.Builder
	for _, item := range items {
		lines := strings.Split(strings.TrimRight(item, "\n"), "\n")
		fmt.Fprintf(&out, "- %s\n", lines[0])
		for _, line := range lines[1:] {
			fmt.Fprintf(&out, "  %s\n", line)
		}
	}
	return strings.TrimRight(out.String(), "\n")
}
