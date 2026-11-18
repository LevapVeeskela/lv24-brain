package textbudget

import (
	"fmt"
	"strings"
	"testing"
)

// TestTokensScriptDensity pins the per-script heuristic: Latin text keeps the
// four-characters-per-token estimate, Cyrillic and CJK pack about two.
func TestTokensScriptDensity(t *testing.T) {
	for name, tc := range map[string]struct {
		text string
		want int
	}{
		"empty":          {"", 0},
		"latin short":    {"hi", 1},
		"latin 8":        {"abcdefgh", 2},
		"latin 400":      {strings.Repeat("a", 400), 100},
		"cyrillic 10":    {"приветмир", 5},
		"cyrillic words": {"привет мир", 6},
		"chinese 8":      {"上下文预算测试", 4},
		"mixed":          {"hello привет", 5},
	} {
		t.Run(name, func(t *testing.T) {
			if got := Tokens(tc.text); got != tc.want {
				t.Fatalf("Tokens(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

// TestTruncateRespectsBudgetForEveryScript checks that the truncated text never
// exceeds the budget, measured with the same estimate.
func TestTruncateRespectsBudgetForEveryScript(t *testing.T) {
	texts := map[string]string{
		"latin":    strings.Repeat("latin words pack a budget ", 80),
		"cyrillic": strings.Repeat("кириллический текст занимает вдвое больше ", 40),
		"mixed":    strings.Repeat("budget бюджет ", 60),
	}
	for name, text := range texts {
		for _, budget := range []int{1, 5, 40, 200} {
			t.Run(name+"/"+fmt.Sprint(budget), func(t *testing.T) {
				cut, truncated := Truncate(text, budget)
				if !truncated {
					t.Fatalf("Truncate(%d tokens) did not cut anything", budget)
				}
				if got := Tokens(cut); got > budget {
					t.Fatalf("truncated %q costs %d tokens, budget %d", cut, got, budget)
				}
				if !strings.HasSuffix(cut, "(truncated)") && budget > 5 {
					t.Fatalf("cut text lacks the marker: %q", cut)
				}
			})
		}
	}
}

// TestTruncateKeepsTextThatFits guards against a marker on unchanged text.
func TestTruncateKeepsTextThatFits(t *testing.T) {
	text := "short latin text"
	got, truncated := Truncate(text, Tokens(text))
	if truncated || got != text {
		t.Fatalf("Truncate = %q, %v; want the text unchanged", got, truncated)
	}
}
