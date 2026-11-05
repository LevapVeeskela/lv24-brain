package sqliteindex

import (
	"strings"
	"unicode"
)

// matchExpression converts free text into an FTS5 query. Every term becomes a
// quoted string literal joined by AND, so user punctuation, operators and
// Unicode text can never change the query structure. Terms without a letter or
// digit are dropped; when nothing remains the search yields no hits.
func matchExpression(text string) (string, bool) {
	fields := strings.Fields(text)
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		term := strings.ReplaceAll(field, `"`, " ")
		term = strings.TrimSpace(term)
		if !hasTokenRune(term) {
			continue
		}
		terms = append(terms, `"`+term+`"`)
	}
	if len(terms) == 0 {
		return "", false
	}
	return strings.Join(terms, " AND "), true
}

func hasTokenRune(value string) bool {
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
