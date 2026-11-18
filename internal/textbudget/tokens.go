// Package textbudget sizes and truncates text for an agent context.
//
// Tokens are approximated from the script of each rune: Latin text packs about
// four characters into a token, while Cyrillic, Greek, CJK and other dense
// scripts pack about two. The approximation is deliberate: it needs no
// tokenizer dependency, it is deterministic, and a budget is a limit to
// respect, not a billing figure. A pure-Latin text therefore keeps the
// historical four-characters-per-token estimate.
package textbudget

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// latinRunesPerToken is the estimate for Latin and other letter-sparse
	// scripts.
	latinRunesPerToken = 4
	// wideRunesPerToken is the estimate for scripts whose tokens hold about
	// two characters: Cyrillic, Greek, Hebrew, Arabic, CJK, Hangul and the
	// Indic/Thai scripts. It is also the conservative value for ambiguous
	// scripts, because under-counting would overflow the budget.
	wideRunesPerToken = 2
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
	if text == "" {
		return 0
	}
	latin, wide := scriptRunes(text)
	tokens := ceilDiv(latin, latinRunesPerToken) + ceilDiv(wide, wideRunesPerToken)
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

// scriptRunes counts the runes of a text by token density.
func scriptRunes(text string) (latin, wide int) {
	for _, r := range text {
		if isWideScript(r) {
			wide++
			continue
		}
		latin++
	}
	return latin, wide
}

// isWideScript reports whether a rune belongs to a script whose tokens hold
// about two characters. Latin letters, ASCII punctuation and the punctuation of
// other scripts stay in the four-characters bucket.
func isWideScript(r rune) bool {
	if r < utf8.RuneSelf {
		return false
	}
	switch {
	case unicode.Is(unicode.Cyrillic, r),
		unicode.Is(unicode.Greek, r),
		unicode.Is(unicode.Hebrew, r),
		unicode.Is(unicode.Arabic, r),
		unicode.Is(unicode.Han, r),
		unicode.Is(unicode.Hangul, r),
		unicode.Is(unicode.Hiragana, r),
		unicode.Is(unicode.Katakana, r),
		unicode.Is(unicode.Thai, r),
		unicode.Is(unicode.Devanagari, r):
		return true
	}
	return false
}

func ceilDiv(value, divisor int) int {
	if value <= 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
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
// anything. The result never exceeds maxTokens, the marker included, so a
// caller can rely on the number. A non-positive budget returns the text
// unchanged.
func Truncate(text string, maxTokens int) (string, bool) {
	if maxTokens <= 0 || Tokens(text) <= maxTokens {
		return text, false
	}
	suffix := truncationMarker
	if Tokens(suffix) >= maxTokens {
		// The budget cannot hold both text and marker; keep a slice of the
		// record rather than only the marker.
		return truncateRunes(text, maxTokens), true
	}
	return truncateRunes(text, maxTokens-Tokens(suffix)) + suffix, true
}

// truncateRunes returns the longest word-aligned prefix of text that fits
// maxTokens. Rune density varies by script, so the prefix length is found by
// binary search on the token estimate instead of a fixed runes-per-token
// factor.
func truncateRunes(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		if Tokens(string(runes[:middle])) <= maxTokens {
			low = middle
			continue
		}
		high = middle - 1
	}
	cut := strings.TrimRight(string(runes[:low]), " \t\r\n")
	if index := strings.LastIndexAny(cut, " \t\r\n"); index > len(cut)/2 {
		cut = cut[:index]
	}
	return strings.TrimRight(cut, " \t\r\n")
}
