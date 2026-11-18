package app

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// FindingSuspectedContradiction flags two current records of one project that
// read as if they disagree about the same subject. It is computed at review
// time by deterministic text heuristics: no model call, no write, no automatic
// supersede. A person or agent opens both records and accepts or rejects one.
const FindingSuspectedContradiction FindingKind = "suspected-contradiction"

// Contradiction reason codes. They name the heuristic that fired and appear in
// the finding message, so a reviewer can tell why the pair was flagged.
const (
	ContradictionNegation = "negation"
	ContradictionAntonym  = "antonym"
	ContradictionNumeric  = "numeric"
	ContradictionTemporal = "temporal"
)

const (
	// contradictionOverlapGate is the share of the shorter record's stemmed
	// tokens the other record must repeat before a pair is compared at all.
	// Two records under the gate are about different subjects, so no
	// heuristic may flag them, however many trigger words they share.
	contradictionOverlapGate = 0.4
	// contradictionMinSharedTokens is the absolute count the gate also
	// requires, so one shared word never makes two records a pair.
	contradictionMinSharedTokens = 2
	// contradictionNegationGate is the sentence overlap a negation asymmetry
	// needs. A negation word anywhere in a long body is not a claim; the
	// sentence that carries it must restate the other sentence's subject.
	contradictionNegationGate = 0.55
	// contradictionNegationJaccard is the stricter same-claim test for a
	// negation pair. Long release notes share project vocabulary without
	// stating the same claim, so the two sentences must also be largely
	// symmetric (Jaccard over the stemmed tokens).
	contradictionNegationJaccard = 0.5
	// contradictionTemporalSentenceGate is the overlap between the sentence
	// carrying a temporal marker and a sentence of the other record. "instead"
	// is a generic connective word, so the shared subject must be strong.
	contradictionTemporalSentenceGate = 0.6
	// contradictionMaxRecords caps how many recent decisions and notes are
	// compared. The newest wins, so review stays fast on a large project.
	contradictionMaxRecords = 200
	// contradictionMaxLabelRunes bounds each quoted title in a message.
	contradictionMaxLabelRunes = 80
	// contradictionMaxBodyRunes skips records whose body is longer than this:
	// a broad auto-summary is context, not a single claim.
	contradictionMaxBodyRunes = 3000
	// contradictionMaxSentences bounds the sentences parsed from one record.
	contradictionMaxSentences = 400
)

// contradictionBroadTitle matches generated session summaries. Their body
// aggregates many topics, so any heuristic run over them produces pairs that
// look contradictory but are just different parts of one summary.
var contradictionBroadTitle = regexp.MustCompile(`(?i)^session(?:\s+summary)?\b`)

// contradictionRecord is one decision or note prepared for the pair checks.
// tokenSet and the normalized text are computed once, so every pair comparison
// is a set intersection plus small string scans.
type contradictionRecord struct {
	id        string
	projectID string
	kind      string // decision or note, for the finding message
	title     string
	createdAt time.Time
	validity  domain.Validity
	text      string // normalized title + body
	tokens    map[string]struct{}
	negated   bool
	sentences []contradictionSentence
}

// contradictionSentence is one sentence prepared for the sentence-scoped
// negation and temporal checks. A broad body is never compared as one blob:
// the sentence that carries the trigger word must restate the other sentence.
type contradictionSentence struct {
	text      string
	tokens    map[string]struct{}
	negated   bool
	marker    string
	hasMarker bool
}

// contradictionHit is one heuristic match with its human-readable reason.
type contradictionHit struct {
	reason string
	detail string
}

// contradictionFindings compares the project's recent decisions and notes and
// reports each differing pair once. Stub records are skipped, records whose
// validity ended are skipped unless includeHistory is set, and a pair already
// linked by supersedes/superseded_by is never a suspected contradiction: the
// owner already decided it.
//
// The review calls this with includeHistory false, so superseded and
// invalidated records stay out of the default report.
func contradictionFindings(in reviewInputs, now time.Time, includeHistory bool) []Finding {
	records := contradictionRecords(in, now, includeHistory)
	if len(records) < 2 {
		return nil
	}
	var findings []Finding
	for older := 0; older < len(records)-1; older++ {
		for newer := older + 1; newer < len(records); newer++ {
			left, right := records[older], records[newer]
			if left.projectID != "" && right.projectID != "" && left.projectID != right.projectID {
				continue
			}
			if contradictionLinked(left, right) {
				continue
			}
			if contradictionDifferentVersions(left, right) {
				continue
			}
			shared, share := contradictionOverlap(left.tokens, right.tokens)
			if shared < contradictionMinSharedTokens || share < contradictionOverlapGate {
				continue
			}
			hit, ok := contradictionBetween(left, right)
			if !ok {
				continue
			}
			findings = append(findings, newContradictionFinding(left, right, hit))
		}
	}
	return findings
}

// contradictionRecords gathers the comparable records of the review input,
// oldest first, and keeps the newest contradictionMaxRecords. Decisions that
// the author rejected are never beliefs and are always skipped; a superseded
// decision is history and is skipped unless the caller asked for history.
func contradictionRecords(in reviewInputs, now time.Time, includeHistory bool) []contradictionRecord {
	records := make([]contradictionRecord, 0, len(in.decisions)+len(in.notes))
	add := func(id, projectID, kind, title, body string, created time.Time, validity domain.Validity) {
		if contradictionBroadRecord(title, body) {
			return
		}
		if IsStubRecord(kind, title, body) {
			return
		}
		if !includeHistory {
			filter := domain.ValidityFilter{}
			if !filter.Allows(validity, created, now) {
				return
			}
		}
		text := normalizeDupText(title + " " + body)
		if strings.TrimSpace(text) == "" {
			return
		}
		records = append(records, contradictionRecord{
			id: id, projectID: projectID, kind: kind, title: strings.TrimSpace(title),
			createdAt: created, validity: validity, text: text,
			tokens: tokenSet(text), negated: contradictionHasNegation(text),
			sentences: contradictionSentences(title, body),
		})
	}
	for _, note := range in.notes {
		add(note.ID, note.ProjectID, ports.RecordNote, note.Title, note.Body, note.CreatedAt, note.Validity)
	}
	for _, decision := range in.decisions {
		if decision.Status == domain.DecisionRejected {
			continue
		}
		if decision.Status == domain.DecisionSuperseded && !includeHistory {
			continue
		}
		body := joinText(decision.Context,
			strings.Join(decision.Alternatives, " "), strings.Join(decision.Consequences, " "))
		add(decision.ID, decision.ProjectID, ports.RecordDecision, decision.Title, body, decision.CreatedAt, decision.Validity)
	}
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].createdAt.Equal(records[j].createdAt) {
			return records[i].createdAt.Before(records[j].createdAt)
		}
		return records[i].id < records[j].id
	})
	if len(records) > contradictionMaxRecords {
		records = records[len(records)-contradictionMaxRecords:]
	}
	return records
}

// contradictionBroadRecord reports whether a record is a broad session
// summary rather than a claim: its title is a generated "Session ..." label or
// its body is longer than contradictionMaxBodyRunes. A summary aggregates many
// topics, so any pair of its sentences can look contradictory while describing
// different work.
func contradictionBroadRecord(title, body string) bool {
	if contradictionBroadTitle.MatchString(strings.TrimSpace(title)) {
		return true
	}
	return utf8.RuneCountInString(body) > contradictionMaxBodyRunes
}

// contradictionSentences splits a record into normalized sentences. Sentence
// boundaries are newlines and terminal punctuation; a period inside a word
// ("brain.ps1", "0.26.0") does not split.
func contradictionSentences(title, body string) []contradictionSentence {
	raw := strings.TrimSpace(title)
	if body = strings.TrimSpace(body); body != "" {
		if raw != "" {
			raw += ". "
		}
		raw += body
	}
	if raw == "" {
		return nil
	}
	var sentences []contradictionSentence
	for _, part := range contradictionSplitSentences(raw) {
		text := normalizeDupText(part)
		if text == "" {
			continue
		}
		marker, marked := contradictionTemporalMarker(text)
		sentences = append(sentences, contradictionSentence{
			text: text, tokens: tokenSet(text), negated: contradictionHasNegation(text),
			marker: marker, hasMarker: marked,
		})
		if len(sentences) >= contradictionMaxSentences {
			break
		}
	}
	return sentences
}

// contradictionSplitSentences cuts raw text at newlines, semicolons and
// sentence-ending punctuation. A period followed by a non-space stays part of
// the token, so version numbers and file names survive.
func contradictionSplitSentences(raw string) []string {
	runes := []rune(raw)
	parts := make([]string, 0, 16)
	start := 0
	for i := 0; i < len(runes); i++ {
		char := runes[i]
		boundary := char == '\n' || char == '\r' || char == ';' || char == '!' || char == '?'
		if char == '.' && (i+1 == len(runes) || unicode.IsSpace(runes[i+1])) {
			boundary = true
		}
		if !boundary {
			continue
		}
		if part := strings.TrimSpace(string(runes[start:i])); part != "" {
			parts = append(parts, part)
		}
		start = i + 1
	}
	if part := strings.TrimSpace(string(runes[start:])); part != "" {
		parts = append(parts, part)
	}
	return parts
}

// contradictionOverlap is the overlap coefficient over the smaller token set:
// the share of the shorter record's words the other record repeats.
func contradictionOverlap(a, b map[string]struct{}) (int, float64) {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(a) == 0 {
		return 0, 0
	}
	shared := 0
	for token := range a {
		if _, ok := b[token]; ok {
			shared++
		}
	}
	return shared, float64(shared) / float64(len(a))
}

// contradictionJaccard is |a∩b|/|a∪b|; 0 when both are empty. Unlike the overlap
// coefficient it punishes one long sentence that merely mentions the other.
func contradictionJaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for token := range a {
		if _, ok := b[token]; ok {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// contradictionLinked reports whether the pair is already tied by explicitness:
// one record supersedes the other. Such a pair is a decision, not a suspicion,
// and must not repeat as a contradiction finding even when history is reviewed.
func contradictionLinked(a, b contradictionRecord) bool {
	return a.validity.Supersedes == b.id || b.validity.Supersedes == a.id ||
		a.validity.SupersededBy == b.id || b.validity.SupersededBy == a.id
}

// contradictionDifferentVersions reports whether both records name different
// dotted release versions. A series of releases is history, not one belief
// changing its value: "v0.8.0 OIDC failed" and "v0.8.1 OIDC works" describe
// different releases even when the wording is nearly identical.
func contradictionDifferentVersions(a, b contradictionRecord) bool {
	left := strings.Join(titleVersions(a.title), " ")
	right := strings.Join(titleVersions(b.title), " ")
	return left != "" && right != "" && left != right
}

// contradictionBetween runs the heuristics in a fixed order so one pair yields
// at most one finding, with the most specific reason that fired. Temporal
// override goes first because "no longer"/"deprecated" also reads as negation,
// and the override is the more useful label for the owner.
func contradictionBetween(older, newer contradictionRecord) (contradictionHit, bool) {
	if hit, ok := temporalContradiction(older, newer); ok {
		return hit, true
	}
	if hit, ok := negationContradiction(older, newer); ok {
		return hit, true
	}
	if hit, ok := antonymContradiction(older, newer); ok {
		return hit, true
	}
	if hit, ok := numericContradiction(older, newer); ok {
		return hit, true
	}
	return contradictionHit{}, false
}

func newContradictionFinding(older, newer contradictionRecord, hit contradictionHit) Finding {
	message := fmt.Sprintf("suspected contradiction (%s) between %s %q and %s %q: %s; open both records and accept or reject one",
		hit.reason, contradictionKindLabel(older), contradictionLabel(older),
		contradictionKindLabel(newer), contradictionLabel(newer), hit.detail)
	return newFinding(FindingSuspectedContradiction, message, older.id+" <-> "+newer.id)
}

// contradictionKindLabel names the record kind and id, so the message always
// carries both ids even when a title is empty.
func contradictionKindLabel(record contradictionRecord) string {
	return fmt.Sprintf("%s %s", record.kind, record.id)
}

// contradictionLabel is the displayed title, falling back to the normalized
// text when a record has no title.
func contradictionLabel(record contradictionRecord) string {
	if record.title != "" {
		return SummarizeLine(record.title, contradictionMaxLabelRunes)
	}
	return SummarizeLine(record.text, contradictionMaxLabelRunes)
}

// contradictionTemporalMarkers are the words that say "the older statement no
// longer holds". They are prefixes where inflection matters: replaced,
// replacement, deprecated, deprecation, заменён, замена.
var contradictionTemporalMarkers = []string{
	"no longer", "instead", "replac", "deprecat", "obsolet", "sunset", "phased out",
	"больше не", "вместо", "замен", "устарел", "неактуал",
}

// temporalContradiction flags a newer record that overrides an older one. Only
// the newer side may carry the marker: an older deprecation followed by a
// plain newer claim is a reinstatement question, not an override. The marker
// sentence must restate a sentence of the older record; a shared document
// topic is not enough, because "instead" and "deprecated" are generic words.
func temporalContradiction(older, newer contradictionRecord) (contradictionHit, bool) {
	if _, olderMarked := contradictionTemporalMarker(older.text); olderMarked {
		return contradictionHit{}, false
	}
	for _, marked := range newer.sentences {
		if !marked.hasMarker {
			continue
		}
		for _, other := range older.sentences {
			shared, share := contradictionOverlap(marked.tokens, other.tokens)
			if shared < contradictionMinSharedTokens || share < contradictionTemporalSentenceGate {
				continue
			}
			return contradictionHit{
				reason: ContradictionTemporal,
				detail: fmt.Sprintf("the newer sentence marks the older claim as overridden (%q)", marked.marker),
			}, true
		}
	}
	return contradictionHit{}, false
}

func contradictionTemporalMarker(text string) (string, bool) {
	for _, marker := range contradictionTemporalMarkers {
		if strings.Contains(text, marker) {
			return marker, true
		}
	}
	return "", false
}

// contradictionNegationWords are whole words that negate a claim. Russian "не"
// is a separate word too; normalizeDupText keeps it, while tokenSet would drop
// it as too short, which is why negation is detected on the normalized text.
var contradictionNegationWords = map[string]bool{
	"not": true, "no": true, "never": true, "none": true, "nothing": true,
	"cannot": true, "without": true, "neither": true, "nor": true,
	"не": true, "нет": true, "никогда": true, "нельзя": true, "без": true,
}

// contradictionContractionStems are the first halves of "isn't", "doesn't",
// "won't" and similar, which normalizeDupText splits into two words.
var contradictionContractionStems = map[string]bool{
	"isn": true, "aren": true, "wasn": true, "weren": true,
	"doesn": true, "don": true, "didn": true,
	"hasn": true, "haven": true, "hadn": true,
	"won": true, "wouldn": true, "couldn": true, "shouldn": true, "mustn": true,
}

func contradictionHasNegation(text string) bool {
	words := strings.Fields(text)
	for i, word := range words {
		if contradictionNegationWords[word] {
			return true
		}
		if i+1 < len(words) && words[i+1] == "t" && contradictionContractionStems[word] {
			return true
		}
	}
	return false
}

// negationContradiction flags one record that negates a statement another
// record affirms. The check is sentence-scoped: a negation word anywhere in a
// long body is not a claim, so the negated sentence must share at least two
// content terms and 55% of the shorter sentence with an affirmative sentence
// of the other record. One negation plus the opposite term of an explicit pair
// ("not enabled" against "disabled") is agreement and is skipped.
func negationContradiction(older, newer contradictionRecord) (contradictionHit, bool) {
	if !negationSentencePair(older, newer) && !negationSentencePair(newer, older) {
		return contradictionHit{}, false
	}
	return contradictionHit{
		reason: ContradictionNegation,
		detail: "one record affirms the statement and the other negates it (not/no/never/не) in a sentence about the same subject",
	}, true
}

// negationSentencePair compares each negated sentence of negatedRecord with the
// affirmative sentences of plainRecord.
func negationSentencePair(negatedRecord, plainRecord contradictionRecord) bool {
	for _, negated := range negatedRecord.sentences {
		if !negated.negated {
			continue
		}
		for _, plain := range plainRecord.sentences {
			if plain.negated {
				continue
			}
			shared, share := contradictionOverlap(negated.tokens, plain.tokens)
			if shared < contradictionMinSharedTokens || share < contradictionNegationGate {
				continue
			}
			if contradictionJaccard(negated.tokens, plain.tokens) < contradictionNegationJaccard {
				continue
			}
			if contradictionSentenceOppositeTerms(negated, plain) {
				continue
			}
			return true
		}
	}
	return false
}

// contradictionSentenceOppositeTerms reports whether the two sentences use the
// opposite sides of one explicit antonym pair. With a negation on one side that
// is agreement, not contradiction.
func contradictionSentenceOppositeTerms(a, b contradictionSentence) bool {
	for _, pair := range contradictionAntonyms {
		aLeft, aRight := contradictionHasRoots(a.tokens, pair.left), contradictionHasRoots(a.tokens, pair.right)
		bLeft, bRight := contradictionHasRoots(b.tokens, pair.left), contradictionHasRoots(b.tokens, pair.right)
		if aLeft && bRight && !aRight && !bLeft {
			return true
		}
		if aRight && bLeft && !aLeft && !bRight {
			return true
		}
	}
	return false
}

// contradictionAntonym is one explicit opposite pair. Roots are matched as
// prefixes of the stemmed tokens, so enabled/enables/включено all match while
// "disable" never matches the "enable" root.
type contradictionAntonym struct {
	label string
	left  []string
	right []string
}

// contradictionAntonyms is the small closed list review understands. It is
// deliberately explicit: no thesaurus and no model, so the same pair always
// produces the same reason.
var contradictionAntonyms = []contradictionAntonym{
	{label: "enable/disable", left: []string{"enabl", "включ"}, right: []string{"disabl", "отключ"}},
	{label: "allow/deny", left: []string{"allow", "разреш"}, right: []string{"deni", "deny", "запрет", "запрещ"}},
	{label: "increase/decrease", left: []string{"increa", "увелич"}, right: []string{"decrea", "уменьш", "снижа", "снизи"}},
	{label: "keep/remove", left: []string{"keep", "остав"}, right: []string{"remov", "удал"}},
	{label: "before/after", left: []string{"befor", "перед"}, right: []string{"after", "после"}},
}

// antonymContradiction flags a pair that uses opposite terms of one explicit
// pair. A record that contains both sides ("enable or disable") is ambiguous
// and never counts, and one negation over an opposite term ("not enabled"
// against "disabled") reads as agreement rather than contradiction.
func antonymContradiction(older, newer contradictionRecord) (contradictionHit, bool) {
	for _, pair := range contradictionAntonyms {
		if !contradictionOppositeTerms(older, newer, pair) {
			continue
		}
		if older.negated != newer.negated {
			continue
		}
		return contradictionHit{
			reason: ContradictionAntonym,
			detail: fmt.Sprintf("the records use opposite terms of the pair %s", pair.label),
		}, true
	}
	return contradictionHit{}, false
}

// contradictionOppositeTerms reports whether one record uses the left term of a
// pair and the other only its right term. A record that names both sides is
// ambiguous and never counts.
func contradictionOppositeTerms(a, b contradictionRecord, pair contradictionAntonym) bool {
	aLeft, aRight := contradictionHasRoots(a.tokens, pair.left), contradictionHasRoots(a.tokens, pair.right)
	bLeft, bRight := contradictionHasRoots(b.tokens, pair.left), contradictionHasRoots(b.tokens, pair.right)
	if aLeft && bRight && !aRight && !bLeft {
		return true
	}
	return aRight && bLeft && !aLeft && !bRight
}

func contradictionHasRoots(tokens map[string]struct{}, roots []string) bool {
	for token := range tokens {
		for _, root := range roots {
			if strings.HasPrefix(token, root) {
				return true
			}
		}
	}
	return false
}

// contradictionUnitAliases maps a written unit to its canonical name. Only
// units that name a measurable quantity are listed; a bare number with no unit
// is not a value change, which keeps versions, issue numbers and ports out.
var contradictionUnitAliases = map[string]string{
	"second": "seconds", "seconds": "seconds", "sec": "seconds", "secs": "seconds",
	"minute": "minutes", "minutes": "minutes", "min": "minutes", "mins": "minutes",
	"hour": "hours", "hours": "hours", "hr": "hours", "hrs": "hours",
	"day": "days", "days": "days", "d": "days",
	"week": "weeks", "weeks": "weeks", "w": "weeks",
	"month": "months", "months": "months",
	"year": "years", "years": "years",
	"retry": "retries", "retries": "retries",
	"attempt": "attempts", "attempts": "attempts",
	"token": "tokens", "tokens": "tokens",
	"record": "records", "records": "records",
	"byte": "bytes", "bytes": "bytes", "kb": "kilobytes", "mb": "megabytes",
	"gb": "gigabytes", "tb": "terabytes",
	"день": "days", "дня": "days", "дней": "days",
	"час": "hours", "часа": "hours", "часов": "hours",
	"минута": "minutes", "минуты": "minutes", "минут": "minutes",
	"неделя": "weeks", "недели": "weeks", "недель": "weeks",
	"месяц": "months", "месяца": "months", "месяцев": "months",
	"год": "years", "года": "years", "лет": "years",
	"попытка": "attempts", "попытки": "attempts", "попыток": "attempts",
	"токен": "tokens", "токена": "tokens", "токенов": "tokens",
}

// numericContradiction flags the same measured unit carrying different values
// in two records of one subject: "90 days" against "180 days".
func numericContradiction(older, newer contradictionRecord) (contradictionHit, bool) {
	olderNumbers := contradictionNumbers(older.text)
	newerNumbers := contradictionNumbers(newer.text)
	if len(olderNumbers) == 0 || len(newerNumbers) == 0 {
		return contradictionHit{}, false
	}
	units := make([]string, 0, len(olderNumbers)+len(newerNumbers))
	for unit := range olderNumbers {
		units = append(units, unit)
	}
	for unit := range newerNumbers {
		if _, ok := olderNumbers[unit]; !ok {
			units = append(units, unit)
		}
	}
	sort.Strings(units)
	for _, unit := range units {
		olderValues, newerValues := sortedUnique(olderNumbers[unit]), sortedUnique(newerNumbers[unit])
		if len(olderValues) == 0 || len(newerValues) == 0 || slices.Equal(olderValues, newerValues) {
			continue
		}
		// One record restating every value of the other plus an extra mention
		// is not a value change ("30 minutes" against "30 minutes and 5
		// minutes").
		if contradictionSubset(olderValues, newerValues) || contradictionSubset(newerValues, olderValues) {
			continue
		}
		return contradictionHit{
			reason: ContradictionNumeric,
			detail: fmt.Sprintf("the same subject carries %s in one record and %s in the other",
				contradictionNumberList(olderValues, unit), contradictionNumberList(newerValues, unit)),
		}, true
	}
	return contradictionHit{}, false
}

// contradictionNumbers pairs each number with the unit that follows it, either
// attached ("90d") or as the next word ("90 days").
func contradictionNumbers(text string) map[string][]string {
	words := strings.Fields(text)
	numbers := map[string][]string{}
	for i, word := range words {
		value, rest := contradictionSplitNumber(word)
		if value == "" {
			continue
		}
		unit := ""
		if rest != "" {
			unit = contradictionUnitAliases[rest]
		}
		if unit == "" && i+1 < len(words) {
			unit = contradictionUnitAliases[words[i+1]]
		}
		if unit == "" {
			continue
		}
		numbers[unit] = append(numbers[unit], value)
	}
	return numbers
}

// contradictionSplitNumber separates the leading ASCII digits of a word from
// the rest, so both "180" and "180d" parse.
func contradictionSplitNumber(word string) (string, string) {
	end := 0
	for end < len(word) && word[end] >= '0' && word[end] <= '9' {
		end++
	}
	if end == 0 {
		return "", ""
	}
	return word[:end], word[end:]
}

func sortedUnique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := slices.Clone(values)
	sort.Strings(out)
	return slices.Compact(out)
}

// contradictionSubset reports whether every value appears in other.
func contradictionSubset(values, other []string) bool {
	if len(values) > len(other) {
		return false
	}
	set := make(map[string]struct{}, len(other))
	for _, value := range other {
		set[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

func contradictionNumberList(values []string, unit string) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = value + " " + unit
	}
	return strings.Join(parts, ", ")
}
