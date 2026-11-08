package domain

import "strings"

// RecordLink is one [[...]] reference to another record found in record text.
//
// Syntax: [[<id>]] or [[<id>|display text]]. The id may be qualified with a
// project id as "<project>/<id>" (the same form as the search index key);
// project and record ids never contain "/", so the first "/" splits them.
type RecordLink struct {
	// ProjectID is the qualifying project, or "" for a link into the
	// project of the record that contains it.
	ProjectID string
	ID        string
	// Text is the optional display text after "|".
	Text string
}

// Target is the canonical "<project>/<id>" form of the link, resolving an
// unqualified link against the project that contains it.
func (l RecordLink) Target(ownerProjectID string) (projectID, id string) {
	if l.ProjectID == "" {
		return ownerProjectID, l.ID
	}
	return l.ProjectID, l.ID
}

// ParseRecordLinks returns the record links in text in order of appearance.
// Links inside inline code (`...`) and fenced code blocks (``` or ~~~) are
// ignored. Malformed brackets (unclosed, empty, nested "[[", whitespace or
// extra "/" inside the id) are skipped, never an error.
func ParseRecordLinks(text string) []RecordLink {
	var links []RecordLink
	inFence := false
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if marker := fenceMarker(trimmed); marker != "" {
			switch {
			case !inFence:
				inFence, fence = true, marker
			case strings.HasPrefix(trimmed, fence):
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}
		links = append(links, parseLineLinks(line)...)
	}
	return links
}

func fenceMarker(line string) string {
	for _, marker := range []string{"```", "~~~"} {
		if strings.HasPrefix(line, marker) {
			return marker
		}
	}
	return ""
}

func parseLineLinks(line string) []RecordLink {
	var links []RecordLink
	for i := 0; i < len(line); {
		switch {
		case line[i] == '`':
			end := strings.IndexByte(line[i+1:], '`')
			if end < 0 {
				return links
			}
			i += end + 2
		case strings.HasPrefix(line[i:], "[["):
			end := strings.Index(line[i+2:], "]]")
			if end < 0 {
				return links
			}
			inner := line[i+2 : i+2+end]
			if nested := strings.LastIndexByte(inner, '['); nested >= 0 {
				// "[[a [[b]]" or "[[[a]]]": restart at the innermost "[[".
				i += 1 + nested
				continue
			}
			if link, ok := parseLinkBody(inner); ok {
				links = append(links, link)
			}
			i += end + 4
		default:
			i++
		}
	}
	return links
}

func parseLinkBody(inner string) (RecordLink, bool) {
	ref, text, _ := strings.Cut(inner, "|")
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.ContainsAny(ref, " \t[]|`") {
		return RecordLink{}, false
	}
	link := RecordLink{ID: ref, Text: strings.TrimSpace(text)}
	if project, id, qualified := strings.Cut(ref, "/"); qualified {
		if project == "" || id == "" || strings.Contains(id, "/") {
			return RecordLink{}, false
		}
		link.ProjectID, link.ID = project, id
	}
	return link, true
}
