package app

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// FindingSupersessionGap flags a newer record that reads like a replacement of
// an older current record but is not linked to it. It is report-only: a person
// or agent decides whether the newer record supersedes the older one.
const FindingSupersessionGap FindingKind = "supersession-gap"

// FindingStaleSuperseded flags a record whose validity ended without naming a
// replacement, so nothing current takes its place. Report-only.
const FindingStaleSuperseded FindingKind = "stale-superseded"

// supersessionGapMaxRecords caps how many records per kind are compared, like
// the near-duplicate check, so review stays fast on a large project.
const supersessionGapMaxRecords = 200

// supersedeCheckRecord is one record prepared for the supersede checks.
type supersedeCheckRecord struct {
	id       string
	title    string
	kind     string
	created  time.Time
	validity domain.Validity
}

// supersessionGapFindings compares every newer current record with the older
// current records of the same kind and reports the first plausible replacement
// that is not linked by superseded_by/supersedes.
func supersessionGapFindings(in reviewInputs) []Finding {
	var findings []Finding
	findings = append(findings, supersessionGaps(ports.RecordNote, noteSupersedeRecords(in.notes))...)
	findings = append(findings, supersessionGaps(ports.RecordDecision, decisionSupersedeRecords(in.decisions))...)
	findings = append(findings, supersessionGaps(ports.RecordAttempt, attemptSupersedeRecords(in.attempts))...)
	findings = append(findings, supersessionGaps(ports.RecordHandoff, handoffSupersedeRecords(in.handoffs))...)
	return findings
}

func noteSupersedeRecords(notes []domain.Note) []supersedeCheckRecord {
	records := make([]supersedeCheckRecord, 0, len(notes))
	for _, note := range notes {
		records = append(records, supersedeCheckRecord{id: note.ID, title: note.Title, kind: ports.RecordNote,
			created: note.CreatedAt, validity: note.Validity})
	}
	return records
}

func decisionSupersedeRecords(decisions []domain.Decision) []supersedeCheckRecord {
	records := make([]supersedeCheckRecord, 0, len(decisions))
	for _, decision := range decisions {
		records = append(records, supersedeCheckRecord{id: decision.ID, title: decision.Title, kind: ports.RecordDecision,
			created: decision.CreatedAt, validity: decision.Validity})
	}
	return records
}

func attemptSupersedeRecords(attempts []domain.Attempt) []supersedeCheckRecord {
	records := make([]supersedeCheckRecord, 0, len(attempts))
	for _, attempt := range attempts {
		records = append(records, supersedeCheckRecord{id: attempt.ID, title: attempt.Title, kind: ports.RecordAttempt,
			created: attempt.CreatedAt, validity: attempt.Validity})
	}
	return records
}

func handoffSupersedeRecords(handoffs []domain.Handoff) []supersedeCheckRecord {
	records := make([]supersedeCheckRecord, 0, len(handoffs))
	for _, handoff := range handoffs {
		records = append(records, supersedeCheckRecord{id: handoff.ID, title: SummarizeLine(handoff.Summary, 120), kind: ports.RecordHandoff,
			created: handoff.CreatedAt, validity: handoff.Validity})
	}
	return records
}

// supersessionGaps reports each newer record once, against its oldest plausible
// predecessor. Both records must be current: a historical pair is already
// superseded by definition.
func supersessionGaps(kind string, records []supersedeCheckRecord) []Finding {
	records = recentSupersedeRecords(records)
	var findings []Finding
	for newer := 1; newer < len(records); newer++ {
		if !records[newer].validity.IsCurrent() || strings.TrimSpace(records[newer].title) == "" {
			continue
		}
		for older := newer - 1; older >= 0; older-- {
			if !records[older].validity.IsCurrent() || strings.TrimSpace(records[older].title) == "" {
				continue
			}
			if records[newer].validity.Supersedes == records[older].id ||
				records[older].validity.SupersededBy == records[newer].id {
				continue
			}
			if !titleLineage(records[older].title, records[newer].title) {
				continue
			}
			findings = append(findings, newFinding(FindingSupersessionGap,
				fmt.Sprintf("%s %q looks replaced by newer %q but is not marked superseded; confirm with `brain record supersede %s --by %s`",
					kind, SummarizeLine(records[older].title, 80), SummarizeLine(records[newer].title, 80),
					records[older].id, records[newer].id),
				records[older].id+" -> "+records[newer].id))
			break
		}
	}
	return findings
}

// recentSupersedeRecords orders records oldest first and keeps the newest cap.
func recentSupersedeRecords(records []supersedeCheckRecord) []supersedeCheckRecord {
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].created.Equal(records[j].created) {
			return records[i].created.Before(records[j].created)
		}
		return records[i].id < records[j].id
	})
	if len(records) > supersessionGapMaxRecords {
		records = records[len(records)-supersessionGapMaxRecords:]
	}
	return records
}

// staleSupersededFindings reports records whose validity ended without a
// replacement. Such a record is no longer current, but nothing says what took
// its place.
func staleSupersededFindings(in reviewInputs, now time.Time) []Finding {
	var findings []Finding
	check := func(records []supersedeCheckRecord) {
		for _, record := range records {
			if record.validity.IsCurrent() || !record.validity.EndedAt(now) || strings.TrimSpace(record.validity.SupersededBy) != "" {
				continue
			}
			findings = append(findings, newFinding(FindingStaleSuperseded,
				fmt.Sprintf("%s %q expired at %s and names no replacement; supersede it or clear its valid_to",
					record.kind, SummarizeLine(record.title, 80), record.validity.ValidTo.UTC().Format(time.RFC3339)),
				record.id))
		}
	}
	check(noteSupersedeRecords(in.notes))
	check(decisionSupersedeRecords(in.decisions))
	check(attemptSupersedeRecords(in.attempts))
	check(handoffSupersedeRecords(in.handoffs))
	return findings
}

// titleLineageCoverage is how much of the shorter title's significant words the
// other title must repeat before both are treated as one topic. It is high on
// purpose: a replacement restates its subject, it does not just share the
// generic phrasing of "keep ... stable for ...".
const titleLineageCoverage = 0.9

// titleBoilerplate words say a record is a release or an announcement, not what
// it is about. They are dropped before two titles are compared, so "Release
// notes 0.6.0" and "0.6.0 shipped" can meet while different versions stay apart.
// The keys are tokenSet stems, not plain words.
var titleBoilerplate = map[string]bool{
	"releas": true, "notes": true, "note": true, "changel": true,
	"versio": true, "shippe": true, "publis": true, "announ": true,
	"draft": true, "final": true, "news": true,
}

// titleLineage reports whether two titles name one topic, which is what makes a
// newer record a plausible replacement. Different release versions are a series
// of records, not one record replacing another, so they never count.
func titleLineage(older, newer string) bool {
	olderVersions, newerVersions := titleVersions(older), titleVersions(newer)
	if strings.Join(olderVersions, " ") != strings.Join(newerVersions, " ") {
		return false
	}
	olderKey, newerKey := titleLineageKey(older), titleLineageKey(newer)
	if len(olderKey) == 0 || len(newerKey) == 0 {
		// Only release words are left: the shared version above is the topic.
		return len(olderKey) == 0 && len(newerKey) == 0 && len(olderVersions) > 0
	}
	shared := sharedTokenCount(olderKey, newerKey)
	smaller := min(len(olderKey), len(newerKey))
	if shared < minSharedTokens {
		return false
	}
	if shared == len(olderKey) && shared == len(newerKey) {
		return true
	}
	return float64(shared)/float64(smaller) >= titleLineageCoverage
}

// titleLineageKey is the sorted set of significant title tokens with the
// release boilerplate removed.
func titleLineageKey(title string) []string {
	tokens := tokenSet(title)
	key := make([]string, 0, len(tokens))
	for token := range tokens {
		if !titleBoilerplate[token] {
			key = append(key, token)
		}
	}
	sort.Strings(key)
	return key
}

// sharedTokenCount counts tokens of left that also appear in right.
func sharedTokenCount(left, right []string) int {
	set := make(map[string]struct{}, len(right))
	for _, token := range right {
		set[token] = struct{}{}
	}
	shared := 0
	for _, token := range left {
		if _, ok := set[token]; ok {
			shared++
		}
	}
	return shared
}

// attemptFailureWords are the words an attempt result uses to record that the
// approach failed. Review never assumes failure: a failure is reported only
// when the attempt says so.
var attemptFailureWords = map[string]bool{
	"fail": true, "failed": true, "fails": true, "failure": true, "failures": true,
	"broke": true, "broken": true, "breaks": true,
	"crash": true, "crashed": true, "crashes": true,
	"errored":    true,
	"regression": true, "regressed": true, "reverted": true,
	"abandoned": true, "rejected": true, "unusable": true, "ineffective": true,
	"timeout": true, "deadlock": true,
}

// attemptFailurePhrases are the multi-word ways a result records a failure.
// They are matched against the normalized text, where punctuation is a space.
var attemptFailurePhrases = []string{
	"did not work", "does not work", "didn t work", "doesn t work",
	"not working", "no effect", "did not help", "does not help",
	"timed out", "не сработал", "не работа", "не помог",
}

// attemptFailurePrefixes cover inflected Russian forms of the failure words.
var attemptFailurePrefixes = []string{"ошиб", "провал", "сломал", "неудач", "регресс"}

// attemptRecordsFailure reports whether an attempt explicitly records a failed
// result. An empty result falls back to the reason, which says why an approach
// was kept or abandoned.
func attemptRecordsFailure(attempt domain.Attempt) bool {
	text := attempt.Result
	if strings.TrimSpace(text) == "" {
		text = attempt.Reason
	}
	normalized := normalizeDupText(text)
	if normalized == "" {
		return false
	}
	for _, phrase := range attemptFailurePhrases {
		if strings.Contains(normalized, phrase) {
			return true
		}
	}
	for _, word := range strings.Fields(normalized) {
		if attemptFailureWords[word] {
			return true
		}
		for _, prefix := range attemptFailurePrefixes {
			if strings.HasPrefix(word, prefix) {
				return true
			}
		}
	}
	return false
}
