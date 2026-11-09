package jira

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// jiraText accepts either Data Center's plain string/wiki text or Cloud's
// Atlassian Document Format object. The raw DC representation is kept as text.
type jiraText struct {
	plain string
	doc   *adfNode
}

func (t *jiraText) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		t.plain, t.doc = "", nil
		return nil
	}
	if data[0] == '"' {
		return json.Unmarshal(data, &t.plain)
	}
	var doc adfNode
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("decode Jira document: %w", err)
	}
	t.doc = &doc
	return nil
}

func (t jiraText) markdown() string {
	if t.doc == nil {
		return t.plain
	}
	return strings.TrimSpace(renderADF(*t.doc, 0))
}

type adfNode struct {
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Attrs   map[string]any `json:"attrs"`
	Content []adfNode      `json:"content"`
	Marks   []adfMark      `json:"marks"`
}

type adfMark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs"`
}

func renderADF(node adfNode, depth int) string {
	var out strings.Builder
	switch node.Type {
	case "text":
		out.WriteString(renderTextMarks(node.Text, node.Marks))
	case "hardBreak":
		out.WriteByte('\n')
	case "mention":
		label, _ := node.Attrs["text"].(string)
		if label == "" {
			label, _ = node.Attrs["displayName"].(string)
		}
		if label == "" {
			label, _ = node.Attrs["id"].(string)
		}
		if label != "" && !strings.HasPrefix(label, "@") {
			label = "@" + label
		}
		out.WriteString(label)
	case "paragraph":
		out.WriteString(renderADFChildren(node.Content, depth))
		out.WriteString("\n\n")
	case "heading":
		level := 1
		if n, ok := number(node.Attrs["level"]); ok && n >= 1 && n <= 6 {
			level = n
		}
		out.WriteString(strings.Repeat("#", level) + " ")
		out.WriteString(renderADFChildren(node.Content, depth))
		out.WriteString("\n\n")
	case "codeBlock":
		out.WriteString("\n````\n")
		out.WriteString(renderADFChildren(node.Content, depth))
		if !strings.HasSuffix(out.String(), "\n") {
			out.WriteByte('\n')
		}
		out.WriteString("````\n\n")
	case "bulletList", "orderedList":
		for index, child := range node.Content {
			if child.Type != "listItem" {
				continue
			}
			marker := "- "
			if node.Type == "orderedList" {
				marker = fmt.Sprintf("%d. ", index+1)
			}
			content := strings.TrimSpace(renderADFChildren(child.Content, depth+1))
			indent := strings.Repeat("  ", depth)
			lines := strings.Split(content, "\n")
			out.WriteString(indent + marker + strings.Join(lines, "\n"+indent+"  ") + "\n")
		}
		out.WriteByte('\n')
	case "listItem":
		out.WriteString(renderADFChildren(node.Content, depth))
	case "doc", "blockquote", "panel", "taskList", "taskItem", "table", "tableRow", "tableCell", "tableHeader":
		out.WriteString(renderADFChildren(node.Content, depth))
	default:
		// Unknown nodes contribute their text/content deterministically rather
		// than silently discarding human-authored content.
		if node.Text != "" {
			out.WriteString(renderTextMarks(node.Text, node.Marks))
		}
		out.WriteString(renderADFChildren(node.Content, depth))
	}
	return out.String()
}

func renderADFChildren(nodes []adfNode, depth int) string {
	var out strings.Builder
	for _, child := range nodes {
		out.WriteString(renderADF(child, depth))
	}
	return out.String()
}

func renderTextMarks(value string, marks []adfMark) string {
	for _, mark := range marks {
		switch mark.Type {
		case "code":
			value = "`" + strings.ReplaceAll(value, "`", "\\`") + "`"
		case "strong":
			value = "**" + value + "**"
		case "em":
			value = "*" + value + "*"
		case "strike":
			value = "~~" + value + "~~"
		case "link":
			href, _ := mark.Attrs["href"].(string)
			if href != "" {
				value = "[" + value + "](" + href + ")"
			}
		}
	}
	return value
}

func number(value any) (int, bool) {
	switch n := value.(type) {
	case float64:
		return int(n), true
	case json.Number:
		v, err := n.Int64()
		return int(v), err == nil
	default:
		return 0, false
	}
}
