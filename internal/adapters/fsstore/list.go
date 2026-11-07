package fsstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// orphanReservationAge is how old an empty record file must be before it is
// reported as an orphaned reservation rather than a write in progress.
const orphanReservationAge = 60 * time.Second

// Problem describes one file under a project's record directories that listing
// skipped. Skipping keeps one damaged file from hiding every other record.
//
// Listing never deletes or repairs a file. Problems is the only place where
// skipped files are reported, so the app can show them to the user.
type Problem struct {
	// Path is the absolute path of the skipped file.
	Path string
	// Kind is the record kind: "note", "decision", "attempt" or "handoff".
	Kind string
	// Reason says why the file was skipped, in a form suitable for display.
	Reason string
}

// ListNotes returns every note stored under projects/<id>/notes, newest first.
// Files that cannot be read are skipped; see Problems.
func (s *Store) ListNotes(ctx context.Context, projectID string) ([]domain.Note, error) {
	notes, _, err := s.scanNotes(ctx, projectID)
	return notes, err
}

// ListDecisions returns every decision stored under projects/<id>/decisions,
// newest first. Files that cannot be read are skipped; see Problems.
func (s *Store) ListDecisions(ctx context.Context, projectID string) ([]domain.Decision, error) {
	decisions, _, err := s.scanDecisions(ctx, projectID)
	return decisions, err
}

// ListAttempts returns every attempt stored under projects/<id>/attempts,
// newest first. Files that cannot be read are skipped; see Problems.
func (s *Store) ListAttempts(ctx context.Context, projectID string) ([]domain.Attempt, error) {
	attempts, _, err := s.scanAttempts(ctx, projectID)
	return attempts, err
}

// ListHandoffs returns every handoff stored under projects/<id>/handoffs,
// newest first. Files that cannot be read are skipped; see Problems.
func (s *Store) ListHandoffs(ctx context.Context, projectID string) ([]domain.Handoff, error) {
	handoffs, _, err := s.scanHandoffs(ctx, projectID)
	return handoffs, err
}

// Problems reports the record files that the List methods skip for projectID:
// notes, decisions, attempts and handoffs that are unreadable, have malformed
// frontmatter, carry an incompatible kind or schema, or are zero bytes.
// Results are ordered by kind, then by path. A missing project is an error; a
// project without problems returns an empty slice.
func (s *Store) Problems(ctx context.Context, projectID string) ([]Problem, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := s.requireProject(projectID); err != nil {
		return nil, err
	}
	problems := make([]Problem, 0)
	_, noteProblems, err := s.scanNotes(ctx, projectID)
	if err != nil {
		return nil, err
	}
	_, decisionProblems, err := s.scanDecisions(ctx, projectID)
	if err != nil {
		return nil, err
	}
	_, attemptProblems, err := s.scanAttempts(ctx, projectID)
	if err != nil {
		return nil, err
	}
	_, handoffProblems, err := s.scanHandoffs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	problems = append(problems, noteProblems...)
	problems = append(problems, decisionProblems...)
	problems = append(problems, attemptProblems...)
	problems = append(problems, handoffProblems...)
	return problems, nil
}

func (s *Store) scanNotes(ctx context.Context, projectID string) ([]domain.Note, []Problem, error) {
	dir, err := s.recordDir(ctx, projectID, notesDir)
	if err != nil {
		return nil, nil, err
	}
	notes, problems, err := readRecords(ctx, dir, "note", func(path string) ([]domain.Note, error) {
		_, fileNotes, err := readNotesFile(path)
		return fileNotes, err
	})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(notes, func(i, j int) bool {
		if !notes[i].CreatedAt.Equal(notes[j].CreatedAt) {
			return notes[i].CreatedAt.After(notes[j].CreatedAt)
		}
		return notes[i].ID < notes[j].ID
	})
	return notes, problems, nil
}

func (s *Store) scanDecisions(ctx context.Context, projectID string) ([]domain.Decision, []Problem, error) {
	dir, err := s.recordDir(ctx, projectID, decisionsDir)
	if err != nil {
		return nil, nil, err
	}
	decisions, problems, err := readRecords(ctx, dir, "decision", func(path string) ([]domain.Decision, error) {
		decision, err := readDecisionFile(path)
		if err != nil {
			return nil, err
		}
		return []domain.Decision{decision}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(decisions, func(i, j int) bool {
		if !decisions[i].CreatedAt.Equal(decisions[j].CreatedAt) {
			return decisions[i].CreatedAt.After(decisions[j].CreatedAt)
		}
		return decisions[i].ID < decisions[j].ID
	})
	return decisions, problems, nil
}

func (s *Store) scanAttempts(ctx context.Context, projectID string) ([]domain.Attempt, []Problem, error) {
	dir, err := s.recordDir(ctx, projectID, attemptsDir)
	if err != nil {
		return nil, nil, err
	}
	attempts, problems, err := readRecords(ctx, dir, "attempt", func(path string) ([]domain.Attempt, error) {
		attempt, err := readAttemptFile(path)
		if err != nil {
			return nil, err
		}
		return []domain.Attempt{attempt}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(attempts, func(i, j int) bool {
		if !attempts[i].CreatedAt.Equal(attempts[j].CreatedAt) {
			return attempts[i].CreatedAt.After(attempts[j].CreatedAt)
		}
		return attempts[i].ID < attempts[j].ID
	})
	return attempts, problems, nil
}

func (s *Store) scanHandoffs(ctx context.Context, projectID string) ([]domain.Handoff, []Problem, error) {
	dir, err := s.recordDir(ctx, projectID, handoffsDir)
	if err != nil {
		return nil, nil, err
	}
	handoffs, problems, err := readRecords(ctx, dir, "handoff", func(path string) ([]domain.Handoff, error) {
		handoff, err := readHandoffFile(path)
		if err != nil {
			return nil, err
		}
		return []domain.Handoff{handoff}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(handoffs, func(i, j int) bool {
		if !handoffs[i].CreatedAt.Equal(handoffs[j].CreatedAt) {
			return handoffs[i].CreatedAt.After(handoffs[j].CreatedAt)
		}
		return handoffs[i].ID < handoffs[j].ID
	})
	return handoffs, problems, nil
}

// recordDir validates the context and project, then returns the record
// subdirectory of kindDir for projectID.
func (s *Store) recordDir(ctx context.Context, projectID, kindDir string) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if err := s.requireProject(projectID); err != nil {
		return "", err
	}
	dir, err := s.projectDir(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, kindDir), nil
}

// readRecords reads every record file in dir with read. A file that cannot be
// read or parsed is skipped and returned as a Problem instead of failing the
// whole listing. A zero-byte file is never parsed: it is an interrupted write,
// so it is skipped and reported, and never deleted.
func readRecords[T any](ctx context.Context, dir, kind string, read func(path string) ([]T, error)) ([]T, []Problem, error) {
	paths, err := recordPaths(dir)
	if err != nil {
		return nil, nil, err
	}
	items := make([]T, 0, len(paths))
	problems := make([]Problem, 0)
	for _, path := range paths {
		if err := contextError(ctx); err != nil {
			return nil, nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				// Removed after the directory was listed.
				continue
			}
			problems = append(problems, Problem{Path: path, Kind: kind, Reason: err.Error()})
			continue
		}
		if info.Size() == 0 {
			problems = append(problems, Problem{Path: path, Kind: kind, Reason: emptyRecordReason(info.ModTime())})
			continue
		}
		got, err := read(path)
		if err != nil {
			problems = append(problems, Problem{Path: path, Kind: kind, Reason: err.Error()})
			continue
		}
		items = append(items, got...)
	}
	return items, problems, nil
}

func emptyRecordReason(modTime time.Time) string {
	age := time.Since(modTime)
	if age >= orphanReservationAge {
		return fmt.Sprintf("empty file, orphaned reservation from an interrupted write (%s old); not deleted automatically", age.Truncate(time.Second))
	}
	return "empty file, possibly a write in progress or an interrupted write"
}

// recordPaths lists sorted Markdown record files in dir. A missing directory is
// an empty record set, matching a freshly created project. Temporary and lock
// files never end in ".md" or are excluded explicitly.
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
		if strings.HasPrefix(name, ".") {
			// Hidden names hold temporary and reservation files.
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
