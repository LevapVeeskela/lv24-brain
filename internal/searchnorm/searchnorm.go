// Package searchnorm normalizes search terms on the query side so that one word
// form finds the others ("decisions" finds "decision", "решений" finds
// "решение"). It is a light, dependency-free suffix stripper, not a full
// stemmer: the stem it returns is meant to be used as a prefix query, so it
// only needs to be a prefix shared by the common forms of a word.
//
// The package is pure (no I/O, no adapter imports), so both the SQLite and the
// PostgreSQL search adapters can share it.
package searchnorm

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MinTermRunes is the shortest term that is stemmed. Shorter terms stay
	// exact so that, for example, "db" does not turn into a prefix.
	MinTermRunes = 4
	// minStemRunes is the shortest stem a suffix may leave behind.
	minStemRunes = 3
	// minEnglishYRunes is the shortest English word whose final consonant+y
	// is dropped (policy -> polic); "only" or "copy" stay intact.
	minEnglishYRunes = 5
)

// Stem returns the lowercased prefix stem of term and whether the term is
// eligible for prefix stemming. Leading and trailing punctuation is ignored
// and a trailing possessive 's is dropped. A term is eligible when what remains
// has at least MinTermRunes runes and consists of letters and digits only;
// operators, quoted phrases and terms with inner punctuation (paths, hosts,
// versions) are not eligible and must be matched as before.
//
// The stem is always a prefix of the lowercased term (after ё is folded to е),
// so a prefix query on the stem matches everything the term matched.
func Stem(term string) (string, bool) {
	word := strings.TrimFunc(term, isNotTokenRune)
	word = strings.ToLower(word)
	word = trimPossessive(word)
	word = strings.ReplaceAll(word, "ё", "е")
	if utf8.RuneCountInString(word) < MinTermRunes || !allTokenRunes(word) {
		return "", false
	}
	switch scriptOf(word) {
	case scriptCyrillic:
		return stemRussian(word), true
	case scriptLatin:
		return stemEnglish(word), true
	default:
		return word, true
	}
}

func isNotTokenRune(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

func allTokenRunes(word string) bool {
	return strings.IndexFunc(word, isNotTokenRune) < 0
}

// trimPossessive drops a trailing 's or ’s (team's -> team).
func trimPossessive(word string) string {
	for _, suffix := range []string{"'s", "’s"} {
		if trimmed, ok := strings.CutSuffix(word, suffix); ok {
			return strings.TrimFunc(trimmed, isNotTokenRune)
		}
	}
	return word
}

type script int

const (
	scriptOther script = iota
	scriptLatin
	scriptCyrillic
)

// scriptOf reports the single letter script of word (ASCII Latin or
// Cyrillic); words that mix scripts, use other letters or have no letters are
// scriptOther and are left unchanged.
func scriptOf(word string) script {
	found := scriptOther
	for _, r := range word {
		var current script
		switch {
		case unicode.Is(unicode.Cyrillic, r):
			current = scriptCyrillic
		case r < utf8.RuneSelf && unicode.IsLetter(r):
			// English rules work on ASCII bytes; other Latin letters
			// (é, ß) fall through to scriptOther and stay unchanged.
			current = scriptLatin
		case unicode.IsLetter(r):
			return scriptOther
		default:
			continue
		}
		if found != scriptOther && found != current {
			return scriptOther
		}
		found = current
	}
	return found
}

// russianSuffixes are common noun, adjective and verb endings, ordered
// longest first by init so the longest matching ending is stripped.
var russianSuffixes = []string{
	"ость", "ости", "остью", "остей", "остям", "остями", "остях",
	"иями", "ями", "ами", "иях", "иям", "ием", "ией", "ия", "ию", "ие", "ий", "ии",
	"ого", "его", "ому", "ему", "ыми", "ими", "ых", "их", "ым", "им",
	"ах", "ях", "ам", "ям", "ом", "ем", "ой", "ей", "ый", "ая", "яя", "ое", "ее", "ые", "ую", "юю", "ов", "ев",
	"ться", "ать", "ять", "ить", "еть", "ть",
	"а", "я", "ы", "и", "у", "ю", "е", "о", "й", "ь",
}

func init() {
	sort.SliceStable(russianSuffixes, func(i, j int) bool {
		return utf8.RuneCountInString(russianSuffixes[i]) > utf8.RuneCountInString(russianSuffixes[j])
	})
}

const russianVowels = "аеиоуыэюя"

// stemRussian strips the longest known ending that leaves a stem of at least
// minStemRunes runes containing a vowel.
func stemRussian(word string) string {
	for _, suffix := range russianSuffixes {
		base, ok := strings.CutSuffix(word, suffix)
		if ok && utf8.RuneCountInString(base) >= minStemRunes && strings.ContainsAny(base, russianVowels) {
			return base
		}
	}
	return word
}

// englishExceptions end in a letter the rules would strip but are not
// inflected forms.
var englishExceptions = map[string]bool{
	"this": true, "does": true, "goes": true, "news": true, "always": true,
	"series": true, "species": true, "perhaps": true, "whereas": true,
	"means": true, "lens": true, "yes": true, "thus": true,
}

// stemEnglish strips one common inflection: -ies, -ied, -ing, -ed, -es (after
// s, x, z, ch, sh), -s, or a final consonant+y. A rule applies only when the
// remaining stem is long enough and contains a vowel; otherwise the next rule
// is tried.
func stemEnglish(word string) string {
	if englishExceptions[word] {
		return word
	}
	if base, ok := strings.CutSuffix(word, "ies"); ok && validEnglishStem(base) {
		return base
	}
	if base, ok := strings.CutSuffix(word, "ied"); ok && validEnglishStem(base) {
		return base
	}
	if base, ok := strings.CutSuffix(word, "ing"); ok && validEnglishStem(base) {
		return undouble(base)
	}
	if base, ok := strings.CutSuffix(word, "ed"); ok && !strings.HasSuffix(word, "eed") && validEnglishStem(base) {
		return undouble(base)
	}
	if base, ok := strings.CutSuffix(word, "es"); ok && sibilantEnding(base) && validEnglishStem(base) {
		return base
	}
	if base, ok := strings.CutSuffix(word, "s"); ok && keepsPluralS(word) && validEnglishStem(base) {
		return base
	}
	return stripConsonantY(word)
}

func validEnglishStem(base string) bool {
	return utf8.RuneCountInString(base) >= minStemRunes && strings.ContainsAny(base, "aeiouy")
}

func sibilantEnding(base string) bool {
	for _, ending := range []string{"s", "x", "z", "ch", "sh"} {
		if strings.HasSuffix(base, ending) {
			return true
		}
	}
	return false
}

// keepsPluralS reports whether a final s is a plural/verb ending: words in
// -ss, -us and -is (class, status, analysis) keep it.
func keepsPluralS(word string) bool {
	for _, ending := range []string{"ss", "us", "is"} {
		if strings.HasSuffix(word, ending) {
			return false
		}
	}
	return true
}

// undouble drops one letter of a final doubled consonant (runn -> run).
func undouble(base string) string {
	n := len(base)
	if n < 2 || base[n-1] != base[n-2] || isEnglishVowel(base[n-1]) {
		return base
	}
	shorter := base[:n-1]
	if utf8.RuneCountInString(shorter) < minStemRunes {
		return base
	}
	return shorter
}

// stripConsonantY turns policy into polic (so it also matches policies);
// deploy keeps its y because a vowel precedes it.
func stripConsonantY(word string) string {
	n := len(word)
	if utf8.RuneCountInString(word) < minEnglishYRunes || word[n-1] != 'y' || isEnglishVowel(word[n-2]) {
		return word
	}
	return word[:n-1]
}

func isEnglishVowel(b byte) bool {
	return strings.IndexByte("aeiouy", b) >= 0
}
