package serverstore

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/searchnorm"
)

// maxTSClauses bounds how many clauses become separate tsquery fragments; a
// longer query is passed to websearch_to_tsquery whole, as before stemming.
const maxTSClauses = 24

// tsClause is one websearch fragment (a word, a quoted phrase or an excluded
// word or phrase) and, for plain words, the to_tsquery prefix of its stem.
type tsClause struct {
	fragment string
	prefix   string
}

// buildTSQuery turns query text into a tsquery SQL expression and its bound
// values, numbered from firstParam. It keeps websearch_to_tsquery semantics:
// clauses separated by whitespace are ANDed, a bare OR separates alternatives,
// and quoted phrases and -exclusions are passed to websearch_to_tsquery
// unchanged. Each plain word of searchnorm.MinTermRunes or more runes is ORed
// with a prefix query on its word-form stem ('decision':* for "decisions"), so
// the result matches everything the plain websearch query matched, and items
// with the exact form match both alternatives and rank higher.
//
// The returned expression contains only fixed SQL and $N placeholders; user
// text is always a bound value, and prefix values hold only letters and digits.
func buildTSQuery(text string, firstParam int) (string, []any) {
	groups := parseTSGroups(text)
	if countClauses(groups) == 0 || countClauses(groups) > maxTSClauses {
		return websearchExpr(firstParam), []any{text}
	}
	param := firstParam
	args := make([]any, 0, 2*countClauses(groups))
	alternatives := make([]string, 0, len(groups))
	for _, group := range groups {
		parts := make([]string, 0, len(group))
		for _, clause := range group {
			expr := websearchExpr(param)
			args = append(args, clause.fragment)
			param++
			if clause.prefix != "" {
				expr = "(" + expr + " || to_tsquery('simple', $" + strconv.Itoa(param) + "::text))"
				args = append(args, clause.prefix)
				param++
			}
			parts = append(parts, expr)
		}
		alternatives = append(alternatives, "("+strings.Join(parts, " && ")+")")
	}
	return "(" + strings.Join(alternatives, " || ") + ")", args
}

func websearchExpr(param int) string {
	return "websearch_to_tsquery('simple', $" + strconv.Itoa(param) + "::text)"
}

func countClauses(groups [][]tsClause) int {
	count := 0
	for _, group := range groups {
		count += len(group)
	}
	return count
}

// parseTSGroups splits text into OR-separated groups of clauses. Empty groups
// (a leading, trailing or doubled OR) are dropped, as websearch_to_tsquery
// ignores such operators.
func parseTSGroups(text string) [][]tsClause {
	var groups [][]tsClause
	var current []tsClause
	for _, token := range scanTSTokens(text) {
		if strings.EqualFold(token, "or") {
			if len(current) > 0 {
				groups = append(groups, current)
			}
			current = nil
			continue
		}
		current = append(current, tsClause{fragment: token, prefix: prefixFor(token)})
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// scanTSTokens splits text on whitespace, keeping a quoted phrase (optionally
// preceded by -) as one token. An unclosed quote runs to the end of the text,
// as in websearch_to_tsquery.
func scanTSTokens(text string) []string {
	var tokens []string
	runes := []rune(text)
	for i := 0; i < len(runes); {
		if unicode.IsSpace(runes[i]) {
			i++
			continue
		}
		start := i
		if runes[i] == '"' || (runes[i] == '-' && i+1 < len(runes) && runes[i+1] == '"') {
			i = phraseEnd(runes, start)
		} else {
			for i < len(runes) && !unicode.IsSpace(runes[i]) {
				i++
			}
		}
		tokens = append(tokens, string(runes[start:i]))
	}
	return tokens
}

// phraseEnd returns the index just past the closing quote of the phrase that
// starts at start, or len(runes) when the quote is not closed.
func phraseEnd(runes []rune, start int) int {
	open := start
	if runes[open] == '-' {
		open++
	}
	for i := open + 1; i < len(runes); i++ {
		if runes[i] == '"' {
			return i + 1
		}
	}
	return len(runes)
}

// prefixFor returns the to_tsquery prefix value for a plain word, or "" for
// phrases, exclusions and words that are not eligible for stemming.
func prefixFor(token string) string {
	if strings.HasPrefix(token, "-") || strings.ContainsRune(token, '"') {
		return ""
	}
	stem, ok := searchnorm.Stem(token)
	if !ok {
		return ""
	}
	return "'" + stem + "':*"
}
