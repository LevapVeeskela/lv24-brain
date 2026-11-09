package confluence

import (
	"fmt"
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// storageToMarkdown is a small, deterministic converter for Confluence storage
// (XHTML) bodies. It intentionally handles only the common block and inline
// structures: headings, paragraphs, lists, pre/code, links, bold/italic,
// blockquotes and simple tables. Unknown tags are stripped; their text is
// kept, except for script and style content, which is dropped.
func storageToMarkdown(input string) string {
	converter := &mdConverter{}
	converter.convert(input)
	return converter.finish()
}

type mdConverter struct {
	out      strings.Builder
	last     byte
	lists    []listLevel
	link     string
	inLink   bool
	inPre    bool
	pre      strings.Builder
	inCode   bool
	skipping string
	table    tableState
}

type listLevel struct {
	ordered bool
	counter int
}

type tableState struct {
	rows  int
	cells int
	open  bool
}

func (c *mdConverter) convert(input string) {
	for index := 0; index < len(input); {
		token, next := scanMarkup(input, index)
		index = next
		if token == "" {
			continue
		}
		if token[0] != '<' {
			c.text(token)
			continue
		}
		c.tag(token)
	}
}

func (c *mdConverter) write(value string) {
	if value == "" {
		return
	}
	c.out.WriteString(value)
	c.last = value[len(value)-1]
}

func (c *mdConverter) text(value string) {
	if c.skipping != "" {
		return
	}
	if c.inPre {
		c.pre.WriteString(html.UnescapeString(value))
		return
	}
	c.inline(html.UnescapeString(value))
}

// inline collapses HTML whitespace but preserves one leading/trailing space so
// "hello <strong>world</strong>" does not become "hello**world**".
func (c *mdConverter) inline(value string) {
	if value == "" {
		return
	}
	firstRune, _ := utf8.DecodeRuneInString(value)
	lastRune, _ := utf8.DecodeLastRuneInString(value)
	leading := unicode.IsSpace(firstRune)
	trailing := unicode.IsSpace(lastRune)
	core := strings.Join(strings.Fields(value), " ")
	if core == "" {
		if (leading || trailing) && c.last != 0 && c.last != '\n' && c.last != ' ' {
			c.write(" ")
		}
		return
	}
	if leading && c.last != 0 && c.last != '\n' && c.last != ' ' {
		core = " " + core
	}
	if trailing {
		core += " "
	}
	c.write(core)
}

func (c *mdConverter) tag(token string) {
	name, attrs, closing, selfClosing, ok := parseTag(token)
	if !ok {
		return
	}
	if c.skipping != "" {
		if closing && name == c.skipping {
			c.skipping = ""
		}
		return
	}
	if c.inPre {
		switch {
		case name == "pre" && closing:
			c.closePre()
		case name == "br":
			c.pre.WriteString("\n")
		}
		return
	}
	if closing {
		switch name {
		case "p", "div", "section", "article":
			c.write("\n\n")
		case "h1", "h2", "h3", "h4", "h5", "h6":
			c.write("\n\n")
		case "li":
			c.write("\n")
		case "ul", "ol":
			if len(c.lists) > 0 {
				c.lists = c.lists[:len(c.lists)-1]
			}
			c.write("\n")
		case "blockquote":
			c.write("\n")
		case "a":
			if c.inLink {
				c.write("](" + c.link + ")")
				c.inLink = false
				c.link = ""
			}
		case "code":
			if c.inCode {
				c.write("`")
				c.inCode = false
			}
		case "strong", "b":
			c.write("**")
		case "em", "i":
			c.write("*")
		case "del", "s", "strike":
			c.write("~~")
		case "tr":
			c.closeTableRow()
		case "table":
			c.write("\n")
			c.table = tableState{}
		}
		return
	}
	switch name {
	case "script", "style":
		if !selfClosing {
			c.skipping = name
		}
	case "pre":
		c.inPre = true
		c.pre.Reset()
	case "br":
		c.write("\n")
	case "hr":
		c.write("\n\n---\n\n")
	case "p":
		c.write("\n\n")
	case "div", "section", "article":
		c.write("\n")
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(name[1] - '0')
		c.write("\n\n" + strings.Repeat("#", level) + " ")
	case "ul":
		if c.last != 0 && c.last != '\n' {
			c.write("\n")
		}
		c.lists = append(c.lists, listLevel{})
	case "ol":
		if c.last != 0 && c.last != '\n' {
			c.write("\n")
		}
		c.lists = append(c.lists, listLevel{ordered: true})
	case "li":
		c.openListItem()
	case "blockquote":
		c.write("\n> ")
	case "a":
		href := strings.TrimSpace(attrValue(attrs, "href"))
		if href != "" {
			c.link = html.UnescapeString(href)
			c.inLink = true
			c.write("[")
		}
	case "img":
		src := strings.TrimSpace(attrValue(attrs, "src"))
		alt := strings.TrimSpace(attrValue(attrs, "alt"))
		if src != "" {
			c.write("![" + alt + "](" + html.UnescapeString(src) + ")")
		}
	case "code":
		c.write("`")
		c.inCode = true
	case "strong", "b":
		c.write("**")
	case "em", "i":
		c.write("*")
	case "del", "s", "strike":
		c.write("~~")
	case "table":
		c.write("\n\n")
		c.table = tableState{}
	case "tr":
		c.openTableRow()
	case "td", "th":
		c.openTableCell()
	}
}

func (c *mdConverter) openListItem() {
	if c.last != 0 && c.last != '\n' {
		c.write("\n")
	}
	depth := len(c.lists) - 1
	if depth < 0 {
		depth = 0
	}
	marker := "- "
	if len(c.lists) > 0 && c.lists[len(c.lists)-1].ordered {
		c.lists[len(c.lists)-1].counter++
		marker = fmt.Sprintf("%d. ", c.lists[len(c.lists)-1].counter)
	}
	c.write(strings.Repeat("  ", depth) + marker)
}

func (c *mdConverter) openTableRow() {
	if c.table.open {
		c.closeTableRow()
	}
	c.write("| ")
	c.table.open = true
	c.table.cells = 0
}

func (c *mdConverter) openTableCell() {
	if c.table.cells > 0 {
		c.write(" | ")
	}
	c.table.cells++
}

func (c *mdConverter) closeTableRow() {
	if !c.table.open {
		return
	}
	c.write(" |")
	if c.table.rows == 0 {
		c.write("\n|")
		for index := 0; index < c.table.cells; index++ {
			c.write(" --- |")
		}
	}
	c.write("\n")
	c.table.rows++
	c.table.open = false
}

func (c *mdConverter) closePre() {
	c.inPre = false
	body := strings.TrimRight(c.pre.String(), "\n")
	c.write("\n\n```\n" + body + "\n```\n\n")
	c.pre.Reset()
}

func (c *mdConverter) finish() string {
	raw := strings.ReplaceAll(c.out.String(), "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")
	for index := range lines {
		lines[index] = strings.TrimRight(lines[index], " \t")
	}
	var out strings.Builder
	blank := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String())
}

// scanMarkup returns the next text run or tag token and the following index.
// A '>' inside a quoted attribute does not end a tag.
func scanMarkup(input string, start int) (string, int) {
	if start >= len(input) {
		return "", len(input)
	}
	if input[start] != '<' {
		if offset := strings.IndexByte(input[start:], '<'); offset >= 0 {
			return input[start : start+offset], start + offset
		}
		return input[start:], len(input)
	}
	if strings.HasPrefix(input[start:], "<!--") {
		if offset := strings.Index(input[start+4:], "-->"); offset >= 0 {
			end := start + 4 + offset + 3
			return input[start:end], end
		}
		return input[start:], len(input)
	}
	var quote byte
	for index := start + 1; index < len(input); index++ {
		ch := input[index]
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '>':
			return input[start : index+1], index + 1
		}
	}
	return input[start:], len(input)
}

func parseTag(token string) (name, attrs string, closing, selfClosing, ok bool) {
	if len(token) < 3 || token[0] != '<' || token[len(token)-1] != '>' {
		return "", "", false, false, false
	}
	body := strings.TrimSpace(token[1 : len(token)-1])
	if body == "" || strings.HasPrefix(body, "!") || strings.HasPrefix(body, "?") {
		return "", "", false, false, false
	}
	if strings.HasPrefix(body, "/") {
		closing = true
		body = strings.TrimSpace(body[1:])
	}
	if strings.HasSuffix(body, "/") {
		selfClosing = true
		body = strings.TrimSpace(body[:len(body)-1])
	}
	if index := strings.IndexFunc(body, unicode.IsSpace); index >= 0 {
		return strings.ToLower(body[:index]), body[index:], closing, selfClosing, true
	}
	return strings.ToLower(body), "", closing, selfClosing, true
}

var attributePattern = regexp.MustCompile(`(?i)([a-zA-Z_:][-a-zA-Z0-9_:.]*)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)

func attrValue(attrs, name string) string {
	for _, match := range attributePattern.FindAllStringSubmatch(attrs, -1) {
		if !strings.EqualFold(match[1], name) {
			continue
		}
		switch {
		case match[2] != "":
			return match[2]
		case match[3] != "":
			return match[3]
		default:
			return match[4]
		}
	}
	return ""
}
