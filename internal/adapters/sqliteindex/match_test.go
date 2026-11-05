package sqliteindex

import "testing"

func TestMatchExpression(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{name: "single term", input: "sqlite", want: `"sqlite"`, ok: true},
		{name: "multiple terms", input: "sqlite index", want: `"sqlite" AND "index"`, ok: true},
		{name: "cyrillic", input: "архитектура сервиса", want: `"архитектура" AND "сервиса"`, ok: true},
		{name: "embedded quotes", input: `he said "hi"`, want: `"he" AND "said" AND "hi"`, ok: true},
		{name: "operators are text", input: "* OR 1=1", want: `"OR" AND "1=1"`, ok: true},
		{name: "empty", input: "", ok: false},
		{name: "whitespace", input: "   ", ok: false},
		{name: "punctuation only", input: "!!! ...", ok: false},
		{name: "quotes only", input: `""`, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := matchExpression(test.input)
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
