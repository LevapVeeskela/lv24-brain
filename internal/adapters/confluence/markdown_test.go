package confluence

import (
	"encoding/json"
	"os"
	"testing"
	"unicode/utf8"
)

func TestStorageToMarkdownCases(t *testing.T) {
	data, err := os.ReadFile("testdata/storage_cases.json")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	var cases []struct {
		Name     string `json:"name"`
		Storage  string `json:"storage"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("decode testdata: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("testdata is empty")
	}
	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			got := storageToMarkdown(testCase.Storage)
			if got != testCase.Markdown {
				t.Fatalf("storageToMarkdown(%q) =\n%q\nwant\n%q", testCase.Storage, got, testCase.Markdown)
			}
			if !utf8.ValidString(got) {
				t.Fatal("converter produced invalid UTF-8")
			}
		})
	}
}

func TestStorageToMarkdownIsDeterministic(t *testing.T) {
	input := "<h2>Title</h2><p>A <strong>bold</strong> line.</p><ul><li>One</li><li>Two</li></ul>"
	first := storageToMarkdown(input)
	for attempt := 0; attempt < 5; attempt++ {
		if got := storageToMarkdown(input); got != first {
			t.Fatalf("converter is not deterministic:\n%q\n%q", first, got)
		}
	}
}

func TestCapUTF8NeverSplitsRune(t *testing.T) {
	value := "prefix-" + repeatRune('я', 100)
	for limit := 0; limit <= len(value)+2; limit++ {
		got := capUTF8(value, limit)
		if len(got) > limit || !utf8.ValidString(got) {
			t.Fatalf("capUTF8(%d) produced %d bytes valid=%v", limit, len(got), utf8.ValidString(got))
		}
	}
}

func repeatRune(value rune, count int) string {
	out := make([]rune, count)
	for index := range out {
		out[index] = value
	}
	return string(out)
}
