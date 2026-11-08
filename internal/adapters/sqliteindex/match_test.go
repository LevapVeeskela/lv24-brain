package sqliteindex

import "testing"

func TestMatchExpression(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		joiner string
		want   string
		ok     bool
	}{
		{name: "single short term exact", input: "db", joiner: joinAll, want: `"db"`, ok: true},
		{name: "long term is prefix", input: "sqlite", joiner: joinAll, want: `"sqlite"*`, ok: true},
		{name: "multiple terms AND", input: "sqlite db", joiner: joinAll, want: `"sqlite"* AND "db"`, ok: true},
		{name: "multiple terms OR", input: "sqlite db", joiner: joinAny, want: `"sqlite"* OR "db"`, ok: true},
		{name: "cyrillic prefix", input: "архитектура сервиса", joiner: joinAll, want: `("архитектура"* OR "архитектур"*) AND ("сервиса"* OR "сервис"*)`, ok: true},
		{name: "english word form", input: "decisions", joiner: joinAll, want: `("decisions"* OR "decision"*)`, ok: true},
		{name: "exclusion stays exact", input: "-decisions", joiner: joinAll, want: `"-decisions"*`, ok: true},
		{name: "embedded quotes", input: `he said "hi"`, joiner: joinAll, want: `"he" AND "said"* AND "hi"`, ok: true},
		{name: "operators are text", input: "* OR 1=1", joiner: joinAll, want: `"OR" AND "1=1"`, ok: true},
		{name: "column filter is text", input: "title:x", joiner: joinAll, want: `"title:x"*`, ok: true},
		{name: "empty", input: "", joiner: joinAll, ok: false},
		{name: "whitespace", input: "   ", joiner: joinAll, ok: false},
		{name: "punctuation only", input: "!!! ...", joiner: joinAll, ok: false},
		{name: "quotes only", input: `""`, joiner: joinAll, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := matchExpression(test.input, test.joiner)
			if ok != test.ok {
				t.Fatalf("ok = %v, want %v", ok, test.ok)
			}
			if got != test.want {
				t.Fatalf("expression = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeLimit(t *testing.T) {
	tests := []struct {
		limit int
		want  int
	}{
		{limit: 0, want: DefaultLimit},
		{limit: -5, want: DefaultLimit},
		{limit: 1, want: 1},
		{limit: 5, want: 5},
		{limit: MaxLimit, want: MaxLimit},
		{limit: MaxLimit + 1, want: MaxLimit},
	}
	for _, test := range tests {
		if got := normalizeLimit(test.limit); got != test.want {
			t.Fatalf("normalizeLimit(%d) = %d, want %d", test.limit, got, test.want)
		}
	}
}
