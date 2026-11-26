package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// How an idea's project was chosen. The value is reported to every surface so
// an agent can correct a wrong guess with one follow-up.
const (
	IdeaProjectExplicit = "explicit"
	IdeaProjectCWD      = "cwd"
	IdeaProjectDefault  = "default"
)

// personalProjectID is the existing project untagged ideas fall back to. The
// engine never creates it: an explicit unknown id is rejected.
const personalProjectID = "personal"

// IdeaInput is a new idea. ProjectID may be an id, an alias or a checkout
// path; empty resolves the working directory and then the personal project.
type IdeaInput struct {
	ProjectID           string
	Title               string
	Body                string
	Status              domain.IdeaStatus
	Priority            domain.IdeaPriority
	DueAt               *time.Time
	DueDate             string
	RemindBeforeMinutes *int
	Tags                []string
	Source              *domain.IdeaSource
	CompletionNote      string
	Provenance          domain.Provenance
	// ID pins the record id. It must already be a valid idea id; retries
	// allocate a fresh ULID instead of adding a numeric suffix.
	ID string
	// AllowProtected overrides the protected project and glob refusal. MCP
	// never sets it; the explicit CLI flag does.
	AllowProtected bool
}

// IdeaResult identifies an appended idea and how its project was chosen.
type IdeaResult struct {
	Idea          domain.Idea
	Revision      domain.SyncRevision
	ProjectSource string
}

// IdeaListOptions narrows ListIdeas. All lists every registered project (and
// orphan project idea directories); otherwise ProjectID resolves like a write:
// explicit selector, then cwd, then personal.
type IdeaListOptions struct {
	ProjectID string
	All       bool
	Statuses  []domain.IdeaStatus
	Tags      []string
	DueBefore time.Time
	DueAfter  time.Time
	Query     string
	Limit     int
}

// IdeaListResult is a filtered idea listing. Problems are the idea files the
// store skipped (conflicted or unparsable); they are reported, never dropped or
// rewritten.
type IdeaListResult struct {
	Ideas         []domain.Idea
	Problems      []ports.RecordProblem
	ProjectSource string
}

// IdeaUpdateInput edits one stored idea. Nil means "unchanged"; the Clear flags
// are the only way to remove a due, a reminder lead or the tags.
type IdeaUpdateInput struct {
	ID string
	// ProjectID moves the idea when non-empty and different from the stored
	// project. It must resolve to a registered project.
	ProjectID           string
	Title               *string
	Body                *string
	Status              *domain.IdeaStatus
	Priority            *domain.IdeaPriority
	DueAt               *time.Time
	DueDate             *string
	ClearDue            bool
	RemindBeforeMinutes *int
	ClearReminder       bool
	Tags                *[]string
	ClearTags           bool
	CompletionNote      *string
	// ExpectedUpdatedAt is the updated_at the caller read. When non-zero the
	// write is a compare-and-swap: a stale value returns
	// *domain.RevisionConflictError and writes nothing.
	ExpectedUpdatedAt time.Time
}

// IdeaUpdateResult is the rewritten idea. ProjectSource is set only when the
// request moved the idea.
type IdeaUpdateResult struct {
	Idea          domain.Idea
	Moved         bool
	ProjectSource string
}

func (in *IdeaInput) redact() {
	redactAll(&in.Title, &in.Body, &in.CompletionNote)
	in.Tags = redactCopy(in.Tags)
	if in.Source != nil {
		source := *in.Source
		redactAll(&source.ID, &source.Harness)
		in.Source = &source
	}
}

func (in *IdeaUpdateInput) redact() {
	if in.Title != nil {
		title := redactEditedText(*in.Title)
		in.Title = &title
	}
	if in.Body != nil {
		body := redactEditedText(*in.Body)
		in.Body = &body
	}
	if in.Tags != nil {
		tags := redactEditedTags(*in.Tags)
		in.Tags = &tags
	}
	if in.CompletionNote != nil {
		note := redactEditedText(*in.CompletionNote)
		in.CompletionNote = &note
	}
}

// ideaStore returns the optional idea capability. A store without it (server
// mode) reports ErrUnsupported instead of pretending there are no ideas.
func (a *App) ideaStore() (ports.IdeaStore, error) {
	if err := a.requireStore(); err != nil {
		return nil, err
	}
	store, ok := a.deps.Store.(ports.IdeaStore)
	if !ok {
		return nil, fmt.Errorf("%w: ideas are not supported by this store yet (server mode)", domain.ErrUnsupported)
	}
	return store, nil
}

// projectIdeas loads the live ideas of one project. Knowledge retrieval never
// calls it, so ideas stay out of `brain context`. A store without the optional
// IdeaStore (server mode) has no ideas: record listing still works and simply
// omits them, while the dedicated idea use cases return ErrUnsupported.
func (a *App) projectIdeas(ctx context.Context, projectID string) ([]domain.Idea, error) {
	store, ok := a.deps.Store.(ports.IdeaStore)
	if !ok {
		return nil, nil
	}
	return store.ListIdeas(ctx, ports.IdeaFilter{ProjectID: projectID})
}

// resolveIdeaProject resolves a write/read scope:
//
//  1. an explicit selector is matched as id, alias or checkout path and an
//     unknown id is rejected with candidates;
//  2. an empty selector uses the working directory's project;
//  3. when the working directory is not a registered project, the existing
//     personal project is used;
//  4. when even personal is absent, the call fails: the engine never invents
//     or registers a project.
func (a *App) resolveIdeaProject(ctx context.Context, selector string) (domain.Project, string, error) {
	selector = strings.TrimSpace(selector)
	if selector != "" && selector != "auto" {
		project, err := a.explicitIdeaProject(ctx, selector)
		if err != nil {
			return domain.Project{}, "", err
		}
		return project, IdeaProjectExplicit, nil
	}
	resolved, err := a.ResolveSelector(ctx, "")
	if err == nil {
		return resolved.Project, IdeaProjectCWD, nil
	}
	if !errors.Is(err, domain.ErrValidation) {
		return domain.Project{}, "", err
	}
	personal, ok, lookupErr := a.LookupProject(ctx, personalProjectID)
	if lookupErr != nil {
		return domain.Project{}, "", lookupErr
	}
	if !ok {
		return domain.Project{}, "", domain.NewValidationError("project",
			fmt.Sprintf("the working directory is not a registered project and no %q project exists; pass --project", personalProjectID))
	}
	return personal, IdeaProjectDefault, nil
}

// explicitIdeaProject resolves an explicit selector without creating anything:
// id and alias go through the existing ensureProject, a checkout path through
// ResolveSelector. An unknown selector is rejected with candidates.
func (a *App) explicitIdeaProject(ctx context.Context, selector string) (domain.Project, error) {
	if !looksLikePath(selector) {
		project, err := a.ensureProject(ctx, selector, false)
		if err == nil {
			return project, nil
		}
		if !errors.Is(err, domain.ErrValidation) {
			return domain.Project{}, err
		}
		return a.unknownIdeaProject(ctx, selector)
	}
	if resolved, err := a.ResolveSelector(ctx, selector); err == nil {
		return resolved.Project, nil
	}
	return a.unknownIdeaProject(ctx, selector)
}

// unknownIdeaProject builds the rejection of an explicit selector: the engine
// never invents or registers a project, and the error names candidates.
func (a *App) unknownIdeaProject(ctx context.Context, selector string) (domain.Project, error) {
	candidates, err := a.ideaProjectCandidates(ctx, selector)
	if err != nil {
		return domain.Project{}, err
	}
	message := fmt.Sprintf("%q is not a registered project id, alias or checkout path", selector)
	if len(candidates) > 0 {
		message += "; candidates: " + strings.Join(candidates, ", ")
	}
	return domain.Project{}, domain.NewValidationError("project", message)
}

// ideaProjectCandidates returns up to five ids to show in an unknown-project
// error: ids or aliases containing the selector, else the first registered ids.
func (a *App) ideaProjectCandidates(ctx context.Context, selector string) ([]string, error) {
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(strings.TrimSpace(selector))
	var matches, all []string
	for _, project := range projects {
		all = append(all, project.ID)
		if strings.Contains(strings.ToLower(project.ID), needle) {
			matches = append(matches, project.ID)
			continue
		}
		for _, alias := range project.Aliases {
			if strings.Contains(strings.ToLower(alias), needle) {
				matches = append(matches, project.ID)
				break
			}
		}
	}
	candidates := matches
	if len(candidates) == 0 {
		candidates = all
	}
	sort.Strings(candidates)
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}
	return candidates, nil
}

// afterIdeaWrite projects a written idea into the search index. A write never
// depends on the index: a failure is reported as "written but not indexed" and
// marks the projection dirty, so the next search repairs it and the idea file
// is never lost. A store without an index (or a nil one) is a no-op.
func (a *App) afterIdeaWrite(ctx context.Context, idea domain.Idea) error {
	if a.deps.Index == nil {
		return nil
	}
	return a.upsertIndex(ctx, ideaDocument(idea))
}

// afterIdeaTrash removes a trashed idea from the search index. The file is
// already in trash, so a failed delete only marks the index dirty; the next
// reindex drops the row because the live file is gone.
func (a *App) afterIdeaTrash(ctx context.Context, idea domain.Idea) error {
	if a.deps.Index == nil {
		return nil
	}
	if err := a.deps.Index.Delete(ctx, indexKey(idea.ProjectID, idea.ID)); err != nil {
		a.markIndexDirty()
		return fmt.Errorf("idea %s was trashed but the search index update failed: %w", idea.ID, err)
	}
	return nil
}

// AddIdea resolves the project, redacts, validates the limits, allocates the
// id and writes one idea file. Nothing is written before every refusal point.
func (a *App) AddIdea(ctx context.Context, input IdeaInput) (IdeaResult, error) {
	input.redact()
	store, err := a.ideaStore()
	if err != nil {
		return IdeaResult{}, err
	}
	if err := a.checkProtectedSelector(input.ProjectID, ports.RecordIdea, strings.TrimSpace(input.Title), input.AllowProtected); err != nil {
		return IdeaResult{}, err
	}
	project, source, err := a.resolveIdeaProject(ctx, input.ProjectID)
	if err != nil {
		return IdeaResult{}, err
	}
	status := input.Status
	if status == "" {
		status = domain.IdeaOpen
	}
	if !status.Valid() {
		return IdeaResult{}, domain.NewValidationError("idea.status", fmt.Sprintf("unknown status %q", status))
	}
	priority := input.Priority
	if priority == "" {
		priority = domain.IdeaPriorityNormal
	}
	if !priority.Valid() {
		return IdeaResult{}, domain.NewValidationError("idea.priority", fmt.Sprintf("unknown priority %q", priority))
	}
	// The file stores RFC3339 second precision; truncating here keeps the
	// returned row, the CLI JSON and the reopened file the same revision.
	now := a.now().UTC().Truncate(time.Second)
	idea := domain.Idea{
		ProjectID:           project.ID,
		Title:               strings.TrimSpace(input.Title),
		Body:                input.Body,
		Status:              status,
		Priority:            priority,
		DueAt:               input.DueAt,
		DueDate:             strings.TrimSpace(input.DueDate),
		RemindBeforeMinutes: input.RemindBeforeMinutes,
		Tags:                input.Tags,
		Source:              input.Source,
		CompletionNote:      input.CompletionNote,
		Provenance:          input.Provenance.Normalized(),
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if idea.DueAt != nil && idea.DueDate != "" {
		return IdeaResult{}, domain.NewValidationError("idea.due", "due_at and due_date are mutually exclusive")
	}
	if idea.Status == domain.IdeaDone {
		// done is a writer action even on create: completed_at records when.
		completed := now
		idea.CompletedAt = &completed
	}
	if err := idea.Validate(); err != nil {
		return IdeaResult{}, err
	}
	if err := a.enforceCreateLimits(project.ID, ports.RecordIdea, ideaWriteContent(idea), input.AllowProtected); err != nil {
		return IdeaResult{}, err
	}
	pinned := strings.TrimSpace(input.ID)
	if pinned != "" && !domain.IsRecordID(ports.RecordIdea, pinned) {
		return IdeaResult{}, domain.NewValidationError("idea.id", fmt.Sprintf("%q is not an idea id", input.ID))
	}
	for try := 0; try < maxRecordIDAttempts; try++ {
		id := pinned
		if try > 0 {
			id = ""
		}
		if id == "" {
			allocated, err := domain.NewIdeaID(idea.CreatedAt, idea.Title)
			if err != nil {
				return IdeaResult{}, err
			}
			id = allocated
		}
		idea.ID = id
		revision, appendErr := store.AppendIdea(ctx, idea)
		if appendErr == nil {
			if err := a.afterIdeaWrite(ctx, idea); err != nil {
				return IdeaResult{Idea: idea, Revision: revision, ProjectSource: source}, err
			}
			return IdeaResult{Idea: idea, Revision: revision, ProjectSource: source}, nil
		}
		if !errors.Is(appendErr, domain.ErrTargetExists) {
			return IdeaResult{}, appendErr
		}
	}
	return IdeaResult{}, fmt.Errorf("idea: %w: no free record id", domain.ErrConflict)
}

// ListIdeas lists ideas with the contract filters. A single project is
// resolved like a write; All lists every registered project and keeps orphan
// idea directories visible, and has no single ProjectSource.
func (a *App) ListIdeas(ctx context.Context, opts IdeaListOptions) (IdeaListResult, error) {
	store, err := a.ideaStore()
	if err != nil {
		return IdeaListResult{}, err
	}
	if opts.All && strings.TrimSpace(opts.ProjectID) != "" {
		return IdeaListResult{}, domain.NewValidationError("project", "all and project are mutually exclusive")
	}
	for _, status := range opts.Statuses {
		if !status.Valid() {
			return IdeaListResult{}, domain.NewValidationError("idea.status", fmt.Sprintf("unknown status %q", status))
		}
	}
	filter := ports.IdeaFilter{
		Statuses:  opts.Statuses,
		Tags:      opts.Tags,
		DueBefore: opts.DueBefore,
		DueAfter:  opts.DueAfter,
		Query:     opts.Query,
		Limit:     opts.Limit,
	}
	result := IdeaListResult{}
	var projectIDs []string
	if opts.All {
		// All is not a project resolution: there is no single project_source,
		// so ProjectSource stays empty and the JSON omits it.
		projects, err := a.deps.Store.ListProjects(ctx)
		if err != nil {
			return IdeaListResult{}, err
		}
		for _, project := range projects {
			projectIDs = append(projectIDs, project.ID)
		}
	} else {
		project, source, err := a.resolveIdeaProject(ctx, opts.ProjectID)
		if err != nil {
			return IdeaListResult{}, err
		}
		filter.ProjectID = project.ID
		result.ProjectSource = source
		projectIDs = []string{project.ID}
	}
	ideas, err := store.ListIdeas(ctx, filter)
	if err != nil {
		return IdeaListResult{}, err
	}
	result.Ideas = ideas
	for _, projectID := range projectIDs {
		problems, err := a.ideaProblems(ctx, projectID)
		if err != nil {
			return IdeaListResult{}, err
		}
		result.Problems = append(result.Problems, problems...)
	}
	return result, nil
}

// ideaProblems returns the idea problem rows of one project. A store without a
// problem reporter simply reports none.
func (a *App) ideaProblems(ctx context.Context, projectID string) ([]ports.RecordProblem, error) {
	reporter, ok := a.deps.Store.(ports.ProblemReporter)
	if !ok {
		return nil, nil
	}
	problems, err := reporter.Problems(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]ports.RecordProblem, 0, len(problems))
	for _, problem := range problems {
		if problem.Kind == ports.RecordIdea {
			out = append(out, problem)
		}
	}
	return out, nil
}

// GetIdea finds one idea by its globally unique id. A problem file returns the
// typed domain.ProblemError (path + reason), never a zero idea.
func (a *App) GetIdea(ctx context.Context, id string) (domain.Idea, error) {
	store, err := a.ideaStore()
	if err != nil {
		return domain.Idea{}, err
	}
	return store.GetIdea(ctx, strings.TrimSpace(id))
}

// UpdateIdea edits one stored idea: mutable fields, the status transition, the
// completion fields and an optional project move. Omitted fields are
// unchanged.
func (a *App) UpdateIdea(ctx context.Context, input IdeaUpdateInput) (IdeaUpdateResult, error) {
	input.redact()
	store, err := a.ideaStore()
	if err != nil {
		return IdeaUpdateResult{}, err
	}
	id := strings.TrimSpace(input.ID)
	if err := domain.ValidateRequired("idea.id", id); err != nil {
		return IdeaUpdateResult{}, err
	}
	if input.ClearDue && (input.DueAt != nil || input.DueDate != nil) {
		return IdeaUpdateResult{}, domain.NewValidationError("idea.due", "clear and set are mutually exclusive")
	}
	if input.DueAt != nil && input.DueDate != nil {
		return IdeaUpdateResult{}, domain.NewValidationError("idea.due", "due_at and due_date are mutually exclusive")
	}
	if input.ClearReminder && input.RemindBeforeMinutes != nil {
		return IdeaUpdateResult{}, domain.NewValidationError("idea.remindBeforeMinutes", "clear and set are mutually exclusive")
	}
	if input.ClearTags && input.Tags != nil {
		return IdeaUpdateResult{}, domain.NewValidationError("idea.tags", "clear and set are mutually exclusive")
	}
	original, err := store.GetIdea(ctx, id)
	if err != nil {
		return IdeaUpdateResult{}, err
	}
	if !input.ExpectedUpdatedAt.IsZero() && !original.UpdatedAt.Equal(input.ExpectedUpdatedAt) {
		return IdeaUpdateResult{}, domain.NewRevisionConflictError(ports.RecordIdea, original.ID, original.UpdatedAt)
	}
	idea := original
	wasTerminal := idea.Status.Terminal()
	if input.Title != nil {
		idea.Title = strings.TrimSpace(*input.Title)
	}
	if input.Body != nil {
		idea.Body = *input.Body
	}
	if input.Priority != nil {
		priority, err := domain.ParseIdeaPriority(string(*input.Priority))
		if err != nil {
			return IdeaUpdateResult{}, err
		}
		idea.Priority = priority
	}
	switch {
	case input.ClearDue:
		idea.DueAt = nil
		idea.DueDate = ""
	case input.DueAt != nil:
		if input.DueAt.IsZero() {
			return IdeaUpdateResult{}, domain.NewValidationError("idea.dueAt", "must be a real timestamp")
		}
		due := *input.DueAt
		idea.DueAt = &due
		idea.DueDate = ""
	case input.DueDate != nil:
		dueDate := strings.TrimSpace(*input.DueDate)
		if dueDate == "" {
			return IdeaUpdateResult{}, domain.NewValidationError("idea.dueDate", "must not be empty; use clear_due")
		}
		if _, err := time.Parse(domain.IdeaDateLayout, dueDate); err != nil {
			return IdeaUpdateResult{}, domain.NewValidationError("idea.dueDate",
				fmt.Sprintf("%q is not a %s date", dueDate, domain.IdeaDateLayout))
		}
		idea.DueDate = dueDate
		idea.DueAt = nil
	}
	switch {
	case input.ClearReminder:
		idea.RemindBeforeMinutes = nil
	case input.RemindBeforeMinutes != nil:
		remind := *input.RemindBeforeMinutes
		idea.RemindBeforeMinutes = &remind
	}
	switch {
	case input.ClearTags:
		idea.Tags = nil
	case input.Tags != nil:
		idea.Tags = append([]string(nil), (*input.Tags)...)
	}
	if input.CompletionNote != nil {
		idea.CompletionNote = *input.CompletionNote
	}
	if input.Status != nil {
		next, err := domain.ParseIdeaStatus(string(*input.Status))
		if err != nil {
			return IdeaUpdateResult{}, err
		}
		from := idea.Status
		if from == "" {
			from = domain.IdeaOpen
		}
		if !domain.CanTransition(from, next) {
			return IdeaUpdateResult{}, domain.NewValidationError("idea.status",
				fmt.Sprintf("cannot move an idea from %s to %s", from, next))
		}
		idea.Status = next
	}
	now := a.now().UTC().Truncate(time.Second)
	switch {
	case idea.Status == domain.IdeaDone:
		if idea.CompletedAt == nil {
			idea.CompletedAt = &now
		}
	case idea.Status == domain.IdeaOpen && wasTerminal:
		// Reopening is the only edge out of done/dropped: clear both the
		// completion instant and the note, as the contract requires.
		idea.CompletedAt = nil
		idea.CompletionNote = ""
	default:
		idea.CompletedAt = nil
	}
	moved := false
	projectSource := ""
	if selector := strings.TrimSpace(input.ProjectID); selector != "" && selector != "auto" {
		project, source, err := a.resolveIdeaProject(ctx, selector)
		if err != nil {
			return IdeaUpdateResult{}, err
		}
		if project.ID != idea.ProjectID {
			idea.ProjectID = project.ID
			moved = true
			projectSource = source
		}
	}
	if err := idea.Validate(); err != nil {
		return IdeaUpdateResult{}, err
	}
	if err := a.enforceUpdateLimits(original.ProjectID, ports.RecordIdea, idea.ID, ideaWriteContent(idea)); err != nil {
		return IdeaUpdateResult{}, err
	}
	if moved {
		if err := a.checkWriteProtection(idea.ProjectID, recordDataPath(idea.ProjectID, ports.RecordIdea, idea.ID+".md"), false); err != nil {
			return IdeaUpdateResult{}, err
		}
	}
	// The store truncates to seconds when it rewrites the file; the returned
	// row must carry the same revision so expected_updated_at round-trips.
	idea.UpdatedAt = now
	if err := store.UpdateIdea(ctx, idea); err != nil {
		return IdeaUpdateResult{}, err
	}
	// The store stamps updated_at itself, so re-read the persisted row: the
	// response revision is then exactly what a later read returns, even when
	// the wall clock rolls to a new second between the two writes.
	//
	// Decision: a failed re-read after a successful store write is non-fatal.
	// The write already landed; returning an error would turn a durable
	// success into an API 500 and invite a retry that rewrites the same
	// revision. Keep the in-memory row; the next read or CAS resolves the
	// persisted revision.
	if stored, err := store.GetIdea(ctx, idea.ID); err == nil {
		idea = stored
	}
	if err := a.afterIdeaWrite(ctx, idea); err != nil {
		return IdeaUpdateResult{Idea: idea, Moved: moved, ProjectSource: projectSource}, err
	}
	return IdeaUpdateResult{Idea: idea, Moved: moved, ProjectSource: projectSource}, nil
}

// TrashIdea moves one idea to its project's trash. The id is global; an empty
// project resolves through the stored idea.
func (a *App) TrashIdea(ctx context.Context, projectID, id string) (domain.Idea, error) {
	store, err := a.ideaStore()
	if err != nil {
		return domain.Idea{}, err
	}
	idea, err := a.GetIdea(ctx, id)
	if err != nil {
		return domain.Idea{}, err
	}
	if selector := strings.TrimSpace(projectID); selector != "" && selector != "auto" {
		project, _, err := a.resolveIdeaProject(ctx, selector)
		if err != nil {
			return domain.Idea{}, err
		}
		if project.ID != idea.ProjectID {
			return domain.Idea{}, fmt.Errorf("idea %q belongs to project %s, not %s: %w",
				idea.ID, idea.ProjectID, project.ID, domain.ErrNotFound)
		}
	}
	if err := store.TrashIdea(ctx, idea.ProjectID, idea.ID); err != nil {
		return domain.Idea{}, err
	}
	if err := a.afterIdeaTrash(ctx, idea); err != nil {
		return idea, err
	}
	return idea, nil
}

// RestoreIdea brings a trashed idea back. The id is global: without an explicit
// project the registered projects are tried in registry order until the trash
// holding the id is found.
func (a *App) RestoreIdea(ctx context.Context, projectID, id string) (domain.Idea, error) {
	store, err := a.ideaStore()
	if err != nil {
		return domain.Idea{}, err
	}
	id = strings.TrimSpace(id)
	if selector := strings.TrimSpace(projectID); selector != "" && selector != "auto" {
		project, _, err := a.resolveIdeaProject(ctx, selector)
		if err != nil {
			return domain.Idea{}, err
		}
		if err := store.RestoreIdea(ctx, project.ID, id); err != nil {
			return domain.Idea{}, err
		}
		return a.finishIdeaRestore(ctx, id)
	}
	projects, err := a.deps.Store.ListProjects(ctx)
	if err != nil {
		return domain.Idea{}, err
	}
	for _, project := range projects {
		restoreErr := store.RestoreIdea(ctx, project.ID, id)
		if restoreErr == nil {
			return a.finishIdeaRestore(ctx, id)
		}
		if errors.Is(restoreErr, domain.ErrNotFound) {
			continue
		}
		return domain.Idea{}, restoreErr
	}
	return domain.Idea{}, fmt.Errorf("trashed idea %q: %w", id, domain.ErrNotFound)
}

func (a *App) finishIdeaRestore(ctx context.Context, id string) (domain.Idea, error) {
	idea, err := a.GetIdea(ctx, id)
	if err != nil {
		return domain.Idea{}, err
	}
	if err := a.afterIdeaWrite(ctx, idea); err != nil {
		return idea, err
	}
	return idea, nil
}
