package app

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// RenderIdeaCalendar renders dated ideas as one iCalendar file. It is the one
// renderer shared by `brain idea ical`, the local API and the desktop export, so
// the DATE/DATE-TIME and VALARM rules cannot drift between surfaces.
//
// The caller decides which statuses to include: this function renders every
// dated idea it receives, so an include-done export can reuse it. Undated ideas
// have no VEVENT. All-day ideas use DATE; timed ideas use the UTC instant
// because the stored value keeps a fixed offset, not a zone name.
//
// Every VEVENT carries a due-time VALARM and, when RemindBeforeMinutes > 0, a
// lead VALARM before it. A timed due-time alarm is relative (-PT0M); an all-day
// idea has no midnight instant, so both its lead and due-time alarms are
// absolute at 09:00 in the machine's local zone (minus the lead), converted to
// UTC. This matches the desktop scheduler, so importing either export alerts at
// the same instants.
func RenderIdeaCalendar(ideas []domain.Idea, now time.Time) string {
	ordered := make([]domain.Idea, 0, len(ideas))
	for _, idea := range ideas {
		if idea.DueDate == "" && (idea.DueAt == nil || idea.DueAt.IsZero()) {
			continue
		}
		ordered = append(ordered, idea)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		left, leftOK := ideaCalendarDue(ordered[i])
		right, rightOK := ideaCalendarDue(ordered[j])
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && !left.Equal(right) {
			return left.Before(right)
		}
		return ordered[i].ID < ordered[j].ID
	})
	var b strings.Builder
	writeICSLine(&b, "BEGIN:VCALENDAR")
	writeICSLine(&b, "VERSION:2.0")
	writeICSLine(&b, "PRODID:-//LV24 Brain//Ideas 1.0//EN")
	writeICSLine(&b, "CALSCALE:GREGORIAN")
	for _, idea := range ordered {
		writeICSLine(&b, "BEGIN:VEVENT")
		writeICSLine(&b, "UID:"+escapeICSText(idea.ID))
		writeICSLine(&b, "DTSTAMP:"+ideaCalendarStamp(idea, now))
		if idea.DueAt != nil && !idea.DueAt.IsZero() {
			writeICSLine(&b, "DTSTART:"+idea.DueAt.UTC().Format("20060102T150405Z"))
		} else {
			writeICSLine(&b, "DTSTART;VALUE=DATE:"+strings.ReplaceAll(idea.DueDate, "-", ""))
		}
		writeICSLine(&b, "SUMMARY:"+escapeICSText(idea.Title))
		if body := strings.TrimSpace(idea.Body); body != "" {
			writeICSLine(&b, "DESCRIPTION:"+escapeICSText(body))
		}
		if idea.RemindBeforeMinutes != nil && *idea.RemindBeforeMinutes > 0 {
			lead := time.Duration(*idea.RemindBeforeMinutes) * time.Minute
			if idea.DueAt != nil && !idea.DueAt.IsZero() {
				writeICSAlarm(&b, fmt.Sprintf("TRIGGER:-PT%dM", *idea.RemindBeforeMinutes), idea.Title)
			} else if anchor, ok := ideaCalendarDue(idea); ok {
				// An all-day DTSTART is midnight, so a relative lead would fire
				// nine hours early. Anchor the lead to the 09:00 local due
				// instant instead.
				leadAt := anchor.Add(-lead).UTC().Format("20060102T150405Z")
				writeICSAlarm(&b, "TRIGGER;VALUE=DATE-TIME:"+leadAt, idea.Title)
			}
		}
		if idea.DueAt != nil && !idea.DueAt.IsZero() {
			writeICSAlarm(&b, "TRIGGER:-PT0M", idea.Title)
		} else if anchor, ok := ideaCalendarDue(idea); ok {
			writeICSAlarm(&b, "TRIGGER;VALUE=DATE-TIME:"+anchor.UTC().Format("20060102T150405Z"), idea.Title)
		}
		writeICSLine(&b, "END:VEVENT")
	}
	writeICSLine(&b, "END:VCALENDAR")
	return b.String()
}

// ideaCalendarDue is the sort anchor of one dated idea: the timed instant, or
// 09:00 local on an all-day date.
func ideaCalendarDue(idea domain.Idea) (time.Time, bool) {
	if idea.DueAt != nil && !idea.DueAt.IsZero() {
		return *idea.DueAt, true
	}
	if idea.DueDate == "" {
		return time.Time{}, false
	}
	date, err := time.ParseInLocation(domain.IdeaDateLayout, idea.DueDate, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(date.Year(), date.Month(), date.Day(), 9, 0, 0, 0, time.Local), true
}

// ideaCalendarStamp is the DTSTAMP of a VEVENT. The record revision is
// deterministic; now is only a fallback for a hand-written file without one.
func ideaCalendarStamp(idea domain.Idea, now time.Time) string {
	stamp := idea.UpdatedAt
	if stamp.IsZero() {
		stamp = idea.CreatedAt
	}
	if stamp.IsZero() {
		stamp = now
	}
	return stamp.UTC().Format("20060102T150405Z")
}

// writeICSAlarm writes one DISPLAY alarm with a complete TRIGGER property line
// (relative or absolute).
func writeICSAlarm(b *strings.Builder, trigger, title string) {
	writeICSLine(b, "BEGIN:VALARM")
	writeICSLine(b, trigger)
	writeICSLine(b, "ACTION:DISPLAY")
	writeICSLine(b, "DESCRIPTION:"+escapeICSText(title))
	writeICSLine(b, "END:VALARM")
}

// writeICSLine writes one CRLF line, folded at 75 octets as RFC 5545 requires.
// Folding never splits a UTF-8 sequence and counts the continuation space.
func writeICSLine(b *strings.Builder, line string) {
	const limit = 75
	count := 0
	for _, r := range line {
		size := utf8.RuneLen(r)
		if count+size > limit {
			b.WriteString("\r\n ")
			count = 1
		}
		b.WriteRune(r)
		count += size
	}
	b.WriteString("\r\n")
}

// escapeICSText escapes the TEXT values of an iCalendar file. Newlines become
// the literal \n, and backslash, semicolon and comma are escaped.
func escapeICSText(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, ";", "\\;")
	value = strings.ReplaceAll(value, ",", "\\,")
	value = strings.ReplaceAll(value, "\r\n", "\\n")
	value = strings.ReplaceAll(value, "\r", "\\n")
	return strings.ReplaceAll(value, "\n", "\\n")
}
