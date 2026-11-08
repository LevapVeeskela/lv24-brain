package domain

import (
	"reflect"
	"testing"
)

func TestParseRecordLinks(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []RecordLink
	}{
		{"none", "plain text [x] [ [y] ]", nil},
		{"plain", "see [[decision-a-1]] now", []RecordLink{{ID: "decision-a-1"}}},
		{"display text", "per [[note-a-2|the release note]].", []RecordLink{{ID: "note-a-2", Text: "the release note"}}},
		{"qualified", "[[beta/attempt-b-3]]", []RecordLink{{ProjectID: "beta", ID: "attempt-b-3"}}},
		{"qualified with text", "[[beta/attempt-b-3|why]]", []RecordLink{{ProjectID: "beta", ID: "attempt-b-3", Text: "why"}}},
		{"multiple per line", "[[a]] and [[b|B]] and [[c]]", []RecordLink{{ID: "a"}, {ID: "b", Text: "B"}, {ID: "c"}}},
		{"multiple lines", "[[a]]\nnext [[b]]", []RecordLink{{ID: "a"}, {ID: "b"}}},
		{"trimmed", "[[ a ]]", []RecordLink{{ID: "a"}}},
		{"inline code ignored", "use `[[a]]` but [[b]]", []RecordLink{{ID: "b"}}},
		{"unclosed backtick stops line", "[[a]] `x [[b]]\n[[c]]", []RecordLink{{ID: "a"}, {ID: "c"}}},
		{"fence ignored", "[[a]]\n```go\n[[b]]\n```\n[[c]]", []RecordLink{{ID: "a"}, {ID: "c"}}},
		{"tilde fence ignored", "~~~\n[[b]]\n~~~\n[[c]]", []RecordLink{{ID: "c"}}},
		{"unclosed fence hides rest", "```\n[[b]]", nil},
		{"empty", "[[]] [[ | text]]", nil},
		{"unclosed", "[[a", nil},
		{"spaces in id", "[[two words]]", nil},
		{"nested opener", "[[a [[b]]", []RecordLink{{ID: "b"}}},
		{"extra slash", "[[a/b/c]] [[/x]] [[p/]]", nil},
		{"triple brackets", "[[[a]]]", []RecordLink{{ID: "a"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseRecordLinks(tc.text)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseRecordLinks(%q) = %#v, want %#v", tc.text, got, tc.want)
			}
		})
	}
}

func TestRecordLinkTarget(t *testing.T) {
	project, id := RecordLink{ID: "n1"}.Target("alpha")
	if project != "alpha" || id != "n1" {
		t.Fatalf("unqualified target = %s/%s", project, id)
	}
	project, id = RecordLink{ProjectID: "beta", ID: "n1"}.Target("alpha")
	if project != "beta" || id != "n1" {
		t.Fatalf("qualified target = %s/%s", project, id)
	}
}
