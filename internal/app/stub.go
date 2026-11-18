package app

import (
	"regexp"
	"strings"
)

// stubTitlePattern is the shape of a title that is only a generated record
// label: <kind>-<date>-<slug>-<id>.
var stubTitlePattern = regexp.MustCompile(`(?i)^(?:note|decision|attempt|handoff)-[0-9]{4}-[0-9]{2}-[0-9]{2}-[a-z0-9]+(?:-[a-z0-9]+)*$`)

// stubBodyPhrases are the whole-body values that carry no information.
var stubBodyPhrases = []string{
	"nothing new",
	"session ended",
	"no changes",
	"no updates",
}

// IsStubRecord reports whether a record carries no retrieval value: its title
// is only a generated label+id, or its body is one of the "nothing happened"
// phrases. The check is case-insensitive and tolerates surrounding punctuation,
// which is how hooks and journal writers emit those lines.
//
// kind names the record being checked. The rule is title- and body-shaped
// today, so it does not narrow the match by kind; callers pass the record's
// identity so a kind-specific rule can be added without changing them.
//
// The flag is derived, never stored in Markdown: lists and counts still include
// the record, and only retrieval and review skip it. The index projection keeps
// the flag so search can filter without re-reading the record.
func IsStubRecord(kind, title, body string) bool {
	if stubTitlePattern.MatchString(strings.TrimSpace(title)) {
		return true
	}
	normalized := strings.Trim(normalizeProjectionText(body), " .,;:!?()[]{}\"'`+-—–")
	for _, phrase := range stubBodyPhrases {
		if normalized == phrase {
			return true
		}
	}
	return false
}
