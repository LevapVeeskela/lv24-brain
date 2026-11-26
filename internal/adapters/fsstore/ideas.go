package fsstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// ideasDir is the per-project directory of idea files. Ideas are one file each
// (projects/<id>/ideas/<idea-id>.md), unlike notes, which share a daily file.
const ideasDir = "ideas"

var _ ports.IdeaStore = (*Store)(nil)

// ideaSourceFrontmatter is the on-disk form of domain.IdeaSource. It is
// intentionally smaller than sourceRefFrontmatter: an idea source has no
// project and no external item fields.
type ideaSourceFrontmatter struct {
	Type    string `yaml:"type"`
	ID      string `yaml:"id,omitempty"`
	Harness string `yaml:"harness,omitempty"`
}

// ideaFrontmatter is the on-disk form of one idea. due_at and completed_at are
// strings, not timestamps: due_at must keep the supplied offset (a YAML
// timestamp can be coerced to UTC), and completed_at must be omitted cleanly
// when unset. due_date is a string so no date coercion can shift a local
// calendar date.
type ideaFrontmatter struct {
	markdown.Frontmatter  `yaml:",inline"`
	provenanceFrontmatter `yaml:",inline"`
	validityFrontmatter   `yaml:",inline"`
	relationsFrontmatter  `yaml:",inline"`
	mergeFrontmatter      `yaml:",inline"`
	reviewFrontmatter     `yaml:",inline"`
	taxonomyFrontmatter   `yaml:",inline"`
	ID                    string                 `yaml:"id,omitempty"`
	Title                 string                 `yaml:"title,omitempty"`
	Status                domain.IdeaStatus      `yaml:"status,omitempty"`
	Priority              domain.IdeaPriority    `yaml:"priority,omitempty"`
	DueAt                 string                 `yaml:"due_at,omitempty"`
	DueDate               string                 `yaml:"due_date,omitempty"`
	RemindBeforeMinutes   *int                   `yaml:"remind_before_minutes,omitempty"`
	Tags                  []string               `yaml:"tags,omitempty"`
	Source                *ideaSourceFrontmatter `yaml:"source,omitempty"`
	CompletedAt           string                 `yaml:"completed_at,omitempty"`
	CompletionNote        string                 `yaml:"completion_note,omitempty"`
	CreatedAt             time.Time              `yaml:"created_at,omitempty"`
}

func ideaSourceToFrontmatter(source *domain.IdeaSource) *ideaSourceFrontmatter {
	if source == nil {
		return nil
	}
	return &ideaSourceFrontmatter{Type: source.Type, ID: source.ID, Harness: source.Harness}
}

func ideaSourceFromFrontmatter(source *ideaSourceFrontmatter) *domain.IdeaSource {
	if source == nil {
		return nil
	}
	return &domain.IdeaSource{Type: source.Type, ID: source.ID, Harness: source.Harness}
}

// formatIdeaDueAt keeps the offset of a timed due. A zero time is omitted: an
// idea with no due has neither due_at nor due_date.
func formatIdeaDueAt(at *time.Time) string {
	if at == nil || at.IsZero() {
		return ""
	}
	return at.Format(time.RFC3339Nano)
}

func formatIdeaTime(at *time.Time) string {
	if at == nil || at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

func parseIdeaDueAt(raw string) (*time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, domain.NewValidationError("idea.dueAt", fmt.Sprintf("%q is not an RFC3339 timestamp", raw))
	}
	if at.IsZero() {
		return nil, nil
	}
	return &at, nil
}

func parseIdeaCompletedAt(raw string) (*time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, domain.NewValidationError("idea.completedAt", fmt.Sprintf("%q is not an RFC3339 timestamp", raw))
	}
	if at.IsZero() {
		return nil, nil
	}
	utc := at.UTC()
	return &utc, nil
}

func normalizedIdeaStatus(status domain.IdeaStatus) domain.IdeaStatus {
	if status == "" {
		return domain.IdeaOpen
	}
	return status
}

func normalizedIdeaPriority(priority domain.IdeaPriority) domain.IdeaPriority {
	if priority == "" {
		return domain.IdeaPriorityNormal
	}
	return priority
}

// renderIdea writes the frontmatter of the contract plus the body verbatim.
// The title stays in the frontmatter and is not repeated in the body.
func renderIdea(idea domain.Idea, updatedAt time.Time) ([]byte, error) {
	frontmatter := ideaFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID:     idea.ProjectID,
			Kind:          ports.RecordIdea,
			UpdatedAt:     updatedAt.UTC(),
			SchemaVersion: SchemaVersion,
		},
		provenanceFrontmatter: provenanceToFrontmatter(idea.Provenance),
		validityFrontmatter:   validityToFrontmatter(idea.Validity, idea.CreatedAt),
		relationsFrontmatter:  relationsToFrontmatter(idea.Relations),
		mergeFrontmatter:      mergeToFrontmatter(idea.MergeInfo),
		reviewFrontmatter:     reviewToFrontmatter(idea.ReviewSchedule),
		taxonomyFrontmatter:   taxonomyToFrontmatter(idea.Taxonomy),
		ID:                    idea.ID,
		Title:                 idea.Title,
		Status:                normalizedIdeaStatus(idea.Status),
		Priority:              normalizedIdeaPriority(idea.Priority),
		DueAt:                 formatIdeaDueAt(idea.DueAt),
		DueDate:               idea.DueDate,
		RemindBeforeMinutes:   idea.RemindBeforeMinutes,
		Tags:                  nonNilStrings(idea.Tags),
		Source:                ideaSourceToFrontmatter(idea.Source),
		CompletedAt:           formatIdeaTime(idea.CompletedAt),
		CompletionNote:        idea.CompletionNote,
		CreatedAt:             idea.CreatedAt.UTC(),
	}
	body := strings.TrimRight(idea.Body, "\n")
	if body != "" {
		body += "\n"
	}
	raw, err := markdown.Render(frontmatter, body)
	if err != nil {
		return nil, fmt.Errorf("fsstore: render idea %q: %w", idea.ID, err)
	}
	return raw, nil
}

// ideaConflictReason reports git conflict markers. A conflicted file must never
// be parsed into a half-merged record; it stays in git for manual resolution.
func ideaConflictReason(raw []byte) string {
	text := strings.TrimPrefix(string(raw), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	for _, line := range strings.Split(text, "\n") {
		for _, marker := range []string{"<<<<<<<", "=======", ">>>>>>>"} {
			if strings.HasPrefix(line, marker) {
				return "git conflict markers"
			}
		}
	}
	return ""
}

// ideaProblem builds the typed error for an idea file that cannot be read as a
// valid record. Callers must never rewrite or delete such a file; it stays in
// git for manual resolution.
func ideaProblem(path, reason string) error {
	return &domain.ProblemError{Path: path, Kind: ports.RecordIdea, Reason: reason}
}

// readIdeaFile parses one idea file. projectID is the directory the file was
// found in; an idea whose frontmatter project differs is a problem, not a
// record that could silently move to another project on the next update.
func readIdeaFile(path, projectID string) (domain.Idea, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return domain.Idea{}, err
	}
	if reason := ideaConflictReason(raw); reason != "" {
		return domain.Idea{}, ideaProblem(path, reason)
	}
	var frontmatter ideaFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, ports.RecordIdea); err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	if frontmatter.Kind != ports.RecordIdea {
		return domain.Idea{}, ideaProblem(path,
			fmt.Sprintf("expected kind %q, found %q", ports.RecordIdea, frontmatter.Kind))
	}
	if err := checkProjectMatch(path, frontmatter.ProjectID, projectID); err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	id := strings.TrimSpace(frontmatter.ID)
	if id == "" {
		return domain.Idea{}, ideaProblem(path, "id is required")
	}
	if !domain.IsRecordID(ports.RecordIdea, id) {
		return domain.Idea{}, ideaProblem(path, fmt.Sprintf("%q is not an idea id", id))
	}
	if stem := strings.TrimSuffix(filepath.Base(path), ".md"); stem != id {
		return domain.Idea{}, ideaProblem(path,
			fmt.Sprintf("file name %q does not match id %q", stem, id))
	}
	dueAt, err := parseIdeaDueAt(frontmatter.DueAt)
	if err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	completedAt, err := parseIdeaCompletedAt(frontmatter.CompletedAt)
	if err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	validity, err := validityFromFrontmatter(frontmatter.validityFrontmatter, frontmatter.CreatedAt.UTC())
	if err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	relations, err := relationsFromFrontmatter(frontmatter.relationsFrontmatter, "idea.relations", id)
	if err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	merge, err := mergeFromFrontmatter(frontmatter.mergeFrontmatter, "idea.merge", id)
	if err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	review, err := reviewFromFrontmatter(frontmatter.reviewFrontmatter)
	if err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	taxonomy, err := taxonomyFromFrontmatter(frontmatter.taxonomyFrontmatter)
	if err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	idea := domain.Idea{
		ID:                  id,
		ProjectID:           strings.TrimSpace(frontmatter.ProjectID),
		Title:               frontmatter.Title,
		Body:                strings.TrimRight(body, "\n"),
		Status:              normalizedIdeaStatus(frontmatter.Status),
		Priority:            normalizedIdeaPriority(frontmatter.Priority),
		DueAt:               dueAt,
		DueDate:             strings.TrimSpace(frontmatter.DueDate),
		RemindBeforeMinutes: frontmatter.RemindBeforeMinutes,
		Tags:                nonNilStrings(frontmatter.Tags),
		Source:              ideaSourceFromFrontmatter(frontmatter.Source),
		CompletedAt:         completedAt,
		CompletionNote:      frontmatter.CompletionNote,
		Provenance:          provenanceFromFrontmatter(frontmatter.provenanceFrontmatter),
		Validity:            validity,
		MergeInfo:           merge,
		Taxonomy:            taxonomy,
		ReviewSchedule:      review,
		Relations:           relations,
		CreatedAt:           frontmatter.CreatedAt.UTC(),
		UpdatedAt:           frontmatter.UpdatedAt.UTC(),
	}
	idea.Provenance = idea.Provenance.Normalized()
	if idea.ProjectID == "" {
		return domain.Idea{}, ideaProblem(path, "project_id is required")
	}
	if err := idea.Validate(); err != nil {
		return domain.Idea{}, ideaProblem(path, err.Error())
	}
	return idea, nil
}

// scanIdeas reads every idea file of one project. Unparsable files are skipped
// and returned as problems, so one conflicted idea cannot hide the others. The
// project id is a directory name, not a registry lookup: an idea of a removed
// project stays readable.
func (s *Store) scanIdeas(ctx context.Context, projectID string) ([]domain.Idea, []Problem, error) {
	if err := contextError(ctx); err != nil {
		return nil, nil, err
	}
	projectPath, err := s.projectDir(projectID)
	if err != nil {
		return nil, nil, err
	}
	paths, err := recordPaths(filepath.Join(projectPath, ideasDir))
	if err != nil {
		return nil, nil, err
	}
	ideas := make([]domain.Idea, 0, len(paths))
	problems := make([]Problem, 0)
	for _, path := range paths {
		if err := contextError(ctx); err != nil {
			return nil, nil, err
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			problems = append(problems, Problem{Path: path, Kind: ports.RecordIdea, Reason: statErr.Error()})
			continue
		}
		if info.Size() == 0 {
			problems = append(problems, Problem{Path: path, Kind: ports.RecordIdea, Reason: emptyRecordReason(info.ModTime())})
			continue
		}
		idea, readErr := readIdeaFile(path, projectID)
		if readErr != nil {
			reason := readErr.Error()
			var problem *domain.ProblemError
			if errors.As(readErr, &problem) {
				reason = problem.Reason
			}
			problems = append(problems, Problem{Path: path, Kind: ports.RecordIdea, Reason: reason})
			continue
		}
		ideas = append(ideas, idea)
	}
	return ideas, problems, nil
}

// ideaProjectIDs resolves the project scope of a filter. An explicit id must be
// registered; an empty one means every registered project.
func (s *Store) ideaProjectIDs(ctx context.Context, projectID string) ([]string, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID != "" {
		if err := s.requireProject(projectID); err != nil {
			return nil, err
		}
		return []string{projectID}, nil
	}
	projects, err := s.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	return ids, nil
}

// ideaLookupProjectIDs is ideaProjectIDs plus orphan projects/*/ideas
// directories: an idea of a project removed from the registry must stay
// readable and is never deleted because a registry entry changed. Orphan ids
// are returned as stored directory names; no project is registered or invented.
func (s *Store) ideaLookupProjectIDs(ctx context.Context) ([]string, error) {
	ids, err := s.ideaProjectIDs(ctx, "")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	entries, err := os.ReadDir(filepath.Join(s.root, projectsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return ids, nil
		}
		return nil, fmt.Errorf("fsstore: read projects dir: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			continue
		}
		if err := validateProjectID(name); err != nil {
			// A stray directory is not a project; it must not break lookups
			// of every other idea.
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.root, projectsDir, name, ideasDir)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("fsstore: stat ideas dir of %s: %w", name, err)
		}
		ids = append(ids, name)
		seen[name] = struct{}{}
	}
	return ids, nil
}

// AppendIdea writes projects/<project>/ideas/<idea-id>.md create-only.
func (s *Store) AppendIdea(ctx context.Context, idea domain.Idea) (domain.SyncRevision, error) {
	if err := contextError(ctx); err != nil {
		return domain.SyncRevision{}, err
	}
	now := s.nowUTC()
	idea.Status = normalizedIdeaStatus(idea.Status)
	idea.Priority = normalizedIdeaPriority(idea.Priority)
	idea.Provenance = idea.Provenance.Normalized()
	if idea.CreatedAt.IsZero() {
		idea.CreatedAt = now
	} else {
		idea.CreatedAt = idea.CreatedAt.UTC()
	}
	if idea.UpdatedAt.IsZero() {
		idea.UpdatedAt = now
	} else {
		idea.UpdatedAt = idea.UpdatedAt.UTC()
	}
	if idea.ID == "" {
		id, err := domain.NewIdeaID(idea.CreatedAt, idea.Title)
		if err != nil {
			return domain.SyncRevision{}, err
		}
		idea.ID = id
	}
	if !domain.IsRecordID(ports.RecordIdea, idea.ID) {
		return domain.SyncRevision{}, domain.NewValidationError("idea.id",
			fmt.Sprintf("%q is not an idea id", idea.ID))
	}
	if err := idea.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	if err := s.requireProject(idea.ProjectID); err != nil {
		return domain.SyncRevision{}, err
	}
	dir, err := s.projectDir(idea.ProjectID)
	if err != nil {
		return domain.SyncRevision{}, err
	}
	raw, err := renderIdea(idea, idea.UpdatedAt)
	if err != nil {
		return domain.SyncRevision{}, err
	}
	if err := publishRecordBytesExact(filepath.Join(dir, ideasDir), ports.RecordIdea, idea.ID, raw); err != nil {
		return domain.SyncRevision{}, fmt.Errorf("fsstore: write idea %q: %w", idea.ID, err)
	}
	return revisionAfterWrite(s, ctx)
}

// ListIdeas returns the ideas matching filter in the contract order.
func (s *Store) ListIdeas(ctx context.Context, filter ports.IdeaFilter) ([]domain.Idea, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	projectIDs, err := s.ideaProjectIDs(ctx, filter.ProjectID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(filter.ProjectID) == "" {
		// A global list keeps orphan ideas visible; an explicit project filter
		// stays registry-validated so a typo never creates a scope.
		projectIDs, err = s.ideaLookupProjectIDs(ctx)
		if err != nil {
			return nil, err
		}
	}
	ideas := make([]domain.Idea, 0)
	for _, projectID := range projectIDs {
		projectIdeas, _, err := s.scanIdeas(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for _, idea := range projectIdeas {
			if ideaMatchesFilter(idea, filter) {
				ideas = append(ideas, idea)
			}
		}
	}
	sortIdeas(ideas)
	if filter.Limit > 0 && len(ideas) > filter.Limit {
		ideas = ideas[:filter.Limit]
	}
	return ideas, nil
}

// GetIdea finds an idea by its globally unique id. The id is the file name
// stem, so the lookup is one stat per registered project plus the orphan
// project directories that still hold idea files.
func (s *Store) GetIdea(ctx context.Context, id string) (domain.Idea, error) {
	if err := contextError(ctx); err != nil {
		return domain.Idea{}, err
	}
	id = strings.TrimSpace(id)
	if !domain.IsRecordID(ports.RecordIdea, id) {
		return domain.Idea{}, domain.NewValidationError("idea.id", fmt.Sprintf("%q is not an idea id", id))
	}
	projectIDs, err := s.ideaLookupProjectIDs(ctx)
	if err != nil {
		return domain.Idea{}, err
	}
	for _, projectID := range projectIDs {
		dir, err := s.projectDir(projectID)
		if err != nil {
			return domain.Idea{}, err
		}
		path := filepath.Join(dir, ideasDir, id+".md")
		info, statErr := os.Stat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return domain.Idea{}, fmt.Errorf("fsstore: stat idea %q: %w", id, statErr)
		}
		if info.Size() == 0 {
			return domain.Idea{}, ideaProblem(path, "empty file (interrupted write); not deleted automatically")
		}
		idea, readErr := readIdeaFile(path, projectID)
		if readErr != nil {
			return domain.Idea{}, fmt.Errorf("fsstore: idea %q: %w", id, readErr)
		}
		return idea, nil
	}
	return domain.Idea{}, notFound(ports.RecordIdea, id)
}

// findIdeaFile returns the project and path of a live idea file by id.
func (s *Store) findIdeaFile(ctx context.Context, id string) (string, string, error) {
	projectIDs, err := s.ideaLookupProjectIDs(ctx)
	if err != nil {
		return "", "", err
	}
	for _, projectID := range projectIDs {
		dir, err := s.projectDir(projectID)
		if err != nil {
			return "", "", err
		}
		path := filepath.Join(dir, ideasDir, id+".md")
		if _, err := os.Stat(path); err == nil {
			return projectID, path, nil
		} else if !os.IsNotExist(err) {
			return "", "", fmt.Errorf("fsstore: stat idea %q: %w", id, err)
		}
	}
	return "", "", notFound(ports.RecordIdea, id)
}

// UpdateIdea rewrites one idea in place, or moves the file when the project
// changes. created_at and the id are preserved; updated_at is the write time.
func (s *Store) UpdateIdea(ctx context.Context, idea domain.Idea) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	id := strings.TrimSpace(idea.ID)
	if !domain.IsRecordID(ports.RecordIdea, id) {
		return domain.NewValidationError("idea.id", fmt.Sprintf("%q is not an idea id", idea.ID))
	}
	idea.Status = normalizedIdeaStatus(idea.Status)
	idea.Priority = normalizedIdeaPriority(idea.Priority)
	idea.Provenance = idea.Provenance.Normalized()
	currentProject, currentPath, err := s.findIdeaFile(ctx, id)
	if err != nil {
		return err
	}
	current, err := readIdeaFile(currentPath, currentProject)
	if err != nil {
		return fmt.Errorf("fsstore: read idea %q: %w", id, err)
	}
	idea.CreatedAt = current.CreatedAt
	idea.UpdatedAt = s.nowUTC()
	if idea.ProjectID == "" {
		return domain.NewValidationError("idea.projectId", "is required")
	}
	if err := idea.Validate(); err != nil {
		return err
	}
	// An in-place edit of an idea whose project was removed from the registry
	// stays possible; only a move to a different project needs a registered id.
	if idea.ProjectID != currentProject {
		if err := s.requireProject(idea.ProjectID); err != nil {
			return err
		}
	}
	raw, err := renderIdea(idea, idea.UpdatedAt)
	if err != nil {
		return err
	}
	if currentProject == idea.ProjectID {
		return withFileLock(ctx, currentPath, func() error {
			if err := writeFileAtomic(currentPath, raw); err != nil {
				return fmt.Errorf("fsstore: write idea %q: %w", id, err)
			}
			return nil
		})
	}
	targetDir, err := s.projectDir(idea.ProjectID)
	if err != nil {
		return err
	}
	targetDir = filepath.Join(targetDir, ideasDir)
	if err := publishRecordBytesExact(targetDir, ports.RecordIdea, id, raw); err != nil {
		return fmt.Errorf("fsstore: move idea %q to project %s: %w", id, idea.ProjectID, err)
	}
	if err := removeFile(currentPath); err != nil {
		return fmt.Errorf("fsstore: idea %q was written to %s but the old file %s could not be removed: %w",
			id, filepath.Join(targetDir, id+".md"), currentPath, err)
	}
	return nil
}

// TrashIdea moves one live idea file to projects/<project>/trash/ideas/. The
// file is kept byte for byte; a name already present in the trash gains the
// usual numeric suffix.
func (s *Store) TrashIdea(ctx context.Context, projectID, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if !domain.IsRecordID(ports.RecordIdea, id) {
		return domain.NewValidationError("idea.id", fmt.Sprintf("%q is not an idea id", id))
	}
	if err := s.requireProject(projectID); err != nil {
		return err
	}
	projectDir, err := s.projectDir(projectID)
	if err != nil {
		return err
	}
	path := filepath.Join(projectDir, ideasDir, id+".md")
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return notFound(ports.RecordIdea, id)
		}
		return fmt.Errorf("fsstore: stat idea %q: %w", id, err)
	}
	// A problem file is never moved or rewritten: it stays in git for manual
	// resolution and the typed error names its path and reason.
	if _, err := readIdeaFile(path, projectID); err != nil {
		return err
	}
	trashKindDir := filepath.Join(projectDir, trashDir, ideasDir)
	return withFileLock(ctx, path, func() error {
		if err := os.MkdirAll(trashKindDir, 0o755); err != nil {
			return fmt.Errorf("fsstore: create trash %s: %w", trashKindDir, err)
		}
		target, err := freeTrashPath(trashKindDir, id+".md")
		if err != nil {
			return err
		}
		if err := renameFile(path, target); err != nil {
			return fmt.Errorf("fsstore: trash idea %q: %w", id, err)
		}
		return nil
	})
}

// RestoreIdea moves a trashed idea back. The live name is claimed with the
// same create-only publish a new record uses, so a live idea written after the
// trash is never overwritten.
func (s *Store) RestoreIdea(ctx context.Context, projectID, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	if !domain.IsRecordID(ports.RecordIdea, id) {
		return domain.NewValidationError("idea.id", fmt.Sprintf("%q is not an idea id", id))
	}
	if err := s.requireProject(projectID); err != nil {
		return err
	}
	projectDir, err := s.projectDir(projectID)
	if err != nil {
		return err
	}
	trashed, err := findTrashedIdea(filepath.Join(projectDir, trashDir, ideasDir), id)
	if err != nil {
		return err
	}
	liveDir := filepath.Join(projectDir, ideasDir)
	livePath := filepath.Join(liveDir, id+".md")
	if _, err := os.Stat(livePath); err == nil {
		return domain.NewTargetExistsError(ports.RecordIdea, id, livePath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("fsstore: stat idea %q: %w", id, err)
	}
	raw, err := os.ReadFile(trashed)
	if err != nil {
		if os.IsNotExist(err) {
			return notFound(ports.RecordIdea, id)
		}
		return fmt.Errorf("fsstore: read trashed idea %q: %w", id, err)
	}
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		return fmt.Errorf("fsstore: create %s: %w", liveDir, err)
	}
	if err := publishRecordBytesExact(liveDir, ports.RecordIdea, id, raw); err != nil {
		return err
	}
	if err := removeFile(trashed); err != nil {
		return fmt.Errorf("fsstore: restored idea %q but the trash copy could not be removed: %w", id, err)
	}
	return nil
}

// findTrashedIdea returns the trashed file of id. A repeated trash of the same
// idea gains a numeric suffix, so the plain name and "<id>-N" are both valid.
func findTrashedIdea(dir, id string) (string, error) {
	paths, err := recordPaths(dir)
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		stem := strings.TrimSuffix(filepath.Base(path), ".md")
		if ideaTrashStem(stem, id) {
			return path, nil
		}
	}
	return "", notFound(ports.RecordIdea, id)
}

func ideaTrashStem(stem, id string) bool {
	if stem == id {
		return true
	}
	rest, ok := strings.CutPrefix(stem, id+"-")
	if !ok || rest == "" {
		return false
	}
	_, err := strconv.Atoi(rest)
	return err == nil
}

// ideaMatchesFilter applies the filter semantics of ports.IdeaFilter.
func ideaMatchesFilter(idea domain.Idea, filter ports.IdeaFilter) bool {
	if len(filter.Statuses) > 0 {
		found := false
		for _, status := range filter.Statuses {
			if normalizedIdeaStatus(idea.Status) == status {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !ideaMatchesDueWindow(idea, filter.DueAfter, filter.DueBefore) {
		return false
	}
	if !ideaHasTags(idea, filter.Tags) {
		return false
	}
	if query := strings.ToLower(strings.TrimSpace(filter.Query)); query != "" {
		if !strings.Contains(strings.ToLower(idea.Title), query) &&
			!strings.Contains(strings.ToLower(idea.Body), query) {
			matched := false
			for _, tag := range idea.Tags {
				if strings.Contains(strings.ToLower(tag), query) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}

// ideaHasTags reports whether an idea carries every wanted tag. Matching is
// case-insensitive and exact (not a substring), and an empty want list keeps
// every idea.
func ideaHasTags(idea domain.Idea, wanted []string) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, want := range wanted {
		want = strings.TrimSpace(want)
		if want == "" {
			continue
		}
		found := false
		for _, tag := range idea.Tags {
			if strings.EqualFold(strings.TrimSpace(tag), want) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ideaMatchesDueWindow applies the half-open due window of the contract. A
// timed idea compares its instant; an all-day idea compares its local calendar
// date to the bound's local date. An undated idea is excluded when a bound is
// set.
func ideaMatchesDueWindow(idea domain.Idea, after, before time.Time) bool {
	if after.IsZero() && before.IsZero() {
		return true
	}
	if idea.DueAt != nil && !idea.DueAt.IsZero() {
		due := *idea.DueAt
		if !after.IsZero() && due.Before(after) {
			return false
		}
		if !before.IsZero() && !due.Before(before) {
			return false
		}
		return true
	}
	if idea.DueDate == "" {
		return false
	}
	if !after.IsZero() && idea.DueDate < after.In(time.Local).Format(domain.IdeaDateLayout) {
		return false
	}
	if !before.IsZero() && idea.DueDate >= before.In(time.Local).Format(domain.IdeaDateLayout) {
		return false
	}
	return true
}

// ideaDueInstant returns the effective due instant of an idea for sorting: the
// timed instant, or 09:00 local on an all-day date.
func ideaDueInstant(idea domain.Idea) (time.Time, bool) {
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

var ideaStatusRank = map[domain.IdeaStatus]int{
	domain.IdeaOpen:    0,
	domain.IdeaDoing:   1,
	domain.IdeaSomeday: 2,
	domain.IdeaDone:    3,
	domain.IdeaDropped: 4,
}

var ideaPriorityRank = map[domain.IdeaPriority]int{
	domain.IdeaPriorityHigh:   0,
	domain.IdeaPriorityNormal: 1,
	domain.IdeaPriorityLow:    2,
}

// sortIdeas applies the deterministic order of the contract.
func sortIdeas(ideas []domain.Idea) {
	sort.SliceStable(ideas, func(i, j int) bool {
		left, right := ideas[i], ideas[j]
		leftStatus := normalizedIdeaStatus(left.Status)
		rightStatus := normalizedIdeaStatus(right.Status)
		if ideaStatusRank[leftStatus] != ideaStatusRank[rightStatus] {
			return ideaStatusRank[leftStatus] < ideaStatusRank[rightStatus]
		}
		leftDue, leftDated := ideaDueInstant(left)
		rightDue, rightDated := ideaDueInstant(right)
		if leftDated != rightDated {
			return leftDated
		}
		if leftDated && !leftDue.Equal(rightDue) {
			return leftDue.Before(rightDue)
		}
		leftPriority := normalizedIdeaPriority(left.Priority)
		rightPriority := normalizedIdeaPriority(right.Priority)
		if ideaPriorityRank[leftPriority] != ideaPriorityRank[rightPriority] {
			return ideaPriorityRank[leftPriority] < ideaPriorityRank[rightPriority]
		}
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		return left.ID < right.ID
	})
}
