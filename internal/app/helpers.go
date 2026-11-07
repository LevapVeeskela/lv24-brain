package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	maxRecordIDAttempts = 100
	maxListedItems      = 5
)

// The list capabilities are optional Store extensions used to build context
// and resolve record identifiers. Adapters that do not implement one still
// support writes; the corresponding context section simply reports no records.
type noteLister interface {
	ListNotes(ctx context.Context, projectID string) ([]domain.Note, error)
}

type decisionLister interface {
	ListDecisions(ctx context.Context, projectID string) ([]domain.Decision, error)
}

type attemptLister interface {
	ListAttempts(ctx context.Context, projectID string) ([]domain.Attempt, error)
}

type handoffLister interface {
	ListHandoffs(ctx context.Context, projectID string) ([]domain.Handoff, error)
}

func (a *App) now() time.Time {
	clock := a.deps.Now
	if clock == nil {
		clock = time.Now
	}
	return clock().UTC()
}

func (a *App) requireStore() error {
	if a.deps.Store != nil {
		return nil
	}
	if a.cfg.Mode == config.ModeServer {
		return fmt.Errorf("%w: server client is not configured; check server_url and BRAIN_TOKEN or run brain secret set server-token", domain.ErrUnsupported)
	}
	return fmt.Errorf("%w: brain data store is not available; check data_dir or run brain init", domain.ErrUnsupported)
}

func (a *App) resolveProject(ctx context.Context, projectID string) (domain.Project, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		projects, err := a.deps.Store.ListProjects(ctx)
		if err != nil {
			return domain.Project{}, err
		}
		switch len(projects) {
		case 0:
			return domain.Project{}, domain.NewValidationError("project.id", "is required: no projects exist yet")
		case 1:
			return projects[0], nil
		default:
			// Like the legacy tool, pick the project registered for the
			// working directory before asking for --project.
			if resolved, err := a.ResolveSelector(ctx, ""); err == nil {
				return resolved.Project, nil
			}
			ids := make([]string, 0, len(projects))
			for _, project := range projects {
				ids = append(ids, project.ID)
			}
			return domain.Project{}, domain.NewValidationError(
				"project.id",
				"is required: multiple projects exist ("+strings.Join(ids, ", ")+"); pass --project",
			)
		}
	}
	project, err := a.deps.Store.GetProject(ctx, id)
	if err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

// ensureProject resolves the project of a write: an explicit id or alias, or
// the working directory's project when projectID is empty. An unknown project
// is created only with create=true, so a typo or an unregistered alias never
// silently starts a new project.
func (a *App) ensureProject(ctx context.Context, projectID string, create bool) (domain.Project, error) {
	id := strings.TrimSpace(projectID)
	if id == "" {
		project, err := a.resolveProject(ctx, "")
		if err != nil {
			return domain.Project{}, err
		}
		return project, a.ensureState(ctx, project.ID)
	}
	if project, ok, err := a.LookupProject(ctx, id); err != nil {
		return domain.Project{}, err
	} else if ok {
		return project, a.ensureState(ctx, project.ID)
	}
	if !create {
		return domain.Project{}, domain.NewValidationError("project",
			fmt.Sprintf("%q is not a registered project id or alias; pass --create to register it", id))
	}
	project, err := a.deps.Store.GetProject(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		candidate := domain.Project{ID: id, Name: id, Kind: domain.ProjectKindOther}
		if validateErr := candidate.Validate(); validateErr != nil {
			return domain.Project{}, validateErr
		}
		if putErr := a.deps.Store.PutProject(ctx, candidate); putErr != nil {
			return domain.Project{}, putErr
		}
		project, err = a.deps.Store.GetProject(ctx, id)
	}
	if err != nil {
		return domain.Project{}, err
	}
	if err := a.ensureState(ctx, project.ID); err != nil {
		return domain.Project{}, err
	}
	return project, nil
}

func (a *App) ensureState(ctx context.Context, projectID string) error {
	if _, err := a.deps.Store.GetState(ctx, projectID); err == nil {
		return nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if _, err := a.deps.Store.PutState(ctx, domain.State{ProjectID: projectID, UpdatedAt: a.now()}); err != nil {
		return fmt.Errorf("initialize state for %s: %w", projectID, err)
	}
	return nil
}

func (a *App) listNotes(ctx context.Context, projectID string) ([]domain.Note, error) {
	lister, ok := a.deps.Store.(noteLister)
	if !ok {
		return nil, nil
	}
	return lister.ListNotes(ctx, projectID)
}

func (a *App) listDecisions(ctx context.Context, projectID string) ([]domain.Decision, error) {
	lister, ok := a.deps.Store.(decisionLister)
	if !ok {
		return nil, nil
	}
	return lister.ListDecisions(ctx, projectID)
}

func (a *App) listAttempts(ctx context.Context, projectID string) ([]domain.Attempt, error) {
	lister, ok := a.deps.Store.(attemptLister)
	if !ok {
		return nil, nil
	}
	return lister.ListAttempts(ctx, projectID)
}

func (a *App) listHandoffs(ctx context.Context, projectID string) ([]domain.Handoff, error) {
	lister, ok := a.deps.Store.(handoffLister)
	if !ok {
		return nil, nil
	}
	return lister.ListHandoffs(ctx, projectID)
}

func recordID(prefix, projectID string, now time.Time) string {
	return prefix + "-" + projectID + "-" + now.UTC().Format("20060102-150405")
}

func availableID(base string, used map[string]struct{}) string {
	if _, exists := used[base]; !exists {
		return base
	}
	for suffix := 2; suffix <= maxRecordIDAttempts; suffix++ {
		candidate := base + "-" + strconv.Itoa(suffix)
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
	return base + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func stringSet(values ...[]string) map[string]struct{} {
	used := make(map[string]struct{})
	for _, group := range values {
		for _, value := range group {
			used[value] = struct{}{}
		}
	}
	return used
}

func noteIDs(notes []domain.Note) []string {
	ids := make([]string, 0, len(notes))
	for _, note := range notes {
		ids = append(ids, note.ID)
	}
	return ids
}

func decisionIDs(decisions []domain.Decision) []string {
	ids := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		ids = append(ids, decision.ID)
	}
	return ids
}

func attemptIDs(attempts []domain.Attempt) []string {
	ids := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		ids = append(ids, attempt.ID)
	}
	return ids
}

func handoffIDs(handoffs []domain.Handoff) []string {
	ids := make([]string, 0, len(handoffs))
	for _, handoff := range handoffs {
		ids = append(ids, handoff.ID)
	}
	return ids
}

func (a *App) upsertIndex(ctx context.Context, doc ports.Document) error {
	if a.deps.Index == nil {
		return nil
	}
	return a.deps.Index.Upsert(ctx, doc)
}

func noteDocument(note domain.Note) ports.Document {
	body := note.Body
	if len(note.Tags) > 0 {
		body = joinText(body, strings.Join(note.Tags, " "))
	}
	return ports.Document{
		ID:        note.ID,
		ProjectID: note.ProjectID,
		Kind:      "note",
		Title:     fallbackTitle(note.Title, note.Body),
		Body:      body,
		UpdatedAt: updatedOrCreated(note.UpdatedAt, note.CreatedAt),
	}
}

func decisionDocument(decision domain.Decision) ports.Document {
	return ports.Document{
		ID:        decision.ID,
		ProjectID: decision.ProjectID,
		Kind:      "decision",
		Title:     decision.Title,
		Body: joinText(
			decision.Context,
			strings.Join(decision.Alternatives, "\n"),
			strings.Join(decision.Consequences, "\n"),
			strings.Join(decision.Evidence, "\n"),
		),
		UpdatedAt: updatedOrCreated(decision.UpdatedAt, decision.CreatedAt),
	}
}

func attemptDocument(attempt domain.Attempt) ports.Document {
	return ports.Document{
		ID:        attempt.ID,
		ProjectID: attempt.ProjectID,
		Kind:      "attempt",
		Title:     attempt.Title,
		Body:      joinText(attempt.Hypothesis, attempt.Result, attempt.Reason),
		UpdatedAt: updatedOrCreated(attempt.UpdatedAt, attempt.CreatedAt),
	}
}

func handoffDocument(handoff domain.Handoff) ports.Document {
	return ports.Document{
		ID:        handoff.ID,
		ProjectID: handoff.ProjectID,
		Kind:      "handoff",
		Title:     handoffTitle(handoff),
		Body:      joinText(handoff.Summary, handoff.GitContext, handoff.NextAction),
		UpdatedAt: handoff.CreatedAt,
	}
}

func handoffTitle(handoff domain.Handoff) string {
	if handoff.CreatedAt.IsZero() {
		return "Handoff"
	}
	return "Handoff " + handoff.CreatedAt.UTC().Format("2006-01-02 15:04")
}

func updatedOrCreated(updated, created time.Time) time.Time {
	if !updated.IsZero() {
		return updated
	}
	return created
}

func fallbackTitle(title, body string) string {
	title = strings.TrimSpace(title)
	if title != "" {
		return title
	}
	if line := firstLine(body); line != "" {
		return line
	}
	return "Note"
}

func joinText(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, "\n\n")
}

func firstLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return truncateRunes(trimmed, 160)
		}
	}
	return ""
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

func humanDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	switch {
	case value < time.Minute:
		return "just now"
	case value < time.Hour:
		return fmt.Sprintf("%dm", int(value/time.Minute))
	case value < 24*time.Hour:
		hours := int(value / time.Hour)
		minutes := int((value % time.Hour) / time.Minute)
		if minutes == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, minutes)
	default:
		days := int(value / (24 * time.Hour))
		hours := int((value % (24 * time.Hour)) / time.Hour)
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, hours)
	}
}

func stateAge(now time.Time, state domain.State) string {
	if state.UpdatedAt.IsZero() {
		return "unknown"
	}
	return humanDuration(now.Sub(state.UpdatedAt))
}

func freshness(now time.Time, state domain.State) string {
	if state.FreshUntil.IsZero() {
		return "not set"
	}
	if now.After(state.FreshUntil) {
		return "stale since " + state.FreshUntil.UTC().Format(time.RFC3339)
	}
	return "fresh until " + state.FreshUntil.UTC().Format(time.RFC3339)
}

func parseSince(value string, now time.Time) (time.Time, bool, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return time.Time{}, false, nil
	}
	unit := value[len(value)-1]
	number := strings.TrimSpace(value[:len(value)-1])
	multiplier := time.Hour
	switch unit {
	case 'm':
		multiplier = time.Minute
	case 'h':
		multiplier = time.Hour
	case 'd':
		multiplier = 24 * time.Hour
	case 'w':
		multiplier = 7 * 24 * time.Hour
	default:
		return time.Time{}, false, domain.NewValidationError("review.since", "must look like 30m, 24h, 7d, or 2w")
	}
	count, err := strconv.Atoi(number)
	if err != nil || count <= 0 {
		return time.Time{}, false, domain.NewValidationError("review.since", "must look like 30m, 24h, 7d, or 2w")
	}
	return now.Add(-time.Duration(count) * multiplier), true, nil
}

func compareVersions(a, b string) int {
	ac, ap := splitVersion(a)
	bc, bp := splitVersion(b)
	if cmp := compareVersionCore(ac, bc); cmp != 0 {
		return cmp
	}
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	default:
		return strings.Compare(ap, bp)
	}
}

func splitVersion(value string) (string, string) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	value = strings.TrimPrefix(value, "V")
	if idx := strings.IndexByte(value, '+'); idx >= 0 {
		value = value[:idx]
	}
	if idx := strings.IndexByte(value, '-'); idx >= 0 {
		return value[:idx], value[idx+1:]
	}
	return value, ""
}

func compareVersionCore(a, b string) int {
	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")
	count := len(aParts)
	if len(bParts) > count {
		count = len(bParts)
	}
	for i := 0; i < count; i++ {
		aPart := "0"
		if i < len(aParts) && aParts[i] != "" {
			aPart = aParts[i]
		}
		bPart := "0"
		if i < len(bParts) && bParts[i] != "" {
			bPart = bParts[i]
		}
		aNumber, aErr := strconv.Atoi(aPart)
		bNumber, bErr := strconv.Atoi(bPart)
		if aErr == nil && bErr == nil {
			switch {
			case aNumber < bNumber:
				return -1
			case aNumber > bNumber:
				return 1
			}
			continue
		}
		if cmp := strings.Compare(aPart, bPart); cmp != 0 {
			return cmp
		}
	}
	return 0
}

// LookupProject finds a project by exact id or alias (case-insensitive); it
// never interprets the selector as a path.
func (a *App) LookupProject(ctx context.Context, selector string) (domain.Project, bool, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return domain.Project{}, false, nil
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return domain.Project{}, false, err
	}
	for _, project := range projects {
		if strings.EqualFold(project.ID, selector) || containsFold(project.Aliases, selector) {
			return project, true, nil
		}
	}
	return domain.Project{}, false, nil
}
