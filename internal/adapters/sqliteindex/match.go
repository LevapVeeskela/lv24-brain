package sqliteindex

import (
	"strings"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/searchnorm"
)

const (
	// prefixMinRunes is the shortest token that is matched as a prefix. Shorter
	// terms stay exact so that, for example, "db" does not match "dbx".
	prefixMinRunes = 4

	joinAll = " AND "
	joinAny = " OR "
)

// matchExpression converts free text into an FTS5 query whose terms are joined
// by joiner (AND for the strict pass, OR for the relaxed pass). Every term is a
// quoted string literal, so user punctuation, operators, column filters and
// Unicode text can never change the query structure. Terms of prefixMinRunes or
// more letters/digits become prefix queries. Terms without a letter or digit are
// dropped; when nothing remains ok is false.
func matchExpression(text, joiner string) (string, bool) {
	terms := searchTerms(text)
	if len(terms) == 0 {
		return "", false
	}
	parts := make([]string, len(terms))
	for index, term := range terms {
		parts[index] = ftsTerm(term)
	}
	return strings.Join(parts, joiner), true
}

// searchTerms splits text on whitespace, removes double quotes (the only
// character that could end an FTS5 string literal) and keeps terms that contain
// at least one letter or digit.
func searchTerms(text string) []string {
	fields := strings.Fields(text)
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		term := strings.TrimSpace(strings.ReplaceAll(field, `"`, " "))
		if !hasTokenRune(term) {
			continue
		}
		terms = append(terms, term)
	}
	return terms
}

// ftsTerm quotes a single term and adds the FTS5 prefix operator when the term
// is long enough. The prefix operator sits outside the quotes. When the term
// has a shorter word-form stem (decisions -> decision, решений -> решен), the
// stem is ORed in as a prefix: the stem is a prefix of the term, so the result
// matches everything the term alone matched, and documents containing the
// exact form match both alternatives and rank higher in bm25.
func ftsTerm(term string) string {
	quoted := `"` + term + `"`
	if tokenRuneCount(term) < prefixMinRunes {
		return quoted
	}
	if stem, ok := stemOf(term); ok {
		return `("` + term + `"* OR "` + stem + `"*)`
	}
	return quoted + "*"
}

// stemOf returns the word-form stem of term when it differs from the term.
// Terms that look like operators (-exclude) are left exact.
func stemOf(term string) (string, bool) {
	if strings.HasPrefix(term, "-") {
		return "", false
	}
	stem, ok := searchnorm.Stem(term)
	if !ok || stem == strings.ToLower(strings.TrimFunc(term, notTokenRune)) {
		return "", false
	}
	return stem, true
}

func notTokenRune(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

func hasTokenRune(value string) bool {
	return tokenRuneCount(value) > 0
}

// tokenRuneCount counts letters and digits, the runes the unicode61 tokenizer
// keeps in a token.
func tokenRuneCount(value string) int {
	count := 0
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			count++
		}
	}
	return count
}

// termCount reports how many searchable terms text contains.
func termCount(text string) int {
	return len(searchTerms(text))
}
