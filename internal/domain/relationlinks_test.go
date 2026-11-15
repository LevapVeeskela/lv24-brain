package domain_test

import (
	"strings"
	"testing"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

func TestRewriteRecordLinksReplacesTargets(t *testing.T) {
	replacements := map[string]string{"decision-old": "decision-new"}
	cases := []struct {
		name string
		text string
		want string
	}{
		{"bare", "see [[decision-old]] here", "see [[decision-new]] here"},
		{"display text", "see [[decision-old|the old plan]] here", "see [[decision-new|the old plan]] here"},
		{"qualified same project", "see [[demo/decision-old]]", "see [[demo/decision-new]]"},
		{"other project untouched", "see [[other/decision-old]]", "see [[other/decision-old]]"},
		{"other id untouched", "see [[decision-other]]", "see [[decision-other]]"},
		{"inline code untouched", "`[[decision-old]]` and [[decision-old]]", "`[[decision-old]]` and [[decision-new]]"},
		{"fenced block untouched", "```\n[[decision-old]]\n```\n[[decision-old]]", "```\n[[decision-old]]\n```\n[[decision-new]]"},
		{"multiple links", "[[decision-old]] and [[decision-old|x]]", "[[decision-new]] and [[decision-new|x]]"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, changed := domain.RewriteRecordLinks(test.text, "demo", replacements)
			if got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
			if changed != (test.text != test.want) {
				t.Fatalf("changed = %v", changed)
			}
		})
	}
}

func TestRewriteRecordLinksNoChange(t *testing.T) {
	if got, changed := domain.RewriteRecordLinks("plain text", "demo", map[string]string{"a": "b"}); changed || got != "plain text" {
		t.Fatalf("got %q, changed %v", got, changed)
	}
	if got, changed := domain.RewriteRecordLinks("[[decision-old]]", "demo", map[string]string{"decision-old": "decision-old"}); changed || got != "[[decision-old]]" {
		t.Fatalf("self replacement changed the text: %q", got)
	}
	if got, changed := domain.RewriteRecordLinks("[[decision-old]]", "demo", nil); changed || got != "[[decision-old]]" {
		t.Fatalf("empty replacements changed the text: %q", got)
	}
}

func TestRewriteRecordLinksKeepsLineBreaksAndTrailingText(t *testing.T) {
	text := "intro\n[[decision-old]] tail\n\nend"
	got, changed := domain.RewriteRecordLinks(text, "demo", map[string]string{"decision-old": "decision-new"})
	if !changed {
		t.Fatal("link not rewritten")
	}
	if got != "intro\n[[decision-new]] tail\n\nend" {
		t.Fatalf("got %q", got)
	}
	if lines := strings.Count(got, "\n"); lines != strings.Count(text, "\n") {
		t.Fatalf("line count changed: %d", lines)
	}
}
