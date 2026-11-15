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
			if !similarWith(records[older].title, records[newer].title, minSharedTokens) {
				continue
			}
			findings = append(findings, Finding{
				Kind: FindingSupersessionGap,
				Message: fmt.Sprintf("%s %q looks replaced by newer %q but is not marked superseded; confirm with `brain record supersede %s --by %s`",
					kind, SummarizeLine(records[older].title, 80), SummarizeLine(records[newer].title, 80),
					records[older].id, records[newer].id),
				Evidence: records[older].id + " -> " + records[newer].id,
			})
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
			findings = append(findings, Finding{
				Kind: FindingStaleSuperseded,
				Message: fmt.Sprintf("%s %q expired at %s and names no replacement; supersede it or clear its valid_to",
					record.kind, SummarizeLine(record.title, 80), record.validity.ValidTo.UTC().Format(time.RFC3339)),
				Evidence: record.id,
			})
		}
	}
	check(noteSupersedeRecords(in.notes))
	check(decisionSupersedeRecords(in.decisions))
	check(attemptSupersedeRecords(in.attempts))
	check(handoffSupersedeRecords(in.handoffs))
	return findings
}
