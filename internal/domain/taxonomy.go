package domain

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RecordScope says where a record is meant to apply. The empty value is the
// historical default and means project scope; it is never written to
// frontmatter, so records written by older engines keep their meaning.
type RecordScope string

const (
	ScopeProject  RecordScope = "project"
	ScopePersonal RecordScope = "personal"
	ScopeGlobal   RecordScope = "global"
)

// Valid reports whether the scope is one of the documented values. The empty
// scope is valid and means project scope.
func (s RecordScope) Valid() bool {
	switch s {
	case "", ScopeProject, ScopePersonal, ScopeGlobal:
		return true
	default:
		return false
	}
}

// Normalized resolves the empty scope to project scope.
func (s RecordScope) Normalized() RecordScope {
	if s == "" {
		return ScopeProject
	}
	return s
}

// TopicKeyMaxRunes bounds a topic key. It matches the engram topic key cap so
// an imported key never has to be truncated silently.
const TopicKeyMaxRunes = 120

// SubtypeMaxRunes bounds the free-form subtype (the engram observation type).
const SubtypeMaxRunes = 120

// NormalizeTopicKey lowercases a topic key, turns runs of spaces and
// underscores into single hyphens and rejects a key that is empty, too long or
// contains anything other than letters, digits and the separators - . / :.
// A normalized key is stable: normalizing it twice yields the same value.
func NormalizeTopicKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", nil
	}
	if !utf8.ValidString(key) {
		return "", NewValidationError("topic", "must be valid UTF-8")
	}
	var builder strings.Builder
	lastSeparator := false
	for _, r := range key {
		switch {
		case unicode.IsSpace(r) || r == '_':
			if !lastSeparator {
				builder.WriteByte('-')
				lastSeparator = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == '/' || r == ':':
			builder.WriteRune(unicode.ToLower(r))
			lastSeparator = r == '-'
		default:
			return "", NewValidationError("topic",
				fmt.Sprintf("contains %q; a topic key uses letters, digits and - . / :", r))
		}
	}
	normalized := strings.Trim(builder.String(), "-")
	if normalized == "" {
		return "", NewValidationError("topic", "must contain a letter or digit")
	}
	if utf8.RuneCountInString(normalized) > TopicKeyMaxRunes {
		return "", NewValidationError("topic",
			fmt.Sprintf("must be at most %d characters", TopicKeyMaxRunes))
	}
	return normalized, nil
}

// Taxonomy is the optional engram-compatible classification shared by every
// record kind: a free-form subtype, a scope, a stable topic key and the number
// of topic upserts that produced the record.
type Taxonomy struct {
	// Subtype is the engram observation type (architecture, bugfix, discovery,
	// session_summary, feature, policy, ...). It is free text.
	Subtype string `json:"subtype,omitempty"`
	// Scope is project, personal or global. Empty means project.
	Scope RecordScope `json:"scope,omitempty"`
	// TopicKey is the normalized topic a record belongs to. Empty means the
	// record is append-only.
	TopicKey string `json:"topicKey,omitempty"`
	// Occurrences counts how many topic writes the record represents. Zero is
	// read as one, so records written before the field existed stay correct.
	Occurrences int `json:"occurrences,omitempty"`
}

// Normalized trims the subtype and the topic key and resolves the defaults.
func (t Taxonomy) Normalized() Taxonomy {
	subtype := strings.TrimSpace(t.Subtype)
	scope := t.Scope.Normalized()
	occurrences := t.Occurrences
	if occurrences < 1 {
		occurrences = 1
	}
	return Taxonomy{Subtype: subtype, Scope: scope, TopicKey: t.TopicKey, Occurrences: occurrences}
}

// NormalizedOccurrences returns the occurrence count, defaulting to one.
func (t Taxonomy) NormalizedOccurrences() int {
	if t.Occurrences < 1 {
		return 1
	}
	return t.Occurrences
}

// Validate checks the fields that are present. The topic key must already be
// normalized: a hand-edited key with uppercase letters is rejected instead of
// silently colliding with a normalized one.
func (t Taxonomy) Validate(field, id string) error {
	if err := validateSubtype(field+".subtype", t.Subtype); err != nil {
		return err
	}
	if !t.Scope.Valid() {
		return NewValidationError(field+".scope",
			fmt.Sprintf("must be %q, %q or %q", ScopeProject, ScopePersonal, ScopeGlobal))
	}
	if t.TopicKey != "" {
		normalized, err := NormalizeTopicKey(t.TopicKey)
		if err != nil {
			return err
		}
		if normalized != t.TopicKey {
			return NewValidationError(field+".topic",
				fmt.Sprintf("%q is not normalized; use %q", t.TopicKey, normalized))
		}
	}
	if t.Occurrences < 0 {
		return NewValidationError(field+".occurrences", "must not be negative")
	}
	return nil
}

// validateSubtype keeps a subtype on one line and inside the documented cap.
func validateSubtype(field, subtype string) error {
	subtype = strings.TrimSpace(subtype)
	if subtype == "" {
		return nil
	}
	if subtype != strings.TrimSpace(subtype) {
		return NewValidationError(field, "must not contain surrounding whitespace")
	}
	if !utf8.ValidString(subtype) {
		return NewValidationError(field, "must be valid UTF-8")
	}
	if utf8.RuneCountInString(subtype) > SubtypeMaxRunes {
		return NewValidationError(field, fmt.Sprintf("must be at most %d characters", SubtypeMaxRunes))
	}
	if strings.ContainsAny(subtype, "\r\n\t") {
		return NewValidationError(field, "must be a single line")
	}
	return nil
}

// MaxTopicHistoryEntries bounds the previous bodies a topic record keeps. The
// oldest entries are dropped first; the current body is never history.
const MaxTopicHistoryEntries = 20

// HistoryEntry is one previous body of a topic record. At is the update time
// the body was replaced at; a zero At is only produced by a hand-written
// history section and is rendered without a heading.
type HistoryEntry struct {
	At   string `json:"at,omitempty"`
	Body string `json:"body"`
}

// TopicHistory holds the previous bodies of a topic record, newest first.
type TopicHistory struct {
	History []HistoryEntry `json:"history,omitempty"`
}

// PrependedHistory returns history with entry first and at most
// MaxTopicHistoryEntries entries. The input is not modified, so a caller can
// keep the stored history for a comparison.
func PrependedHistory(history []HistoryEntry, entry HistoryEntry) []HistoryEntry {
	entry.Body = strings.TrimSpace(entry.Body)
	if entry.Body == "" {
		return BoundedHistory(history)
	}
	next := make([]HistoryEntry, 0, len(history)+1)
	next = append(next, entry)
	next = append(next, history...)
	return BoundedHistory(next)
}

// BoundedHistory truncates history to the newest MaxTopicHistoryEntries
// entries. Empty bodies are dropped.
func BoundedHistory(history []HistoryEntry) []HistoryEntry {
	out := make([]HistoryEntry, 0, len(history))
	for _, entry := range history {
		if strings.TrimSpace(entry.Body) == "" {
			continue
		}
		out = append(out, entry)
		if len(out) == MaxTopicHistoryEntries {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ValidateHistory rejects an entry body that is not valid UTF-8. The count is
// bounded on write (BoundedHistory), not on read: a hand-edited file with more
// entries still parses so its content can be repaired instead of hidden.
func ValidateHistory(field string, history []HistoryEntry) error {
	for i, entry := range history {
		if !utf8.ValidString(entry.Body) {
			return NewValidationError(fmt.Sprintf("%s[%d].body", field, i), "must be valid UTF-8")
		}
	}
	return nil
}
