package app

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// FindingNearDuplicate flags two records of the same kind in one project
// whose text is nearly the same. It is report-only: the person or agent picks
// which record to keep.
const FindingNearDuplicate FindingKind = "near-duplicate"

const (
	// duplicateShingleWords is the word count of one shingle.
	duplicateShingleWords = 3
	// duplicateThreshold is the Jaccard similarity of the word shingles two
	// records need to be reported as near-duplicates.
	duplicateThreshold = 0.8
	// duplicateAutoThreshold applies when either record is an automatic
	// session handoff. Those are written by the session-end hook on every
	// session: they share the "[auto]" prefix and copy the state's next
	// action, so they are compared on the summary alone and need a higher
	// similarity, otherwise routine sessions about one topic would flood the
	// review.
	duplicateAutoThreshold = 0.9
	// duplicateTitleThreshold is the body similarity that is enough when the
	// normalized titles are identical.
	duplicateTitleThreshold = 0.6
	// duplicateMinShingles is the shingle count below which a text is too
	// short for shingles to mean anything; such records match only when their
	// normalized text is equal.
	duplicateMinShingles = 3
	// duplicateMaxRecords caps the records compared per kind to the most
	// recent ones, so a project with thousands of records stays fast.
	duplicateMaxRecords = 500
	// duplicateAutoPrefix marks a handoff written by the session-end hook.
	duplicateAutoPrefix = "[auto]"
)

// dupRecord is one record prepared for comparison.
type dupRecord struct {
	id        string
	created   time.Time
	title     string // normalized
	body      string // normalized
	text      string // normalized title + body
	shingles  map[string]struct{}
	bodyShing map[string]struct{}
	auto      bool
}

// nearDuplicateFindings compares the live records of each kind with each other
// (never across kinds; the inputs are already one project's live records, so
// trashed ones are not among them). Decisions count only while proposed or
// accepted. Each newer record is reported once, against its most similar
// older record, so a group of copies yields one finding per extra copy.
func nearDuplicateFindings(in reviewInputs) []Finding {
	var findings []Finding
	findings = append(findings, duplicatesOfKind(ports.RecordNote, noteDupRecords(in.notes))...)
	findings = append(findings, duplicatesOfKind(ports.RecordDecision, decisionDupRecords(in.decisions))...)
	findings = append(findings, duplicatesOfKind(ports.RecordAttempt, attemptDupRecords(in.attempts))...)
	findings = append(findings, duplicatesOfKind(ports.RecordHandoff, handoffDupRecords(in.handoffs))...)
	return findings
}

func noteDupRecords(notes []domain.Note) []dupRecord {
	records := make([]dupRecord, 0, len(notes))
	for _, note := range notes {
		records = append(records, newDupRecord(note.ID, note.CreatedAt, note.Title, note.Body, false))
	}
	return records
}

func decisionDupRecords(decisions []domain.Decision) []dupRecord {
	records := make([]dupRecord, 0, len(decisions))
	for _, decision := range decisions {
		if decision.Status != "" && decision.Status != domain.DecisionProposed && decision.Status != domain.DecisionAccepted {
			continue
		}
		body := strings.Join([]string{decision.Context, strings.Join(decision.Alternatives, " "), strings.Join(decision.Consequences, " ")}, " ")
		records = append(records, newDupRecord(decision.ID, decision.CreatedAt, decision.Title, body, false))
	}
	return records
}

func attemptDupRecords(attempts []domain.Attempt) []dupRecord {
	records := make([]dupRecord, 0, len(attempts))
	for _, attempt := range attempts {
		body := strings.Join([]string{attempt.Hypothesis, attempt.Result, attempt.Reason}, " ")
		records = append(records, newDupRecord(attempt.ID, attempt.CreatedAt, attempt.Title, body, false))
	}
	return records
}

// handoffDupRecords compares manual handoffs on summary and next action, and
// automatic ones on the summary alone (their next action is a copy of the
// state's, identical across sessions).
func handoffDupRecords(handoffs []domain.Handoff) []dupRecord {
	records := make([]dupRecord, 0, len(handoffs))
	for _, handoff := range handoffs {
		summary := strings.TrimSpace(handoff.Summary)
		if strings.HasPrefix(summary, duplicateAutoPrefix) {
			records = append(records, newDupRecord(handoff.ID, handoff.CreatedAt, "", strings.TrimPrefix(summary, duplicateAutoPrefix), true))
			continue
		}
		records = append(records, newDupRecord(handoff.ID, handoff.CreatedAt, "", summary+" "+handoff.NextAction, false))
	}
	return records
}

func newDupRecord(id string, created time.Time, title, body string, auto bool) dupRecord {
	normTitle, normBody := normalizeDupText(title), normalizeDupText(body)
	text := strings.TrimSpace(normTitle + " " + normBody)
	return dupRecord{
		id: id, created: created, title: normTitle, body: normBody, text: text,
		shingles: wordShingles(text), bodyShing: wordShingles(normBody), auto: auto,
	}
}

// normalizeDupText lowercases, folds ё to е, drops punctuation and Markdown
// markup (every rune that is not a letter or digit) and collapses whitespace.
func normalizeDupText(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return strings.ReplaceAll(strings.Join(words, " "), "ё", "е")
}

// wordShingles returns the set of consecutive duplicateShingleWords-word
// sequences of a normalized text.
func wordShingles(text string) map[string]struct{} {
	words := strings.Fields(text)
	set := map[string]struct{}{}
	for i := 0; i+duplicateShingleWords <= len(words); i++ {
		set[strings.Join(words[i:i+duplicateShingleWords], " ")] = struct{}{}
	}
	return set
}

// jaccard is |a∩b| / |a∪b|; 0 when both are empty.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) > len(b) {
		a, b = b, a
	}
	shared := 0
	for shingle := range a {
		if _, ok := b[shingle]; ok {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// dupSimilarity scores a pair; ok reports whether it is a near-duplicate.
func dupSimilarity(a, b dupRecord) (float64, bool) {
	if a.text == "" || b.text == "" {
		return 0, false
	}
	if a.text == b.text {
		return 1, true
	}
	if len(a.shingles) < duplicateMinShingles || len(b.shingles) < duplicateMinShingles {
		return 0, false
	}
	score := jaccard(a.shingles, b.shingles)
	threshold := duplicateThreshold
	if a.auto || b.auto {
		threshold = duplicateAutoThreshold
	}
	if score >= threshold {
		return score, true
	}
	if a.title != "" && a.title == b.title {
		if a.body == b.body {
			return 1, true
		}
		if len(a.bodyShing) >= duplicateMinShingles && len(b.bodyShing) >= duplicateMinShingles {
			if bodyScore := jaccard(a.bodyShing, b.bodyShing); bodyScore >= duplicateTitleThreshold {
				return math.Max(score, bodyScore), true
			}
		}
	}
	return score, false
}

// duplicatesOfKind finds near-duplicates among records of one kind. Candidate
// pairs come from three buckets that every reportable pair falls into: equal
// normalized text, equal normalized title, or a shared shingle from the
// rarest-first prefix of each shingle set (prefix filtering: two sets with
// Jaccard >= t must share one of the first |s|-ceil(t|s|)+1 rarest shingles).
// That keeps the work far below all pairs when records are mostly distinct.
func duplicatesOfKind(kind string, records []dupRecord) []Finding {
	records = recentDupRecords(records)
	if len(records) < 2 {
		return nil
	}
	candidates := dupCandidates(records)
	var findings []Finding
	for newer := 1; newer < len(records); newer++ {
		best, bestScore := -1, 0.0
		for _, older := range candidates[newer] {
			if score, ok := dupSimilarity(records[older], records[newer]); ok && score > bestScore {
				best, bestScore = older, score
			}
		}
		if best >= 0 {
			findings = append(findings, duplicateFinding(kind, records[best], records[newer], bestScore))
		}
	}
	return findings
}

// recentDupRecords orders records oldest first and keeps the most recent
// duplicateMaxRecords.
func recentDupRecords(records []dupRecord) []dupRecord {
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].created.Equal(records[j].created) {
			return records[i].created.Before(records[j].created)
		}
		return records[i].id < records[j].id
	})
	if len(records) > duplicateMaxRecords {
		records = records[len(records)-duplicateMaxRecords:]
	}
	return records
}

// dupCandidates maps each record index to the older indexes worth comparing.
func dupCandidates(records []dupRecord) map[int][]int {
	frequency := map[string]int{}
	for _, record := range records {
		for shingle := range record.shingles {
			frequency[shingle]++
		}
	}
	byText, byTitle, byShingle := map[string][]int{}, map[string][]int{}, map[string][]int{}
	candidates := map[int][]int{}
	for index, record := range records {
		seen := map[int]struct{}{}
		add := func(others []int) {
			for _, other := range others {
				if _, ok := seen[other]; !ok {
					seen[other] = struct{}{}
					candidates[index] = append(candidates[index], other)
				}
			}
		}
		if record.text != "" {
			add(byText[record.text])
			byText[record.text] = append(byText[record.text], index)
		}
		if record.title != "" {
			add(byTitle[record.title])
			byTitle[record.title] = append(byTitle[record.title], index)
		}
		for _, shingle := range shinglePrefix(record.shingles, frequency) {
			add(byShingle[shingle])
			byShingle[shingle] = append(byShingle[shingle], index)
		}
	}
	return candidates
}

// shinglePrefix returns the rarest shingles of a set, as many as prefix
// filtering needs for duplicateThreshold (the lowest shingle threshold).
func shinglePrefix(set map[string]struct{}, frequency map[string]int) []string {
	if len(set) < duplicateMinShingles {
		return nil
	}
	ordered := make([]string, 0, len(set))
	for shingle := range set {
		ordered = append(ordered, shingle)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if frequency[ordered[i]] != frequency[ordered[j]] {
			return frequency[ordered[i]] < frequency[ordered[j]]
		}
		return ordered[i] < ordered[j]
	})
	size := len(ordered) - int(math.Ceil(duplicateThreshold*float64(len(ordered)))) + 1
	return ordered[:min(size, len(ordered))]
}

func duplicateFinding(kind string, older, newer dupRecord, score float64) Finding {
	fix := fmt.Sprintf("keep one: `brain record trash %s --yes` (reversible with `brain record restore`)", older.id)
	if kind == ports.RecordDecision {
		fix = fmt.Sprintf("`brain record supersede %s --by %s`", older.id, newer.id)
	}
	percent := int(math.Floor(score * 100))
	return Finding{
		Kind:     FindingNearDuplicate,
		Message:  fmt.Sprintf("%s %s and %s look like near-duplicates (%d%% similar)", kind, older.id, newer.id, percent),
		Evidence: quote(SummarizeLine(newer.text, 80)),
		Fix:      fix,
	}
}

// doctorDuplicates counts near-duplicate records per project. It warns, never
// fails: duplicates are a curation task (`brain review`), not damage.
func (a *App) doctorDuplicates(ctx context.Context) DoctorCheck {
	if a.deps.Store == nil || a.cfg.Mode == config.ModeServer {
		return DoctorCheck{Name: "duplicates", Status: doctorOK, Message: "not checked in this mode"}
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return DoctorCheck{Name: "duplicates", Status: doctorFail, Message: err.Error()}
	}
	var counts []string
	total := 0
	for _, project := range projects {
		in, err := a.duplicateInputs(ctx, project.ID)
		if err != nil {
			return DoctorCheck{Name: "duplicates", Status: doctorFail, Message: project.ID + ": " + err.Error()}
		}
		if found := len(nearDuplicateFindings(in)); found > 0 {
			counts = append(counts, fmt.Sprintf("%s %d", project.ID, found))
			total += found
		}
	}
	if total == 0 {
		return DoctorCheck{Name: "duplicates", Status: doctorOK, Message: fmt.Sprintf("no near-duplicate records (%d projects)", len(projects))}
	}
	return DoctorCheck{
		Name:    "duplicates",
		Status:  doctorWarn,
		Message: fmt.Sprintf("%d near-duplicate records (%s); see brain review --project <id>", total, strings.Join(counts, ", ")),
	}
}

// duplicateInputs loads only the records the near-duplicate check reads.
func (a *App) duplicateInputs(ctx context.Context, projectID string) (reviewInputs, error) {
	var in reviewInputs
	var err error
	if in.notes, err = a.listNotes(ctx, projectID); err != nil {
		return in, err
	}
	if in.decisions, err = a.listDecisions(ctx, projectID); err != nil {
		return in, err
	}
	if in.attempts, err = a.listAttempts(ctx, projectID); err != nil {
		return in, err
	}
	in.handoffs, err = a.listHandoffs(ctx, projectID)
	return in, err
}
