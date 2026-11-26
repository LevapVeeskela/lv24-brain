package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// Idea brief limits from the contract: at most three items and 700 characters
// in the session block. The caller may ask for fewer or more (1..10) items, but
// the character budget is fixed so the block never crowds out the handoff.
const (
	DefaultIdeaBriefMax     = 3
	MaxIdeaBriefMax         = 10
	IdeaBriefMaxCharacters  = 700
	IdeaBriefScopeCurrent   = "current"
	IdeaBriefScopeAll       = "all"
	ideaBriefFooterHint     = "brain idea list"
	ideaGreetingStateDir    = "agent"
	ideaGreetingStateFile   = "ideas.json"
	ideaGreetingIgnoreEntry = "*\n"
)

// ideaGreetingState is the machine-local mute file. It is never published and
// never enters a record or the index.
type ideaGreetingState struct {
	GreetingMutedUntil *time.Time `json:"greeting_muted_until"`
}

// IdeaBriefOptions selects the brief scope and size. Scope is "all" (default)
// or "current"; ProjectID narrows an "all" brief to the project that receives
// the first items and, with "current", selects the only project.
type IdeaBriefOptions struct {
	ProjectID string
	Scope     string
	Max       int
}

// IdeaBriefResult is the one session block every surface prints: CLI, MCP
// brief, the local API and the session hook. Text never contains a body.
type IdeaBriefResult struct {
	Text     string
	Ideas    []domain.Idea
	Open     int
	DueToday int
	Muted    bool
}

// IdeaBrief renders the capped session block. The same function serves the
// CLI, MCP, the API and the session hook, so the text cannot drift between
// surfaces. It never includes a body. The block covers actionable ideas only:
// open and doing. Someday is parked in the icebox and does not inflate the
// greeting counts; `brain idea list --status someday` and the desktop Ideas
// page still show it.
func (a *App) IdeaBrief(ctx context.Context, opts IdeaBriefOptions) (IdeaBriefResult, error) {
	scope := strings.TrimSpace(opts.Scope)
	switch scope {
	case "":
		scope = IdeaBriefScopeAll
	case IdeaBriefScopeAll, IdeaBriefScopeCurrent:
	default:
		return IdeaBriefResult{}, domain.NewValidationError("scope",
			fmt.Sprintf("unknown scope %q (current or all)", opts.Scope))
	}
	max := opts.Max
	if max == 0 {
		max = DefaultIdeaBriefMax
	}
	if max < 1 || max > MaxIdeaBriefMax {
		return IdeaBriefResult{}, domain.NewValidationError("max",
			fmt.Sprintf("must be between 1 and %d", MaxIdeaBriefMax))
	}
	// The session block is for actionable ideas: open and doing. Someday is
	// parked (the icebox) and stays available through `brain idea list
	// --status someday` and the desktop Ideas page, but it is not part of the
	// greeting's open/due-today counts.
	statuses := []domain.IdeaStatus{domain.IdeaOpen, domain.IdeaDoing}
	result := IdeaBriefResult{Ideas: []domain.Idea{}}
	currentProjectID := ""
	if scope == IdeaBriefScopeCurrent {
		project, _, err := a.resolveIdeaProject(ctx, opts.ProjectID)
		if err != nil {
			return IdeaBriefResult{}, err
		}
		currentProjectID = project.ID
		list, err := a.ListIdeas(ctx, IdeaListOptions{ProjectID: project.ID, Statuses: statuses})
		if err != nil {
			return IdeaBriefResult{}, err
		}
		result.Ideas = list.Ideas
	} else {
		if selector := strings.TrimSpace(opts.ProjectID); selector != "" && selector != "auto" {
			project, _, err := a.resolveIdeaProject(ctx, selector)
			if err != nil {
				return IdeaBriefResult{}, err
			}
			currentProjectID = project.ID
		} else if project, _, err := a.resolveIdeaProject(ctx, ""); err == nil {
			// The current project orders the block but does not define the
			// scope: an unregistered working directory still gets a global
			// brief.
			currentProjectID = project.ID
		}
		list, err := a.ListIdeas(ctx, IdeaListOptions{All: true, Statuses: statuses})
		if err != nil {
			return IdeaBriefResult{}, err
		}
		result.Ideas = list.Ideas
	}
	now := a.now()
	result.Open = len(result.Ideas)
	result.DueToday = ideaDueTodayCount(result.Ideas, now)
	if until := a.IdeaGreetingMutedUntil(); !until.IsZero() {
		result.Muted = true
		result.Ideas = []domain.Idea{}
		result.Text = fmt.Sprintf("ideas: %d open (greeting muted)", result.Open)
		return result, nil
	}
	selected := selectIdeaBriefItems(result.Ideas, currentProjectID, max, now)
	result.Ideas = selected
	result.Text = renderIdeaBriefText(selected, result.Open, result.DueToday, now)
	return result, nil
}

// selectIdeaBriefItems picks the block items in the contract order: up to two
// from the current project, then one global overdue/today item, then the next
// items in store order until max. Duplicates are never picked twice.
func selectIdeaBriefItems(ideas []domain.Idea, currentProjectID string, max int, now time.Time) []domain.Idea {
	selected := make([]domain.Idea, 0, max)
	used := make(map[string]bool, max)
	take := func(idea domain.Idea) {
		if len(selected) >= max || used[idea.ID] {
			return
		}
		selected = append(selected, idea)
		used[idea.ID] = true
	}
	if currentProjectID != "" {
		fromCurrent := 0
		for _, idea := range ideas {
			if fromCurrent >= 2 || len(selected) >= max {
				break
			}
			if idea.ProjectID != currentProjectID {
				continue
			}
			take(idea)
			fromCurrent++
		}
	}
	for _, idea := range ideas {
		if len(selected) >= max {
			break
		}
		if used[idea.ID] {
			continue
		}
		if ideaDueOnOrBeforeToday(idea, now) {
			take(idea)
			break
		}
	}
	for _, idea := range ideas {
		if len(selected) >= max {
			break
		}
		take(idea)
	}
	return selected
}

// renderIdeaBriefText builds the header, item lines and footer, and trims items
// until the block fits the 700-character budget.
func renderIdeaBriefText(ideas []domain.Idea, open, dueToday int, now time.Time) string {
	for {
		header := fmt.Sprintf("## Ideas — %d open", open)
		if dueToday > 0 {
			header += fmt.Sprintf(" · %d due today", dueToday)
		}
		lines := make([]string, 0, len(ideas)+2)
		lines = append(lines, header)
		for _, idea := range ideas {
			lines = append(lines, ideaBriefLine(idea, now))
		}
		if remaining := open - len(ideas); remaining > 0 {
			lines = append(lines, fmt.Sprintf("+%d more · %s", remaining, ideaBriefFooterHint))
		}
		text := strings.Join(lines, "\n")
		if utf8.RuneCountInString(text) <= IdeaBriefMaxCharacters || len(ideas) == 0 {
			return text
		}
		ideas = ideas[:len(ideas)-1]
	}
}

// ideaBriefLine renders one item: marker, due or priority chip, title and
// project. The body is never included.
func ideaBriefLine(idea domain.Idea, now time.Time) string {
	line := "- " + IdeaMarker(idea, now)
	if label, ok := IdeaDueLabel(idea, now); ok && label != "" {
		line += " " + label
	}
	line += " — " + idea.Title
	if idea.ProjectID != "" {
		line += " (" + idea.ProjectID + ")"
	}
	return line
}

// ideaDueTodayCount counts non-terminal ideas whose effective due falls on the
// machine's local calendar date.
func ideaDueTodayCount(ideas []domain.Idea, now time.Time) int {
	count := 0
	for _, idea := range ideas {
		if IdeaDueToday(idea, now) {
			count++
		}
	}
	return count
}

// IdeaDueToday reports whether an idea is due on the local date of now. A timed
// due is compared in local time; an all-day due is the stored local date.
func IdeaDueToday(idea domain.Idea, now time.Time) bool {
	today := localIdeaDate(now)
	if idea.DueAt != nil && !idea.DueAt.IsZero() {
		return localIdeaDate(*idea.DueAt) == today
	}
	return idea.DueDate != "" && idea.DueDate == today
}

// ideaDueOnOrBeforeToday reports whether an idea is overdue or due today, the
// second selection bucket of the brief.
func ideaDueOnOrBeforeToday(idea domain.Idea, now time.Time) bool {
	today := localIdeaDate(now)
	if idea.DueAt != nil && !idea.DueAt.IsZero() {
		return localIdeaDate(*idea.DueAt) <= today
	}
	return idea.DueDate != "" && idea.DueDate <= today
}

// IdeaMarker renders the checkbox of the contract: [!] overdue, [x] done,
// [~] dropped and [ ] otherwise.
func IdeaMarker(idea domain.Idea, now time.Time) string {
	switch idea.Status {
	case domain.IdeaDone:
		return "[x]"
	case domain.IdeaDropped:
		return "[~]"
	}
	if ideaOverdue(idea, now) {
		return "[!]"
	}
	return "[ ]"
}

// IdeaDueLabel renders the compact due or priority chip of the list and brief:
// "today 18:00", "tomorrow", "Fri", "2026-12-03", "overdue 2d", "high" or
// "low". The second result is false when the idea has no chip.
func IdeaDueLabel(idea domain.Idea, now time.Time) (string, bool) {
	if idea.Status.Terminal() {
		return "", false
	}
	now = now.In(time.Local)
	today := localIdeaDate(now)
	if idea.DueAt != nil && !idea.DueAt.IsZero() {
		due := idea.DueAt.In(time.Local)
		if due.Before(now) {
			return overdueIdeaLabel(due, now), true
		}
		return futureIdeaLabel(due, today, true), true
	}
	if idea.DueDate != "" {
		date, err := time.ParseInLocation(domain.IdeaDateLayout, idea.DueDate, time.Local)
		if err == nil {
			if idea.DueDate < today {
				return overdueIdeaLabel(date, now), true
			}
			return futureIdeaLabel(date, today, false), true
		}
	}
	switch idea.Priority {
	case domain.IdeaPriorityHigh, domain.IdeaPriorityLow:
		return string(idea.Priority), true
	}
	return "", false
}

// futureIdeaLabel names a due on the local calendar: today, tomorrow, a weekday
// within the next week, or the full date. Timed values also show HH:MM; an
// all-day value is a date and has no time.
func futureIdeaLabel(due time.Time, today string, timed bool) string {
	date := localIdeaDate(due)
	offset := ideaDateOffset(today, date)
	var base string
	switch {
	case offset == 0:
		base = "today"
	case offset == 1:
		base = "tomorrow"
	case offset > 1 && offset < 7:
		base = due.Format("Mon")
	default:
		base = due.Format(domain.IdeaDateLayout)
	}
	if timed {
		base += " " + due.Format("15:04")
	}
	return base
}

// overdueIdeaLabel counts whole local calendar days between the due date and
// now: "overdue 2d", or "overdue" when the due is today but the instant passed.
func overdueIdeaLabel(due time.Time, now time.Time) string {
	days := ideaDateOffset(localIdeaDate(due), localIdeaDate(now))
	if days <= 0 {
		return "overdue"
	}
	return fmt.Sprintf("overdue %dd", days)
}

// ideaOverdue reports whether a non-terminal idea is past its effective due.
// An all-day idea due today is not overdue; a timed idea due earlier today is.
func ideaOverdue(idea domain.Idea, now time.Time) bool {
	if idea.Status.Terminal() {
		return false
	}
	if idea.DueAt != nil && !idea.DueAt.IsZero() {
		return idea.DueAt.Before(now)
	}
	if idea.DueDate != "" {
		return idea.DueDate < localIdeaDate(now)
	}
	return false
}

// localIdeaDate renders the local calendar date of an instant.
func localIdeaDate(at time.Time) string {
	return at.In(time.Local).Format(domain.IdeaDateLayout)
}

// ideaDateOffset is the signed number of days from one YYYY-MM-DD date to
// another. Both dates are calendar dates in the machine's local zone.
func ideaDateOffset(from, to string) int {
	fromDate, err := time.ParseInLocation(domain.IdeaDateLayout, from, time.Local)
	if err != nil {
		return 0
	}
	toDate, err := time.ParseInLocation(domain.IdeaDateLayout, to, time.Local)
	if err != nil {
		return 0
	}
	return int(toDate.Sub(fromDate).Hours() / 24)
}

// IdeaGreetingMutedUntil returns the machine-local mute deadline, or the zero
// time when the greeting is not muted. A missing or unreadable file is not
// muted: the brief is best-effort and must never fail because of local state.
func (a *App) IdeaGreetingMutedUntil() time.Time {
	path, err := a.ideaGreetingStatePath()
	if err != nil {
		return time.Time{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	var state ideaGreetingState
	if err := json.Unmarshal(raw, &state); err != nil || state.GreetingMutedUntil == nil {
		return time.Time{}
	}
	until := state.GreetingMutedUntil.UTC()
	if !until.After(a.now()) {
		return time.Time{}
	}
	return until
}

// SetIdeaGreetingMute writes the machine-local mute file, or clears it when
// until is nil. The file is 0600 and lives in the self-ignored agent directory,
// so it is never published. It returns the effective deadline, zero when
// cleared.
func (a *App) SetIdeaGreetingMute(until *time.Time) (time.Time, error) {
	path, err := a.ideaGreetingStatePath()
	if err != nil {
		return time.Time{}, err
	}
	state := ideaGreetingState{}
	if until != nil && !until.IsZero() {
		deadline := until.UTC()
		state.GreetingMutedUntil = &deadline
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return time.Time{}, fmt.Errorf("ideas: encode greeting state: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return time.Time{}, fmt.Errorf("ideas: create agent directory: %w", err)
	}
	ignorePath := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(ignorePath, []byte(ideaGreetingIgnoreEntry), 0o600); err != nil {
		return time.Time{}, fmt.Errorf("ideas: write agent ignore file: %w", err)
	}
	if err := writeIdeaLocalFile(path, append(raw, '\n')); err != nil {
		return time.Time{}, err
	}
	if state.GreetingMutedUntil == nil {
		return time.Time{}, nil
	}
	return *state.GreetingMutedUntil, nil
}

// ideaGreetingStatePath is <data-dir>/agent/ideas.json. Server mode has no data
// directory, so the mute state cannot be stored there.
func (a *App) ideaGreetingStatePath() (string, error) {
	dataDir := strings.TrimSpace(a.cfg.DataDir)
	if dataDir == "" {
		return "", domain.NewValidationError("data_dir", "the greeting mute needs a local data directory")
	}
	return filepath.Join(dataDir, ideaGreetingStateDir, ideaGreetingStateFile), nil
}

// writeIdeaLocalFile atomically replaces one machine-local file with 0600
// permissions. It is not a record write: no redaction, index or git applies.
func writeIdeaLocalFile(path string, raw []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ideas-*.tmp")
	if err != nil {
		return fmt.Errorf("ideas: create temporary state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("ideas: secure temporary state file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("ideas: write temporary state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("ideas: close temporary state file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("ideas: replace %s: %w", path, err)
	}
	return nil
}
