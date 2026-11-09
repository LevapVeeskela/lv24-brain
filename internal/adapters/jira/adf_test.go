package jira

import (
	"strings"
	"testing"
)

func TestADFToMarkdown(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "paragraph and heading",
			input: `{"type":"doc","content":[{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Heading"}]},{"type":"paragraph","content":[{"type":"text","text":"hello"},{"type":"hardBreak"},{"type":"text","text":"world"}]}]}`,
			want:  "## Heading\n\nhello\nworld",
		},
		{
			name:  "lists and inline marks",
			input: `{"type":"doc","content":[{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"bold","marks":[{"type":"strong"}]}]}]}]},{"type":"orderedList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item"}]}]}]}]}`,
			want:  "- **bold**\n\n1. item",
		},
		{
			name:  "code link mention",
			input: `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"x()","marks":[{"type":"code"}]},{"type":"text","text":"site","marks":[{"type":"link","attrs":{"href":"https://example.test"}}]},{"type":"mention","attrs":{"text":"@Ada"}}]}]}`,
			want:  "`x()`[site](https://example.test)@Ada",
		},
		{
			name:  "code block",
			input: `{"type":"doc","content":[{"type":"codeBlock","content":[{"type":"text","text":"go test"}]}]}`,
			want:  "````\ngo test\n````",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var text jiraText
			if err := text.UnmarshalJSON([]byte(test.input)); err != nil {
				t.Fatal(err)
			}
			got := text.markdown()
			if got != test.want {
				t.Fatalf("markdown = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDataCenterWikiTextStaysText(t *testing.T) {
	var text jiraText
	if err := text.UnmarshalJSON([]byte(`"h2. heading\\n[wiki link]"`)); err != nil {
		t.Fatal(err)
	}
	if got := text.markdown(); got != "h2. heading\\n[wiki link]" || strings.Contains(got, "# heading") {
		t.Fatalf("wiki text was transformed: %q", got)
	}
}
