package searchnorm

import (
	"strings"
	"testing"
)

func TestStemWordFamiliesShareAPrefix(t *testing.T) {
	families := [][]string{
		{"решение", "решения", "решений", "решениям", "решением"},
		{"деплой", "деплоя", "деплоем", "деплою"},
		{"проекты", "проектов", "проектам"},
		{"decision", "decisions"},
		{"deploy", "deployed", "deploying", "deploys"},
		{"policy", "policies"},
		{"query", "queries", "queried"},
		{"release", "releases", "released"},
	}
	for _, family := range families {
		for _, query := range family {
			stem, ok := Stem(query)
			if !ok {
				t.Fatalf("Stem(%q) not eligible", query)
			}
			for _, form := range family {
				if !strings.HasPrefix(strings.ToLower(form), stem) {
					t.Errorf("Stem(%q) = %q is not a prefix of %q", query, stem, form)
				}
			}
		}
	}
}

func TestStem(t *testing.T) {
	tests := []struct {
		term string
		want string
	}{
		{"decisions", "decision"},
		{"Decisions", "decision"},
		{"decisions,", "decision"},
		{"deployed", "deploy"},
		{"deploying", "deploy"},
		{"policies", "polic"},
		{"policy", "polic"},
		{"running", "run"},
		{"stopped", "stop"},
		{"caching", "cach"},
		{"classes", "class"},
		{"boxes", "box"},
		{"notes", "note"},
		{"team's", "team"},
		{"решений", "решен"},
		{"решение", "решен"},
		{"решениям", "решен"},
		{"деплоем", "депло"},
		{"деплой", "депл"},
		{"данных", "данн"},
		{"сложности", "сложн"},
		{"решён", "решен"},
		// Must not be over-stemmed.
		{"string", "string"},
		{"thing", "thing"},
		{"status", "status"},
		{"class", "class"},
		{"analysis", "analysis"},
		{"this", "this"},
		{"news", "news"},
		{"speed", "speed"},
		{"only", "only"},
		{"deploy", "deploy"},
		{"decision", "decision"},
		{"сервер", "сервер"},
		{"проект", "проект"},
		{"стол", "стол"},
		{"кость", "кос"},
		{"lv24", "lv24"},
		{"2026", "2026"},
		{"café", "café"},
		{"lv24бот", "lv24бот"},
	}
	for _, test := range tests {
		got, ok := Stem(test.term)
		if !ok || got != test.want {
			t.Errorf("Stem(%q) = %q, %t; want %q, true", test.term, got, ok, test.want)
		}
	}
}

func TestStemIneligibleTerms(t *testing.T) {
	for _, term := range []string{"", "db", "кэш", "use", "OR", "lv24-brain", "foo.bar", "v0.6.3", "a/b/c", "!!!"} {
		if stem, ok := Stem(term); ok {
			t.Errorf("Stem(%q) = %q, true; want not eligible", term, stem)
		}
	}
}
