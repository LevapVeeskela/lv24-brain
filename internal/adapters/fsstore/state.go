package fsstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

type stateFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	FreshUntil           *time.Time `yaml:"fresh_until,omitempty"`
}

// GetState loads projects/<id>/state.md.
func (s *Store) GetState(ctx context.Context, projectID string) (domain.State, error) {
	if err := contextError(ctx); err != nil {
		return domain.State{}, err
	}
	dir, err := s.projectDir(projectID)
	if err != nil {
		return domain.State{}, err
	}
	path := filepath.Join(dir, stateFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return domain.State{}, notFound("state", projectID)
		}
		return domain.State{}, fmt.Errorf("fsstore: read state %q: %w", projectID, err)
	}
	var frontmatter stateFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		return domain.State{}, fmt.Errorf("fsstore: parse state %q: %w", projectID, err)
	}
	if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, "state"); err != nil {
		return domain.State{}, err
	}
	if err := checkProjectMatch(path, frontmatter.ProjectID, projectID); err != nil {
		return domain.State{}, err
	}
	sections := parseStateBody(body)
	state := domain.State{
		ProjectID:     projectID,
		Goal:          sections.goal,
		Active:        sections.active,
		Blocked:       sections.blocked,
		NextAction:    sections.nextAction,
		OpenQuestions: sections.openQuestions,
		UpdatedAt:     frontmatter.UpdatedAt.UTC(),
	}
	if frontmatter.FreshUntil != nil {
		state.FreshUntil = frontmatter.FreshUntil.UTC()
	}
	return state, nil
}

// PutState replaces projects/<id>/state.md and returns the new content
// revision. The project must already exist.
func (s *Store) PutState(ctx context.Context, state domain.State) (domain.SyncRevision, error) {
	if err := contextError(ctx); err != nil {
		return domain.SyncRevision{}, err
	}
	if err := state.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	dir, err := s.projectDir(state.ProjectID)
	if err != nil {
		return domain.SyncRevision{}, err
	}
	var revision domain.SyncRevision
	err = withFileLock(ctx, filepath.Join(dir, stateFile), func() error {
		revision, err = s.putStateLocked(ctx, dir, state)
		return err
	})
	return revision, err
}

// putStateLocked writes state.md in dir. The caller must hold the lock of
// state.md, so that a write cannot tear a concurrent rename of the same file.
func (s *Store) putStateLocked(ctx context.Context, dir string, state domain.State) (domain.SyncRevision, error) {
	project, err := s.loadProject(state.ProjectID)
	if err != nil {
		return domain.SyncRevision{}, err
	}

	state.Active = nonNilStrings(state.Active)
	state.Blocked = nonNilStrings(state.Blocked)
	state.OpenQuestions = nonNilStrings(state.OpenQuestions)
	// Keep a timestamp chosen by the caller (the app clock) so state and the
	// records written in the same operation share one timeline; stamp now only
	// when none was given.
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = s.nowUTC()
	} else {
		state.UpdatedAt = state.UpdatedAt.UTC()
	}

	frontmatter := stateFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID:     state.ProjectID,
			Kind:          "state",
			UpdatedAt:     state.UpdatedAt,
			SchemaVersion: SchemaVersion,
		},
	}
	if !state.FreshUntil.IsZero() {
		freshUntil := state.FreshUntil.UTC()
		frontmatter.FreshUntil = &freshUntil
	}
	raw, err := markdown.Render(frontmatter, renderStateBody(project.Name, state))
	if err != nil {
		return domain.SyncRevision{}, fmt.Errorf("fsstore: render state %q: %w", state.ProjectID, err)
	}
	if err := writeFileAtomic(filepath.Join(dir, stateFile), raw); err != nil {
		return domain.SyncRevision{}, fmt.Errorf("fsstore: write state %q: %w", state.ProjectID, err)
	}
	return s.revisionAfterWrite(ctx)
}

func renderStateBody(projectName string, state domain.State) string {
	title := strings.TrimSpace(projectName)
	if title == "" {
		title = state.ProjectID
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# %s — State\n\n", singleLine(title))
	out.WriteString("## Goal\n\n")
	out.WriteString(strings.TrimSpace(state.Goal))
	out.WriteString("\n\n## Active\n\n")
	writeStateList(&out, state.Active, "_Nothing active._")
	out.WriteString("\n## Blocked\n\n")
	writeStateList(&out, state.Blocked, "_Nothing blocked._")
	out.WriteString("\n## Next action\n\n")
	out.WriteString(strings.TrimSpace(state.NextAction))
	out.WriteString("\n\n## Open questions\n\n")
	writeStateList(&out, state.OpenQuestions, "_None._")
	return out.String()
}

func writeStateList(out *strings.Builder, items []string, fallback string) {
	if len(items) == 0 {
		out.WriteString(fallback)
		out.WriteByte('\n')
		return
	}
	for _, item := range items {
		writeListItem(out, item)
	}
}

func writeListItem(out *strings.Builder, item string) {
	lines := strings.Split(strings.TrimRight(item, "\n"), "\n")
	fmt.Fprintf(out, "- %s\n", lines[0])
	for _, line := range lines[1:] {
		fmt.Fprintf(out, "  %s\n", line)
	}
}

// UpdateState runs change on the stored state of projectID while holding the
// file lock of state.md, then writes the result. Concurrent UpdateState calls
// for one project therefore serialize and cannot lose each other's fields.
// A missing state.md starts from an empty state. The project must exist.
func (s *Store) UpdateState(ctx context.Context, projectID string, change func(*domain.State) error) (domain.State, error) {
	if err := contextError(ctx); err != nil {
		return domain.State{}, err
	}
	dir, err := s.projectDir(projectID)
	if err != nil {
		return domain.State{}, err
	}
	if _, err := s.loadProject(projectID); err != nil {
		return domain.State{}, err
	}
	var updated domain.State
	err = withFileLock(ctx, filepath.Join(dir, stateFile), func() error {
		state, err := s.GetState(ctx, projectID)
		if errors.Is(err, domain.ErrNotFound) {
			state = domain.State{ProjectID: projectID}
		} else if err != nil {
			return err
		}
		if err := change(&state); err != nil {
			return err
		}
		if _, err := s.putStateLocked(ctx, dir, state); err != nil {
			return err
		}
		updated = state
		return nil
	})
	if err != nil {
		return domain.State{}, err
	}
	return updated, nil
}
