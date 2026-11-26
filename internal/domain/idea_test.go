package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testIdea() Idea {
	return Idea{
		ID:        "idea-2026-11-26-calendar-dark-theme-01jcs6example000000000000",
		ProjectID: "lv24-brain",
		Title:     "Тёмная тема для календаря",
		Status:    IdeaOpen,
		Priority:  IdeaPriorityNormal,
		CreatedAt: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC),
	}
}

func ptrTime(at time.Time) *time.Time { return &at }

func ptrInt(value int) *int { return &value }

func TestIdeaStatusConstantsAndValidity(t *testing.T) {
	statuses := map[IdeaStatus]string{
		IdeaOpen:    "open",
		IdeaDoing:   "doing",
		IdeaDone:    "done",
		IdeaDropped: "dropped",
		IdeaSomeday: "someday",
	}
	for status, want := range statuses {
		if string(status) != want {
			t.Fatalf("status constant = %q, want %q", status, want)
		}
		if !status.Valid() {
			t.Fatalf("%q is not valid", status)
		}
	}
	for _, status := range []IdeaStatus{"", "blocked", "Open"} {
		if status.Valid() {
			t.Fatalf("%q is valid, want invalid", status)
		}
	}
}

func TestParseIdeaStatus(t *testing.T) {
	for input, want := range map[string]IdeaStatus{
		"open":    IdeaOpen,
		"doing":   IdeaDoing,
		"done":    IdeaDone,
		"dropped": IdeaDropped,
		"someday": IdeaSomeday,
		"  done ": IdeaDone,
	} {
		got, err := ParseIdeaStatus(input)
		if err != nil {
			t.Fatalf("ParseIdeaStatus(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseIdeaStatus(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"", "DONE", "blocked"} {
		if _, err := ParseIdeaStatus(input); !errors.Is(err, ErrValidation) {
			t.Fatalf("ParseIdeaStatus(%q) error = %v, want validation", input, err)
		}
	}
}

func TestParseIdeaPriority(t *testing.T) {
	for input, want := range map[string]IdeaPriority{
		"low":    IdeaPriorityLow,
		"normal": IdeaPriorityNormal,
		"high":   IdeaPriorityHigh,
		" high ": IdeaPriorityHigh,
		"":       IdeaPriorityNormal,
	} {
		got, err := ParseIdeaPriority(input)
		if err != nil {
			t.Fatalf("ParseIdeaPriority(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseIdeaPriority(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"urgent", "HIGH"} {
		if _, err := ParseIdeaPriority(input); !errors.Is(err, ErrValidation) {
			t.Fatalf("ParseIdeaPriority(%q) error = %v, want validation", input, err)
		}
	}
}

func TestIdeaValidate(t *testing.T) {
	longTitle := strings.Repeat("я", 200)
	tooLongTitle := strings.Repeat("я", 201)
	longTag := strings.Repeat("ж", 40)
	tooLongTag := strings.Repeat("ж", 41)

	cases := []struct {
		name      string
		mutate    func(*Idea)
		wantField string
	}{
		{"minimal", func(*Idea) {}, ""},
		{"empty status reads as open", func(i *Idea) { i.Status = "" }, ""},
		{"empty priority reads as normal", func(i *Idea) { i.Priority = "" }, ""},
		{"all statuses", func(i *Idea) { i.Status = IdeaSomeday }, ""},
		{"all priorities", func(i *Idea) { i.Priority = IdeaPriorityHigh }, ""},
		{"missing project", func(i *Idea) { i.ProjectID = "" }, "idea.projectId"},
		{"missing title", func(i *Idea) { i.Title = "" }, "idea.title"},
		{"blank title", func(i *Idea) { i.Title = "   " }, "idea.title"},
		{"title 200 runes", func(i *Idea) { i.Title = longTitle }, ""},
		{"title 201 runes", func(i *Idea) { i.Title = tooLongTitle }, "idea.title"},
		{"invalid title utf8", func(i *Idea) { i.Title = "bad\xff" }, "idea.title"},
		{"title with nul", func(i *Idea) { i.Title = "bad\x00title" }, "idea.title"},
		{"invalid body utf8", func(i *Idea) { i.Body = "bad\xff" }, "idea.body"},
		{"body with nul", func(i *Idea) { i.Body = "bad\x00body" }, "idea.body"},
		{"unknown status", func(i *Idea) { i.Status = "blocked" }, "idea.status"},
		{"unknown priority", func(i *Idea) { i.Priority = "urgent" }, "idea.priority"},
		{"both due fields", func(i *Idea) {
			i.DueAt = ptrTime(time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC))
			i.DueDate = "2026-11-27"
		}, "idea.due"},
		{"invalid due date", func(i *Idea) { i.DueDate = "2026-02-30" }, "idea.dueDate"},
		{"unpadded due date", func(i *Idea) { i.DueDate = "2026-1-5" }, "idea.dueDate"},
		{"due date with time", func(i *Idea) { i.DueDate = "2026-11-27T18:00:00Z" }, "idea.dueDate"},
		{"valid all-day due date", func(i *Idea) { i.DueDate = "2026-11-27" }, ""},
		{"valid leap day", func(i *Idea) { i.DueDate = "2028-02-29" }, ""},
		{"non-leap february 29", func(i *Idea) { i.DueDate = "2026-02-29" }, "idea.dueDate"},
		{"zero due at", func(i *Idea) { i.DueAt = ptrTime(time.Time{}) }, "idea.dueAt"},
		{"valid timed due", func(i *Idea) {
			i.DueAt = ptrTime(time.Date(2026, 11, 27, 18, 0, 0, 0, time.FixedZone("MSK", 3*3600)))
		}, ""},
		{"remind below zero", func(i *Idea) { i.RemindBeforeMinutes = ptrInt(-1) }, "idea.remindBeforeMinutes"},
		{"remind above max", func(i *Idea) { i.RemindBeforeMinutes = ptrInt(10081) }, "idea.remindBeforeMinutes"},
		{"remind zero", func(i *Idea) { i.RemindBeforeMinutes = ptrInt(0) }, ""},
		{"remind max", func(i *Idea) { i.RemindBeforeMinutes = ptrInt(10080) }, ""},
		{"ten tags", func(i *Idea) {
			i.Tags = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
		}, ""},
		{"eleven tags", func(i *Idea) {
			i.Tags = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}
		}, "idea.tags"},
		{"empty tag", func(i *Idea) { i.Tags = []string{"ui", "  "} }, "idea.tags"},
		{"tag 40 runes", func(i *Idea) { i.Tags = []string{longTag} }, ""},
		{"tag 41 runes", func(i *Idea) { i.Tags = []string{tooLongTag} }, "idea.tags"},
		{"tag with nul", func(i *Idea) { i.Tags = []string{"ui\x00"} }, "idea.tags"},
		{"completed at on open", func(i *Idea) {
			i.CompletedAt = ptrTime(time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC))
		}, "idea.completedAt"},
		{"zero completed at", func(i *Idea) {
			i.Status = IdeaDone
			i.CompletedAt = ptrTime(time.Time{})
		}, "idea.completedAt"},
		{"done with completion", func(i *Idea) {
			i.Status = IdeaDone
			i.CompletedAt = ptrTime(time.Date(2026, 11, 27, 18, 30, 0, 0, time.UTC))
			i.CompletionNote = "проверено"
		}, ""},
		{"done without completion time", func(i *Idea) { i.Status = IdeaDone }, ""},
		{"dropped with completion note", func(i *Idea) {
			i.Status = IdeaDropped
			i.CompletionNote = "не актуально"
		}, ""},
		{"someday with completed at", func(i *Idea) {
			i.Status = IdeaSomeday
			i.CompletedAt = ptrTime(time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC))
		}, "idea.completedAt"},
		{"completion note with nul", func(i *Idea) {
			i.Status = IdeaDone
			i.CompletionNote = "bad\x00note"
		}, "idea.completionNote"},
		{"valid session source", func(i *Idea) {
			i.Source = &IdeaSource{Type: "session", ID: "ses_123", Harness: "opencode"}
		}, ""},
		{"source without type", func(i *Idea) {
			i.Source = &IdeaSource{ID: "ses_123"}
		}, "idea.source.type"},
		{"source unknown type", func(i *Idea) {
			i.Source = &IdeaSource{Type: "telegram", ID: "1"}
		}, "idea.source.type"},
		{"session source without id", func(i *Idea) {
			i.Source = &IdeaSource{Type: "session"}
		}, "idea.source.id"},
		{"manual source without id", func(i *Idea) {
			i.Source = &IdeaSource{Type: "manual"}
		}, ""},
		{"desktop source with id", func(i *Idea) {
			i.Source = &IdeaSource{Type: "desktop", ID: "window-1"}
		}, ""},
		{"source id surrounding whitespace", func(i *Idea) {
			i.Source = &IdeaSource{Type: "note", ID: " note-1 "}
		}, "idea.source.id"},
		{"source harness with control", func(i *Idea) {
			i.Source = &IdeaSource{Type: "session", ID: "ses_1", Harness: "open\ncode"}
		}, "idea.source.harness"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idea := testIdea()
			tc.mutate(&idea)
			err := idea.Validate()
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate() = %v, want ErrValidation", err)
			}
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("Validate() = %T, want *ValidationError", err)
			}
			if !strings.HasPrefix(validation.Field, tc.wantField) {
				t.Fatalf("Validate() field = %q, want prefix %q", validation.Field, tc.wantField)
			}
		})
	}
}

func TestIdeaDueAtKeepsItsOffset(t *testing.T) {
	idea := testIdea()
	msk := time.FixedZone("MSK", 3*3600)
	idea.DueAt = ptrTime(time.Date(2026, 11, 27, 18, 0, 0, 0, msk))
	if err := idea.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
	if got := idea.DueAt.Format(time.RFC3339); got != "2026-11-27T18:00:00+03:00" {
		t.Fatalf("due_at = %q, want the original offset", got)
	}
}

func TestIdeaAllDayDueDateIsAStableString(t *testing.T) {
	idea := testIdea()
	idea.DueDate = "2026-11-27"
	if err := idea.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
	if idea.DueDate != "2026-11-27" {
		t.Fatalf("due_date changed to %q", idea.DueDate)
	}
	if idea.DueAt != nil {
		t.Fatalf("all-day idea gained a due_at: %v", idea.DueAt)
	}
}

func TestIdeaSourceValidate(t *testing.T) {
	valid := []IdeaSource{
		{Type: "session", ID: "ses_1"},
		{Type: "note", ID: "note-2026-11-26-x"},
		{Type: "decision", ID: "decision-2026-11-26-x"},
		{Type: "commit", ID: "abcdef1"},
		{Type: "manual", ID: "manual-1"},
		{Type: "desktop"},
		{Type: "manual", Harness: "opencode"},
	}
	for _, source := range valid {
		if err := source.Validate(); err != nil {
			t.Fatalf("IdeaSource%+v.Validate() = %v, want nil", source, err)
		}
	}
	invalid := []IdeaSource{
		{},
		{Type: "telegram", ID: "1"},
		{Type: "session"},
		{Type: "commit"},
		{Type: "note", ID: " note-1"},
		{Type: "session", ID: "ses_1", Harness: strings.Repeat("x", 65)},
	}
	for _, source := range invalid {
		if err := source.Validate(); err == nil {
			t.Fatalf("IdeaSource%+v.Validate() = nil, want error", source)
		} else if !errors.Is(err, ErrValidation) {
			t.Fatalf("IdeaSource%+v.Validate() = %v, want ErrValidation", source, err)
		}
	}
}

func TestCanTransition(t *testing.T) {
	statuses := []IdeaStatus{IdeaOpen, IdeaDoing, IdeaDone, IdeaDropped, IdeaSomeday}
	allowed := map[IdeaStatus]map[IdeaStatus]bool{
		IdeaOpen: {
			IdeaDoing: true, IdeaDone: true, IdeaDropped: true, IdeaSomeday: true,
		},
		IdeaDoing: {
			IdeaOpen: true, IdeaDone: true, IdeaDropped: true, IdeaSomeday: true,
		},
		IdeaSomeday: {
			// someday -> open is the unpark edge; without it a parked idea
			// could never be started again.
			IdeaOpen: true, IdeaDone: true, IdeaDropped: true,
		},
		IdeaDone:    {IdeaOpen: true},
		IdeaDropped: {IdeaOpen: true},
	}
	for _, from := range statuses {
		for _, to := range statuses {
			want := from == to || allowed[from][to]
			if got := CanTransition(from, to); got != want {
				t.Fatalf("CanTransition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	if !CanTransition("", IdeaDoing) {
		t.Fatal("an empty status must read as open")
	}
	if CanTransition(IdeaOpen, "") {
		t.Fatal("an empty target status must be refused")
	}
	if CanTransition("blocked", IdeaOpen) || CanTransition(IdeaOpen, "blocked") {
		t.Fatal("unknown statuses must be refused")
	}
}

func TestNewIdeaID(t *testing.T) {
	// 01:30 at UTC+03 is the previous UTC day; the id date is the UTC date,
	// matching every other record id.
	msk := time.FixedZone("MSK", 3*3600)
	at := time.Date(2026, 11, 27, 1, 30, 0, 0, msk)
	id, err := NewIdeaID(at, "Calendar Dark Theme")
	if err != nil {
		t.Fatalf("NewIdeaID: %v", err)
	}
	const prefix = "idea-2026-11-26-calendar-dark-theme-"
	if !strings.HasPrefix(id, prefix) {
		t.Fatalf("NewIdeaID = %q, want prefix %q", id, prefix)
	}
	if suffix := strings.TrimPrefix(id, prefix); len(suffix) != RecordIDULIDLength || !IsULID(suffix) {
		t.Fatalf("NewIdeaID suffix = %q, want a %d-character ULID", suffix, RecordIDULIDLength)
	}
	if !IsRecordID("idea", id) {
		t.Fatalf("NewIdeaID produced an id IsRecordID rejects: %q", id)
	}
	if IsRecordID("decision", id) {
		t.Fatalf("idea id %q is also a decision id", id)
	}
}

func TestNewIdeaIDSlugifiesAndIsUnique(t *testing.T) {
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	id, err := NewIdeaID(at, "Тёмная тема / календарь")
	if err != nil {
		t.Fatalf("NewIdeaID: %v", err)
	}
	if !strings.HasPrefix(id, "idea-2026-11-26-temnaya-tema-kalendar-") {
		t.Fatalf("NewIdeaID = %q, want a transliterated slug", id)
	}
	if !IsRecordID("idea", id) {
		t.Fatalf("NewIdeaID produced an id IsRecordID rejects: %q", id)
	}
	other, err := NewIdeaID(at, "Тёмная тема / календарь")
	if err != nil {
		t.Fatalf("NewIdeaID second call: %v", err)
	}
	if id == other {
		t.Fatalf("two calls returned the same id %q", id)
	}
}

func TestNewIdeaIDEmptySlugFallsBack(t *testing.T) {
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	id, err := NewIdeaID(at, "   ")
	if err != nil {
		t.Fatalf("NewIdeaID: %v", err)
	}
	if !strings.HasPrefix(id, "idea-2026-11-26-item-") {
		t.Fatalf("NewIdeaID = %q, want the item slug fallback", id)
	}
	if !IsRecordID("idea", id) {
		t.Fatalf("NewIdeaID produced an id IsRecordID rejects: %q", id)
	}
}
