package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// IdeaDateLayout is the all-day due date format of an idea: a local calendar
// date with zero padding, never a timestamp.
const IdeaDateLayout = "2006-01-02"

const (
	// MaxIdeaTitleChars bounds an idea title in Unicode code points.
	MaxIdeaTitleChars = 200
	// MaxIdeaTags bounds the number of tags of one idea.
	MaxIdeaTags = 10
	// MaxIdeaTagChars bounds one tag in Unicode code points.
	MaxIdeaTagChars = 40
	// MaxIdeaRemindBeforeMinutes is one week of lead time. A larger lead is
	// almost always a mistake, not a reminder.
	MaxIdeaRemindBeforeMinutes = 7 * 24 * 60

	maxIdeaSourceIDLength = 256
	maxIdeaHarnessLength  = 64
)

// IdeaStatus is the lifecycle state of an idea.
//
//	open    actionable now
//	doing   being worked on
//	someday parked, not actionable now
//	done    completed; completed_at records when
//	dropped abandoned; completion_note records why
type IdeaStatus string

const (
	IdeaOpen    IdeaStatus = "open"
	IdeaDoing   IdeaStatus = "doing"
	IdeaDone    IdeaStatus = "done"
	IdeaDropped IdeaStatus = "dropped"
	IdeaSomeday IdeaStatus = "someday"
)

// Valid reports whether s is one of the five idea statuses. The empty value is
// not valid: readers treat it as open, but a writer must choose a status.
func (s IdeaStatus) Valid() bool {
	switch s {
	case IdeaOpen, IdeaDoing, IdeaDone, IdeaDropped, IdeaSomeday:
		return true
	default:
		return false
	}
}

// Terminal reports whether the status closes the idea. Done and dropped are
// terminal; someday is parked, not finished, and can still be completed.
func (s IdeaStatus) Terminal() bool {
	return s == IdeaDone || s == IdeaDropped
}

// ParseIdeaStatus parses a status name exactly as stored. It trims surrounding
// whitespace but does not fold case, so a typo is reported instead of guessed.
func ParseIdeaStatus(value string) (IdeaStatus, error) {
	status := IdeaStatus(strings.TrimSpace(value))
	if !status.Valid() {
		return "", NewValidationError("idea.status", fmt.Sprintf("unknown status %q", value))
	}
	return status, nil
}

// IdeaPriority is the optional urgency of an idea. The empty value reads as
// normal.
type IdeaPriority string

const (
	IdeaPriorityLow    IdeaPriority = "low"
	IdeaPriorityNormal IdeaPriority = "normal"
	IdeaPriorityHigh   IdeaPriority = "high"
)

// Valid reports whether p is one of the three idea priorities.
func (p IdeaPriority) Valid() bool {
	switch p {
	case IdeaPriorityLow, IdeaPriorityNormal, IdeaPriorityHigh:
		return true
	default:
		return false
	}
}

// ParseIdeaPriority parses a priority name. An empty value is the default,
// normal; an unknown value is an error.
func ParseIdeaPriority(value string) (IdeaPriority, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return IdeaPriorityNormal, nil
	}
	priority := IdeaPriority(trimmed)
	if !priority.Valid() {
		return "", NewValidationError("idea.priority", fmt.Sprintf("unknown priority %q", value))
	}
	return priority, nil
}

// IdeaSource records where an idea came from. Unlike SourceRef it is
// idea-specific and optional: manual and desktop captures need no id, and the
// source project may differ from the idea's project (capture in project A,
// tag to project B).
type IdeaSource struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Harness string `json:"harness,omitempty"`
}

// Idea source types. Session, note, decision and commit sources identify a
// record or a session; manual and desktop captures may have no id.
const (
	IdeaSourceSession  = "session"
	IdeaSourceNote     = "note"
	IdeaSourceDecision = "decision"
	IdeaSourceCommit   = "commit"
	IdeaSourceManual   = "manual"
	IdeaSourceDesktop  = "desktop"
)

func validIdeaSourceType(value string) bool {
	switch value {
	case IdeaSourceSession, IdeaSourceNote, IdeaSourceDecision, IdeaSourceCommit,
		IdeaSourceManual, IdeaSourceDesktop:
		return true
	default:
		return false
	}
}

// Validate checks the source fields. A source type is required; an id is
// required for session, note, decision and commit sources and optional for
// manual and desktop captures.
func (s IdeaSource) Validate() error {
	return s.validate("idea.source")
}

func (s IdeaSource) validate(field string) error {
	if s.Type != strings.TrimSpace(s.Type) {
		return NewValidationError(field+".type", "must not contain surrounding whitespace")
	}
	if !validIdeaSourceType(s.Type) {
		return NewValidationError(field+".type", fmt.Sprintf("unknown source type %q", s.Type))
	}
	if s.ID != strings.TrimSpace(s.ID) {
		return NewValidationError(field+".id", "must not contain surrounding whitespace")
	}
	if s.ID == "" {
		if s.Type != IdeaSourceManual && s.Type != IdeaSourceDesktop {
			return NewValidationError(field+".id", fmt.Sprintf("is required for a %s source", s.Type))
		}
	} else if err := validateIdeaSourceText(field+".id", s.ID, maxIdeaSourceIDLength); err != nil {
		return err
	}
	if s.Harness != strings.TrimSpace(s.Harness) {
		return NewValidationError(field+".harness", "must not contain surrounding whitespace")
	}
	if s.Harness != "" {
		if err := validateIdeaSourceText(field+".harness", s.Harness, maxIdeaHarnessLength); err != nil {
			return err
		}
	}
	return nil
}

func validateIdeaSourceText(field, value string, maxLength int) error {
	if len(value) > maxLength {
		return NewValidationError(field, fmt.Sprintf("must be at most %d bytes", maxLength))
	}
	if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return NewValidationError(field, "must be valid UTF-8 without control characters")
	}
	return nil
}

// Idea is one agent-first TODO record. It lives in
// projects/<project_id>/ideas/<idea-id>.md, one file per idea, and is modeled
// on Decision: a status enum with in-place updates plus an optional due time.
//
// Exactly one of DueAt (timed, RFC3339 with offset) and DueDate (all-day,
// YYYY-MM-DD) may be set. DueDate is a local calendar date and never shifts
// with travel; DueAt keeps its offset. An absent due means the idea has no
// date. RemindBeforeMinutes is a concrete lead; absent or zero means notify at
// the due time only.
type Idea struct {
	ID        string       `json:"id"`
	ProjectID string       `json:"projectId"`
	Title     string       `json:"title"`
	Body      string       `json:"body,omitempty"`
	Status    IdeaStatus   `json:"status"`
	Priority  IdeaPriority `json:"priority,omitempty"`
	// DueAt is a timed due instant. Nil when the idea is all-day or undated.
	DueAt *time.Time `json:"dueAt,omitempty"`
	// DueDate is an all-day local date in IdeaDateLayout. Empty when the idea
	// is timed or undated.
	DueDate string `json:"dueDate,omitempty"`
	// RemindBeforeMinutes is the lead before the effective due instant. Nil or
	// zero means no lead.
	RemindBeforeMinutes *int        `json:"remindBeforeMinutes,omitempty"`
	Tags                []string    `json:"tags,omitempty"`
	Source              *IdeaSource `json:"source,omitempty"`
	CompletedAt         *time.Time  `json:"completedAt,omitempty"`
	CompletionNote      string      `json:"completionNote,omitempty"`
	Provenance
	Validity
	MergeInfo
	Taxonomy
	TopicHistory
	Relations RelationList `json:"relations,omitempty"`
	ReviewSchedule
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Validate checks the idea fields and the embedded record metadata. An empty
// status reads as open and an empty priority reads as normal; both may be left
// for the store to default. CompletedAt is only allowed on a done idea; the
// writer sets it when the idea is completed.
func (i Idea) Validate() error {
	if err := ValidateRequired("idea.projectId", i.ProjectID); err != nil {
		return err
	}
	if err := validateIdeaTitle(i.Title); err != nil {
		return err
	}
	if err := validateIdeaText("idea.body", i.Body); err != nil {
		return err
	}
	if i.Status != "" && !i.Status.Valid() {
		return NewValidationError("idea.status", fmt.Sprintf("unknown status %q", i.Status))
	}
	if i.Priority != "" && !i.Priority.Valid() {
		return NewValidationError("idea.priority", fmt.Sprintf("unknown priority %q", i.Priority))
	}
	if err := i.validateDue(); err != nil {
		return err
	}
	if i.RemindBeforeMinutes != nil &&
		(*i.RemindBeforeMinutes < 0 || *i.RemindBeforeMinutes > MaxIdeaRemindBeforeMinutes) {
		return NewValidationError("idea.remindBeforeMinutes",
			fmt.Sprintf("must be between 0 and %d", MaxIdeaRemindBeforeMinutes))
	}
	if err := validateIdeaTags(i.Tags); err != nil {
		return err
	}
	if i.Source != nil {
		if err := i.Source.validate("idea.source"); err != nil {
			return err
		}
	}
	if err := i.validateCompletion(); err != nil {
		return err
	}
	if err := validateIdeaText("idea.completionNote", i.CompletionNote); err != nil {
		return err
	}
	if err := i.Provenance.Validate(); err != nil {
		return err
	}
	if err := i.Validity.Validate("idea.validity", i.CreatedAt, i.ID); err != nil {
		return err
	}
	if err := i.MergeInfo.Validate("idea.merge", i.ID); err != nil {
		return err
	}
	if err := i.Relations.Validate("idea.relations", i.ID); err != nil {
		return err
	}
	if err := i.Taxonomy.Validate("idea.taxonomy", i.ID); err != nil {
		return err
	}
	if err := ValidateHistory("idea.history", i.History); err != nil {
		return err
	}
	return nil
}

func validateIdeaTitle(title string) error {
	if strings.TrimSpace(title) == "" {
		return NewValidationError("idea.title", "is required")
	}
	if utf8.RuneCountInString(title) > MaxIdeaTitleChars {
		return NewValidationError("idea.title",
			fmt.Sprintf("must be at most %d characters", MaxIdeaTitleChars))
	}
	if !utf8.ValidString(title) {
		return NewValidationError("idea.title", "must be valid UTF-8")
	}
	if strings.IndexFunc(title, unicode.IsControl) >= 0 {
		return NewValidationError("idea.title", "must not contain control characters")
	}
	return nil
}

// validateIdeaText accepts multiline body text but rejects NUL and invalid
// UTF-8, which would make the stored Markdown unreadable.
func validateIdeaText(field, value string) error {
	if !utf8.ValidString(value) {
		return NewValidationError(field, "must be valid UTF-8")
	}
	if strings.ContainsRune(value, '\x00') {
		return NewValidationError(field, "must not contain NUL")
	}
	return nil
}

func (i Idea) validateDue() error {
	if i.DueAt != nil && i.DueDate != "" {
		return NewValidationError("idea.due", "due_at and due_date are mutually exclusive")
	}
	if i.DueAt != nil && i.DueAt.IsZero() {
		return NewValidationError("idea.dueAt", "must be a real timestamp")
	}
	if i.DueDate != "" {
		if _, err := time.Parse(IdeaDateLayout, i.DueDate); err != nil {
			return NewValidationError("idea.dueDate",
				fmt.Sprintf("%q is not a %s date", i.DueDate, IdeaDateLayout))
		}
	}
	return nil
}

func validateIdeaTags(tags []string) error {
	if len(tags) > MaxIdeaTags {
		return NewValidationError("idea.tags", fmt.Sprintf("must be at most %d tags", MaxIdeaTags))
	}
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" {
			return NewValidationError("idea.tags", "entries must not be empty")
		}
		if utf8.RuneCountInString(tag) > MaxIdeaTagChars {
			return NewValidationError("idea.tags",
				fmt.Sprintf("each tag must be at most %d characters", MaxIdeaTagChars))
		}
		if !utf8.ValidString(tag) || strings.IndexFunc(tag, unicode.IsControl) >= 0 {
			return NewValidationError("idea.tags", "entries must be valid UTF-8 without control characters")
		}
	}
	return nil
}

func (i Idea) validateCompletion() error {
	status := i.Status
	if status == "" {
		status = IdeaOpen
	}
	if i.CompletedAt == nil {
		return nil
	}
	if status != IdeaDone {
		return NewValidationError("idea.completedAt",
			fmt.Sprintf("is only allowed on a done idea, not %q", status))
	}
	if i.CompletedAt.IsZero() {
		return NewValidationError("idea.completedAt", "must be a real timestamp")
	}
	return nil
}

// CanTransition reports whether a status change from one idea status to
// another is allowed:
//
//	open <-> doing
//	open, doing, someday -> done, dropped, someday
//	done, dropped -> open (reopen)
//	someday -> open (unpark)
//	any status -> itself (idempotent write)
//
// An empty from reads as open; an empty or unknown to is refused. A status a
// user set explicitly through `idea edit --status` follows the same rules as
// the done/reopen/drop verbs.
func CanTransition(from, to IdeaStatus) bool {
	if from == "" {
		from = IdeaOpen
	}
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return true
	}
	switch from {
	case IdeaOpen:
		return to == IdeaDoing || to == IdeaSomeday || to.Terminal()
	case IdeaDoing:
		return to == IdeaOpen || to == IdeaSomeday || to.Terminal()
	case IdeaSomeday:
		return to == IdeaOpen || to.Terminal()
	case IdeaDone, IdeaDropped:
		return to == IdeaOpen
	default:
		return false
	}
}

// NewIdeaID returns idea-<YYYY-MM-DD>-<slug>-<ULID>, the id and file name stem
// of a new idea. The date is the UTC date, matching every other record id; the
// slug is normalized with Slugify, so a human title is accepted as well as an
// already slugified string. A failure of the OS random source is returned as an
// error instead of producing an id with a broken or reused suffix.
func NewIdeaID(date time.Time, slug string) (string, error) {
	ulid, err := NewULID(date)
	if err != nil {
		return "", fmt.Errorf("domain: generate idea id: %w", err)
	}
	return NewRecordID("idea", date, Slugify(slug), ulid), nil
}
