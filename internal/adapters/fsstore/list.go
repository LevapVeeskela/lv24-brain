package fsstore

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// ListNotes returns every note stored under projects/<id>/notes, newest first.
func (s *Store) ListNotes(ctx context.Context, projectID string) ([]domain.Note, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := s.requireProject(projectID); err != nil {
		return nil, err
	}
	dir, err := s.projectDir(projectID)
	if err != nil {
		return nil, err
	}
	paths, err := recordPaths(filepath.Join(dir, notesDir))
	if err != nil {
		return nil, err
	}
	notes := make([]domain.Note, 0, len(paths))
	for _, path := range paths {
		_, fileNotes, err := readNotesFile(path)
		if err != nil {
			return nil, err
		}
		notes = append(notes, fileNotes...)
	}
	sort.SliceStable(notes, func(i, j int) bool {
		if !notes[i].CreatedAt.Equal(notes[j].CreatedAt) {
			return notes[i].CreatedAt.After(notes[j].CreatedAt)
		}
		return notes[i].ID < notes[j].ID
	})
	return notes, nil
}

// ListDecisions returns every decision stored under projects/<id>/decisions,
// newest first.
func (s *Store) ListDecisions(ctx context.Context, projectID string) ([]domain.Decision, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := s.requireProject(projectID); err != nil {
		return nil, err
	}
	dir, err := s.projectDir(projectID)
	if err != nil {
		return nil, err
	}
	paths, err := recordPaths(filepath.Join(dir, decisionsDir))
	if err != nil {
		return nil, err
	}
	decisions := make([]domain.Decision, 0, len(paths))
	for _, path := range paths {
		decision, err := readDecisionFile(path)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	sort.SliceStable(decisions, func(i, j int) bool {
		if !decisions[i].CreatedAt.Equal(decisions[j].CreatedAt) {
			return decisions[i].CreatedAt.After(decisions[j].CreatedAt)
		}
		return decisions[i].ID < decisions[j].ID
	})
	return decisions, nil
}

// ListAttempts returns every attempt stored under projects/<id>/attempts,
// newest first.
func (s *Store) ListAttempts(ctx context.Context, projectID string) ([]domain.Attempt, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := s.requireProject(projectID); err != nil {
		return nil, err
	}
	dir, err := s.projectDir(projectID)
	if err != nil {
		return nil, err
	}
	paths, err := recordPaths(filepath.Join(dir, attemptsDir))
	if err != nil {
		return nil, err
	}
	attempts := make([]domain.Attempt, 0, len(paths))
	for _, path := range paths {
		attempt, err := readAttemptFile(path)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	sort.SliceStable(attempts, func(i, j int) bool {
		if !attempts[i].CreatedAt.Equal(attempts[j].CreatedAt) {
			return attempts[i].CreatedAt.After(attempts[j].CreatedAt)
		}
		return attempts[i].ID < attempts[j].ID
	})
	return attempts, nil
}

// ListHandoffs returns every handoff stored under projects/<id>/handoffs,
// newest first.
func (s *Store) ListHandoffs(ctx context.Context, projectID string) ([]domain.Handoff, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := s.requireProject(projectID); err != nil {
		return nil, err
	}
	dir, err := s.projectDir(projectID)
	if err != nil {
		return nil, err
	}
	paths, err := recordPaths(filepath.Join(dir, handoffsDir))
	if err != nil {
		return nil, err
	}
	handoffs := make([]domain.Handoff, 0, len(paths))
	for _, path := range paths {
		handoff, err := readHandoffFile(path)
		if err != nil {
			return nil, err
		}
		handoffs = append(handoffs, handoff)
	}
	sort.SliceStable(handoffs, func(i, j int) bool {
		if !handoffs[i].CreatedAt.Equal(handoffs[j].CreatedAt) {
			return handoffs[i].CreatedAt.After(handoffs[j].CreatedAt)
		}
		return handoffs[i].ID < handoffs[j].ID
	})
	return handoffs, nil
}

// recordPaths lists sorted Markdown record files in dir. A missing directory is
// an empty record set, matching a freshly created project.
func recordPaths(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, tempFilePrefix) {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), ".md") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	return paths, nil
}
