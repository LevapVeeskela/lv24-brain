// Package textbudget sizes and truncates text for an agent context.
//
// Tokens are approximated with the usual four-characters-per-token rule. The
// approximation is deliberate: it needs no tokenizer dependency, it is
// deterministic, and a budget is a limit to respect, not a billing figure.
package textbudget

import (
	"strings"
	"unicode/utf8"
)

const (
	// runesPerToken is the estimate both Tokens and Truncate use.
	runesPerToken = 4
	// truncationMarker ends a shortened text so a reader can tell that the
	// record continues.
	truncationMarker = "\n… (truncated)"
	// PreviewRunes is the longest preview a search returns per hit. A model
	// reads the preview first and asks for the body only when it needs one,
	// which is what keeps a search answer cheap.
	PreviewRunes = 160
)

// Tokens estimates the token count of text. An empty text is zero tokens; any
// non-empty text costs at least one.
func Tokens(text string) int {
	runes := utf8.RuneCountInString(text)
	if runes == 0 {
		return 0
	}
	return (runes + runesPerToken - 1) / runesPerToken
}

// Preview shortens text to at most maxRunes runes, marking the cut with an
// ellipsis. Text that fits is returned unchanged, without a marker.
func Preview(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(text) <= maxRunes {
		return text
	}
	runes := []rune(text)
	return strings.TrimRight(string(runes[:maxRunes-1]), " \t\r\n") + "…"
}

// Truncate shortens text to at most maxTokens and reports whether it cut
// anything. The result never exceeds maxTokens plus the marker: the marker is
// part of the budget, so a caller can rely on the number. A non-positive
// budget returns the text unchanged.
func Truncate(text string, maxTokens int) (string, bool) {
	if maxTokens <= 0 || Tokens(text) <= maxTokens {
		return text, false
	}
	suffix := truncationMarker
	suffixTokens := Tokens(suffix)
	if suffixTokens >= maxTokens {
		// The budget cannot hold both text and marker; keep a slice of the
		// record rather than only the marker.
		return truncateRunes(text, maxTokens*runesPerToken), true
	}
	body := truncateRunes(text, (maxTokens-suffixTokens)*runesPerToken)
	return body + suffix, true
}

// truncateRunes cuts text at a word boundary when one is close to the limit,
// so a shortened body does not end mid-word.
func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	cut := strings.TrimRight(string(runes[:limit]), " \t\r\n")
	if index := strings.LastIndexAny(cut, " \t\r\n"); index > len(cut)/2 {
		cut = cut[:index]
	}
	return strings.TrimRight(cut, " \t\r\n")
}
