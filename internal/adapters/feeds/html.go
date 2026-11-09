package feeds

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Minimal stdlib-only HTML to text conversion for item bodies: item bodies
// are plain Markdown, and feed/page HTML is reduced to readable text without
// pulling in golang.org/x/net/html (not present in go.mod, and the package
// adds no dependencies).

var (
	// htmlTitleRe extracts the document title before tags are stripped.
	htmlTitleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	// Noise blocks are removed before tag stripping so their content never
	// leaks into the body. RE2 has no backreferences, hence one pattern per
	// element.
	htmlNoiseRes = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<head\b[^>]*>.*?</head\s*>`),
		regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`),
		regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style\s*>`),
		regexp.MustCompile(`(?is)<noscript\b[^>]*>.*?</noscript\s*>`),
		regexp.MustCompile(`(?is)<svg\b[^>]*>.*?</svg\s*>`),
		regexp.MustCompile(`(?is)<iframe\b[^>]*>.*?</iframe\s*>`),
		regexp.MustCompile(`(?is)<object\b[^>]*>.*?</object\s*>`),
		regexp.MustCompile(`(?is)<template\b[^>]*>.*?</template\s*>`),
	}
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->|<!\[CDATA\[.*?\]\]>|<!DOCTYPE[^>]*>|<![^>]*>`)
	// htmlTagRe matches a complete tag including quoted attribute values;
	// an unquoted `>` inside such a value cannot end the match.
	htmlTagRe = regexp.MustCompile(`(?is)</?[a-zA-Z][a-zA-Z0-9-]*(?:[^>"']|"[^"]*"|'[^']*')*>`)
	// htmlBlockRe marks tags that produce a line break in the text form.
	htmlBlockRe = regexp.MustCompile(`(?i)^</?(?:p|div|li|ul|ol|br|tr|table|thead|tbody|tfoot|h[1-6]|blockquote|pre|section|article|header|footer|figure|figcaption|dl|dt|dd|hr)\b`)
	// htmlEntityRe iterates character references (with or without the
	// trailing semicolon for the numeric forms).
	htmlEntityRe = regexp.MustCompile(`&(?:[A-Za-z][A-Za-z0-9]{1,31};|#[Xx][0-9A-Fa-f]{1,6};|#[0-9]{1,7};)`)
)

// namedEntities covers the entities that matter for readable text; unknown
// named references are dropped rather than guessed.
var namedEntities = map[string]string{
	"amp":    "&",
	"lt":     "<",
	"gt":     ">",
	"quot":   `"`,
	"apos":   "'",
	"mdash":  "—",
	"ndash":  "–",
	"hellip": "…",
	"copy":   "©",
	"reg":    "®",
	"nbsp":   " ",
}

// decodeEntities resolves character references and drops everything the
// renderer cannot represent as plain text.
func decodeEntities(value string) string {
	if !strings.ContainsRune(value, '&') {
		return value
	}
	var out strings.Builder
	last := 0
	for _, match := range htmlEntityRe.FindAllStringIndex(value, -1) {
		out.WriteString(value[last:match[0]])
		last = match[1]
		out.WriteString(entityText(value[match[0]+1 : match[1]-1]))
	}
	out.WriteString(value[last:])
	return out.String()
}

// entityText resolves one entity reference body (between & and ;).
func entityText(name string) string {
	if strings.HasPrefix(name, "#") {
		var code int64
		var err error
		if len(name) > 2 && (name[1] == 'x' || name[1] == 'X') {
			code, err = strconv.ParseInt(name[2:], 16, 32)
		} else {
			code, err = strconv.ParseInt(name[1:], 10, 32)
		}
		if err != nil || code <= 0 || code > int64(utf8.MaxRune) {
			return ""
		}
		r := rune(code)
		if isControlRune(r) {
			return ""
		}
		return string(r)
	}
	return namedEntities[strings.ToLower(name)]
}

// isControlRune reports whether r is a control character; newlines are
// handled by the block-tag logic and whitespace normalization.
func isControlRune(r rune) bool {
	return r < ' ' || r == 127
}

// stripHTMLTags removes every tag, leaving the text segments joined with the
// separators in between.
func stripHTMLTags(value string) string {
	var out strings.Builder
	last := 0
	for _, match := range htmlTagRe.FindAllStringIndex(value, -1) {
		out.WriteString(value[last:match[0]])
		last = match[1]
	}
	out.WriteString(value[last:])
	return out.String()
}

// htmlToText reduces one HTML fragment or document to readable text: noise
// blocks and comments are dropped, block tags become line breaks, remaining
// tags are stripped, entities are decoded and whitespace is normalized per
// line, so similar HTML normalizes to the same text.
func htmlToText(value string) string {
	result := value
	for _, noise := range htmlNoiseRes {
		result = noise.ReplaceAllString(result, " ")
	}
	result = htmlCommentRe.ReplaceAllString(result, " ")
	var out strings.Builder
	last := 0
	for _, match := range htmlTagRe.FindAllStringIndex(result, -1) {
		textChunk(&out, result[last:match[0]])
		if htmlBlockRe.MatchString(result[match[0]:match[1]]) {
			out.WriteByte('\n')
		}
		last = match[1]
	}
	textChunk(&out, result[last:])
	return normalizeText(out.String())
}

// textChunk appends one tagless text segment with decoded entities: control
// characters are dropped, other whitespace is preserved for the newline
// logic.
func textChunk(out *strings.Builder, raw string) {
	for _, r := range decodeEntities(raw) {
		switch {
		case r == 0, isControlRune(r):
		default:
			out.WriteRune(r)
		}
	}
}

// normalizeText trims every line, spaces inside lines collapse to one space,
// and leading/trailing blank lines are dropped. Line endings become LF.
func normalizeText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		lines[index] = strings.Join(strings.Fields(line), " ")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// htmlTitle extracts the <title> of an HTML document as plain text.
func htmlTitle(value string) string {
	match := htmlTitleRe.FindStringSubmatch(value)
	if match == nil {
		return ""
	}
	title := decodeEntities(htmlTagRe.ReplaceAllString(match[1], " "))
	return strings.TrimSpace(normalizeText(title))
}

// rssTitle flattens an RSS/Atom title or body fragment to spaced plain text.
func rssTitle(value string) string {
	return strings.Join(strings.Fields(decodeEntities(stripHTMLTags(value))), " ")
}
