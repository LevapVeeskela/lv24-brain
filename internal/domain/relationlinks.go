package domain

import "strings"

// linkSpan is one [[...]] occurrence with its byte range in the line it was
// found in. end is exclusive and points just after "]]".
type linkSpan struct {
	start int
	end   int
	link  RecordLink
}

// RewriteRecordLinks replaces the target of every record link whose id is in
// replacements. An unqualified link and a link qualified with projectID are
// rewritten; a link into another project is left alone, because two projects
// may hold records with the same id.
//
// Display text, the project qualifier and every other byte of the text are
// preserved. Links inside inline code and fenced code blocks are ignored, the
// same rule ParseRecordLinks uses. The second result reports whether anything
// changed.
func RewriteRecordLinks(text, projectID string, replacements map[string]string) (string, bool) {
	if len(replacements) == 0 || !strings.Contains(text, "[[") {
		return text, false
	}
	var out strings.Builder
	out.Grow(len(text))
	changed := false
	inFence := false
	fence := ""
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if index > 0 {
			out.WriteByte('\n')
		}
		trimmed := strings.TrimSpace(line)
		if marker := fenceMarker(trimmed); marker != "" {
			switch {
			case !inFence:
				inFence, fence = true, marker
			case strings.HasPrefix(trimmed, fence):
				inFence = false
			}
			out.WriteString(line)
			continue
		}
		if inFence {
			out.WriteString(line)
			continue
		}
		rewritten, lineChanged := rewriteLineLinks(line, projectID, replacements)
		changed = changed || lineChanged
		out.WriteString(rewritten)
	}
	if !changed {
		return text, false
	}
	return out.String(), true
}

// rewriteLineLinks rebuilds one line, replacing the targets of the links it
// carries. The scanner mirrors parseLineLinks so both agree on what a link is.
func rewriteLineLinks(line, projectID string, replacements map[string]string) (string, bool) {
	spans := scanLineLinks(line)
	if len(spans) == 0 {
		return line, false
	}
	var out strings.Builder
	out.Grow(len(line))
	cursor := 0
	changed := false
	for _, span := range spans {
		replacement, ok := linkReplacement(span.link, projectID, replacements)
		if !ok {
			continue
		}
		out.WriteString(line[cursor:span.start])
		out.WriteString(replacement)
		cursor = span.end
		changed = true
	}
	if !changed {
		return line, false
	}
	out.WriteString(line[cursor:])
	return out.String(), true
}

// linkReplacement builds the rewritten bracketed link for one span.
func linkReplacement(link RecordLink, projectID string, replacements map[string]string) (string, bool) {
	if link.ProjectID != "" && link.ProjectID != projectID {
		return "", false
	}
	replacement, ok := replacements[link.ID]
	if !ok || replacement == "" || replacement == link.ID {
		return "", false
	}
	ref := replacement
	if link.ProjectID != "" {
		ref = link.ProjectID + "/" + replacement
	}
	rewritten := "[[" + ref
	if link.Text != "" {
		rewritten += "|" + link.Text
	}
	rewritten += "]]"
	return rewritten, true
}

// scanLineLinks returns the record links of one line with their byte ranges.
// Inline code is skipped; a malformed bracket sequence stops the scan, exactly
// like parseLineLinks.
func scanLineLinks(line string) []linkSpan {
	var spans []linkSpan
	for i := 0; i < len(line); {
		switch {
		case line[i] == '`':
			end := strings.IndexByte(line[i+1:], '`')
			if end < 0 {
				return spans
			}
			i += end + 2
		case strings.HasPrefix(line[i:], "[["):
			end := strings.Index(line[i+2:], "]]")
			if end < 0 {
				return spans
			}
			inner := line[i+2 : i+2+end]
			if nested := strings.LastIndexByte(inner, '['); nested >= 0 {
				i += 1 + nested
				continue
			}
			if link, ok := parseLinkBody(inner); ok {
				spans = append(spans, linkSpan{start: i, end: i + end + 4, link: link})
			}
			i += end + 4
		default:
			i++
		}
	}
	return spans
}
