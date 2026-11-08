package fsstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// trashDir is the per-project folder that holds trashed records. Each kind
// keeps the name of its live directory: trash/notes, trash/decisions,
// trash/attempts and trash/handoffs.
const trashDir = "trash"

var _ ports.RecordEditor = (*Store)(nil)

// recordKinds lists the record kinds in listing order.
var recordKinds = []string{ports.RecordNote, ports.RecordDecision, ports.RecordAttempt, ports.RecordHandoff}

// recordKindDirs maps each record kind to its live directory name.
var recordKindDirs = map[string]string{
	ports.RecordNote:     notesDir,
	ports.RecordDecision: decisionsDir,
	ports.RecordAttempt:  attemptsDir,
	ports.RecordHandoff:  handoffsDir,
}

func recordKindDir(kind string) (string, error) {
	dir, ok := recordKindDirs[kind]
	if !ok {
		return "", domain.NewValidationError("kind", fmt.Sprintf("unknown record kind %q", kind))
	}
	return dir, nil
}

// UpdateDecision rewrites the decision file with the same ID in place. The
// file path, ID and created_at are kept; the body and status are re-rendered
// with the same layout AppendDecision uses.
func (s *Store) UpdateDecision(ctx context.Context, decision domain.Decision) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := domain.ValidateRequired("decision.id", strings.TrimSpace(decision.ID)); err != nil {
		return err
	}
	if decision.Status == "" {
		decision.Status = domain.DecisionProposed
	}
	if err := decision.Validate(); err != nil {
		return err
	}
	dir, err := s.recordDir(ctx, decision.ProjectID, decisionsDir)
	if err != nil {
		return err
	}
	path, err := findRecordPath(dir, ports.RecordDecision, decision.ID)
	if err != nil {
		return err
	}
	return withFileLock(ctx, path, func() error {
		current, err := readDecisionFile(path)
		if err != nil {
			return fmt.Errorf("fsstore: read decision %q: %w", decision.ID, err)
		}
		existing, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("fsstore: read decision %q: %w", decision.ID, err)
		}
		var oldFrontmatter decisionFrontmatter
		oldBody, err := markdown.Parse(existing, &oldFrontmatter)
		if err != nil {
			return fmt.Errorf("fsstore: parse decision %q: %w", decision.ID, err)
		}
		frontmatter := decisionFrontmatter{
			Frontmatter: markdown.Frontmatter{
				ProjectID:     current.ProjectID,
				Kind:          "decision",
				UpdatedAt:     s.nowUTC(),
				SchemaVersion: SchemaVersion,
				Permalink:     decision.Permalink,
			},
			ID:        current.ID,
			Title:     decision.Title,
			Status:    decision.Status,
			CreatedAt: current.CreatedAt,
		}
		if extras := decisionExtras(oldBody); extras != "" && !strings.Contains(decision.Context, extras) {
			decision.Context = strings.TrimSpace(strings.TrimSpace(decision.Context) + "\n\n" + extras)
		}
		raw, err := markdown.Render(frontmatter, renderDecisionBody(decision))
		if err != nil {
			return fmt.Errorf("fsstore: render decision %q: %w", decision.ID, err)
		}
		if err := writeFileAtomic(path, raw); err != nil {
			return fmt.Errorf("fsstore: write decision %q: %w", decision.ID, err)
		}
		return nil
	})
}

// decisionListSections are the list sections; text after them that is not a
// bullet under a known heading is dropped by parsing.
var decisionListSections = []string{"alternatives", "consequences", "evidence", "paths"}

// decisionExtras returns hand-written text that parsing a decision drops:
// paragraphs between the title/status and the first section, and sections
// with other headings placed after the list sections. UpdateDecision folds it
// into Context, where it survives later reads and updates.
func decisionExtras(body string) string {
	var kept []string
	started, afterLists, inExtra := false, false, false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if heading, ok := strings.CutPrefix(trimmed, "## "); ok {
			name := strings.ToLower(strings.TrimSpace(heading))
			started = true
			switch {
			case slices.Contains(decisionListSections, name):
				afterLists, inExtra = true, false
			case slices.Contains(decisionSections, name):
				inExtra = false
			case afterLists:
				inExtra = true
				kept = append(kept, line)
			}
			continue
		}
		switch {
		case inExtra:
			kept = append(kept, line)
		case !started:
			lower := strings.ToLower(trimmed)
			if trimmed != "" && !strings.HasPrefix(trimmed, "# ") && !strings.HasPrefix(lower, "status:") {
				kept = append(kept, line)
			}
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// TrashRecord moves a record into projects/<id>/trash/<dir>/. Decisions,
// attempts and handoffs move as whole files. A note is cut out of its daily
// file and appended, unchanged, to trash/notes/<date>.md.
func (s *Store) TrashRecord(ctx context.Context, projectID, kind, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	kindDir, err := recordKindDir(kind)
	if err != nil {
		return err
	}
	if err := domain.ValidateRequired("record.id", strings.TrimSpace(id)); err != nil {
		return err
	}
	liveDir, err := s.recordDir(ctx, projectID, kindDir)
	if err != nil {
		return err
	}
	if kind == ports.RecordNote {
		return s.trashNote(ctx, projectID, liveDir, id)
	}
	path, err := findRecordPath(liveDir, kind, id)
	if err != nil {
		return err
	}
	trashKindDir, err := s.recordDir(ctx, projectID, filepath.Join(trashDir, kindDir))
	if err != nil {
		return err
	}
	return withFileLock(ctx, path, func() error {
		if err := os.MkdirAll(trashKindDir, 0o755); err != nil {
			return fmt.Errorf("fsstore: create trash %s: %w", trashKindDir, err)
		}
		target, err := freeTrashPath(trashKindDir, filepath.Base(path))
		if err != nil {
			return err
		}
		if err := renameFile(path, target); err != nil {
			return fmt.Errorf("fsstore: trash %s %q: %w", kind, id, err)
		}
		return nil
	})
}

// RestoreRecord moves a trashed record back to its live directory under its
// original file name. A live record with the same ID, or a live file with the
// same name, yields ErrConflict.
func (s *Store) RestoreRecord(ctx context.Context, projectID, kind, id string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	kindDir, err := recordKindDir(kind)
	if err != nil {
		return err
	}
	if err := domain.ValidateRequired("record.id", strings.TrimSpace(id)); err != nil {
		return err
	}
	if kind == ports.RecordNote {
		return s.restoreNote(ctx, projectID, id)
	}
	trashKindDir, err := s.recordDir(ctx, projectID, filepath.Join(trashDir, kindDir))
	if err != nil {
		return err
	}
	trashed, err := findRecordPath(trashKindDir, kind, id)
	if err != nil {
		return err
	}
	liveDir, err := s.recordDir(ctx, projectID, kindDir)
	if err != nil {
		return err
	}
	if _, err := findRecordPath(liveDir, kind, id); err == nil {
		return conflictError(kind, id, "a live record already uses this ID")
	} else if !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	target := filepath.Join(liveDir, filepath.Base(trashed))
	return withFileLock(ctx, trashed, func() error {
		taken, err := pathExists(target)
		if err != nil {
			return err
		}
		if taken {
			return conflictError(kind, id, "the file name is taken in the live directory")
		}
		if err := os.MkdirAll(liveDir, 0o755); err != nil {
			return fmt.Errorf("fsstore: create %s: %w", liveDir, err)
		}
		if err := renameFile(trashed, target); err != nil {
			return fmt.Errorf("fsstore: restore %s %q: %w", kind, id, err)
		}
		return nil
	})
}

// ListTrash lists the trashed records of a project in kind order. Trash files
// that cannot be read are skipped, as listing skips unreadable live files.
func (s *Store) ListTrash(ctx context.Context, projectID string) ([]ports.TrashedRecord, error) {
	trashed := make([]ports.TrashedRecord, 0)
	for _, kind := range recordKinds {
		dir, err := s.recordDir(ctx, projectID, filepath.Join(trashDir, recordKindDirs[kind]))
		if err != nil {
			return nil, err
		}
		items, err := trashedRecords(kind, dir)
		if err != nil {
			return nil, err
		}
		trashed = append(trashed, items...)
	}
	return trashed, nil
}

func trashedRecords(kind, dir string) ([]ports.TrashedRecord, error) {
	paths, err := recordPaths(dir)
	if err != nil {
		return nil, err
	}
	items := make([]ports.TrashedRecord, 0, len(paths))
	for _, path := range paths {
		if kind == ports.RecordNote {
			_, notes, err := readNotesFile(path)
			if err != nil {
				continue
			}
			for _, note := range notes {
				items = append(items, ports.TrashedRecord{Kind: kind, ID: note.ID, Title: note.Title})
			}
			continue
		}
		id, title, err := readRecordHeader(kind, path)
		if err != nil {
			continue
		}
		items = append(items, ports.TrashedRecord{Kind: kind, ID: id, Title: title})
	}
	return items, nil
}

// readRecordHeader returns the ID and title of a decision, attempt or handoff
// file. Handoffs have no title, so one is derived from their creation time.
func readRecordHeader(kind, path string) (string, string, error) {
	switch kind {
	case ports.RecordDecision:
		decision, err := readDecisionFile(path)
		return decision.ID, decision.Title, err
	case ports.RecordAttempt:
		attempt, err := readAttemptFile(path)
		return attempt.ID, attempt.Title, err
	case ports.RecordHandoff:
		handoff, err := readHandoffFile(path)
		return handoff.ID, "Handoff " + handoff.CreatedAt.Format(time.RFC3339), err
	default:
		return "", "", domain.NewValidationError("kind", fmt.Sprintf("record kind %q has no file header", kind))
	}
}

// findRecordPath returns the file in dir whose frontmatter ID is id. Files that
// cannot be read are skipped. A missing record wraps domain.ErrNotFound.
func findRecordPath(dir, kind, id string) (string, error) {
	paths, err := recordPaths(dir)
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		recordID, _, err := readRecordHeader(kind, path)
		if err != nil || recordID != id {
			continue
		}
		return path, nil
	}
	return "", notFound(kind, id)
}

// freeTrashPath returns dir/name, or dir/<stem>-N<ext> for the first N from 2
// whose name is free.
func freeTrashPath(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for attempt := 1; attempt <= maxNameAttempts; attempt++ {
		candidate := name
		if attempt > 1 {
			candidate = fmt.Sprintf("%s-%d%s", stem, attempt, ext)
		}
		taken, err := pathExists(filepath.Join(dir, candidate))
		if err != nil {
			return "", err
		}
		if !taken {
			return filepath.Join(dir, candidate), nil
		}
	}
	return "", fmt.Errorf("fsstore: no free trash name for %s in %s", name, dir)
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("fsstore: stat %s: %w", path, err)
}

func conflictError(kind, id, reason string) error {
	return fmt.Errorf("fsstore: restore %s %q: %s: %w", kind, id, reason, domain.ErrConflict)
}

// trashNote moves one marker entry from its daily notes file to the trash.
// The entry is appended to the trash first and removed from the live file
// second, so a failure in between can duplicate a note but never lose it.
// The live daily file is kept even when no entries remain in it.
func (s *Store) trashNote(ctx context.Context, projectID, liveDir, id string) error {
	path, err := findNoteFile(liveDir, id)
	if err != nil {
		return err
	}
	trashNotesDir, err := s.recordDir(ctx, projectID, filepath.Join(trashDir, notesDir))
	if err != nil {
		return err
	}
	trashPath := filepath.Join(trashNotesDir, filepath.Base(path))
	now := s.nowUTC()
	return withFileLock(ctx, path, func() error {
		note, err := readNoteEntry(path, id)
		if err != nil {
			return err
		}
		note.ProjectID = projectID
		err = withFileLock(ctx, trashPath, func() error {
			return appendNoteLocked(trashPath, note, noteDateStem(path), note.CreatedAt, now)
		})
		if err != nil {
			return err
		}
		return removeNoteEntry(path, id, now)
	})
}

// restoreNote re-appends a trashed note entry to its daily notes file, then
// removes it from the trash. The entry keeps its ID and created_at.
func (s *Store) restoreNote(ctx context.Context, projectID, id string) error {
	trashNotesDir, err := s.recordDir(ctx, projectID, filepath.Join(trashDir, notesDir))
	if err != nil {
		return err
	}
	trashPath, err := findNoteFile(trashNotesDir, id)
	if err != nil {
		return err
	}
	liveNotes, err := s.ListNotes(ctx, projectID)
	if err != nil {
		return err
	}
	for _, live := range liveNotes {
		if live.ID == id {
			return conflictError(ports.RecordNote, id, "a live note already uses this ID")
		}
	}
	note, err := readNoteEntry(trashPath, id)
	if err != nil {
		return err
	}
	note.ProjectID = projectID
	liveDir, err := s.recordDir(ctx, projectID, notesDir)
	if err != nil {
		return err
	}
	livePath := filepath.Join(liveDir, filepath.Base(trashPath))
	now := s.nowUTC()
	err = withFileLock(ctx, livePath, func() error {
		return appendNoteLocked(livePath, note, noteDateStem(trashPath), note.CreatedAt, now)
	})
	if err != nil {
		return err
	}
	return withFileLock(ctx, trashPath, func() error {
		return removeNoteEntry(trashPath, id, now)
	})
}

// findNoteFile returns the daily notes file in dir that holds a note with id.
func findNoteFile(dir, id string) (string, error) {
	paths, err := recordPaths(dir)
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		_, notes, err := readNotesFile(path)
		if err != nil {
			continue
		}
		for _, note := range notes {
			if note.ID == id {
				return path, nil
			}
		}
	}
	return "", notFound(ports.RecordNote, id)
}

func noteDateStem(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".md")
}

// readNoteEntry returns the marker entry with id from the daily file at path.
// Hand-written sections have no marker and cannot be trashed or restored.
func readNoteEntry(path, id string) (domain.Note, error) {
	_, lines, err := loadNoteFile(path)
	if err != nil {
		return domain.Note{}, err
	}
	_, _, note, err := findNoteEntry(lines, id)
	return note, err
}

// removeNoteEntry rewrites the daily file at path without the entry with id.
// The caller must hold the lock for path.
func removeNoteEntry(path, id string, now time.Time) error {
	frontmatter, lines, err := loadNoteFile(path)
	if err != nil {
		return err
	}
	start, end, _, err := findNoteEntry(lines, id)
	if err != nil {
		return err
	}
	rest := make([]string, 0, len(lines)-(end-start))
	rest = append(rest, lines[:start]...)
	rest = append(rest, lines[end:]...)
	frontmatter.UpdatedAt = now
	rendered, err := markdown.Render(frontmatter, strings.Join(rest, "\n"))
	if err != nil {
		return fmt.Errorf("fsstore: render notes %s: %w", path, err)
	}
	if err := writeFileAtomic(path, rendered); err != nil {
		return fmt.Errorf("fsstore: write notes %s: %w", path, err)
	}
	return nil
}

// loadNoteFile parses a daily notes file into its frontmatter and body lines.
func loadNoteFile(path string) (noteFileFrontmatter, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return noteFileFrontmatter{}, nil, notFound(ports.RecordNote, filepath.Base(path))
		}
		return noteFileFrontmatter{}, nil, fmt.Errorf("fsstore: read notes %s: %w", path, err)
	}
	var frontmatter noteFileFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		return noteFileFrontmatter{}, nil, fmt.Errorf("fsstore: parse notes %s: %w", path, err)
	}
	if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, "note"); err != nil {
		return noteFileFrontmatter{}, nil, err
	}
	return frontmatter, strings.Split(body, "\n"), nil
}

// findNoteEntry returns the line range [start, end) of the marker entry with
// id, and the note it holds. An entry runs from its marker to the next marker.
func findNoteEntry(lines []string, id string) (int, int, domain.Note, error) {
	markers := make([]int, 0)
	for i, line := range lines {
		_, isMarker, err := parseNoteMarker(line)
		if err != nil {
			return 0, 0, domain.Note{}, fmt.Errorf("fsstore: parse notes: %w", err)
		}
		if isMarker {
			markers = append(markers, i)
		}
	}
	for k, start := range markers {
		end := len(lines)
		if k+1 < len(markers) {
			end = markers[k+1]
		}
		notes, err := parseNotesBody(strings.Join(lines[start:end], "\n"), time.Time{})
		if err != nil {
			return 0, 0, domain.Note{}, fmt.Errorf("fsstore: parse notes: %w", err)
		}
		if len(notes) == 1 && notes[0].ID == id {
			return start, end, notes[0], nil
		}
	}
	return 0, 0, domain.Note{}, notFound(ports.RecordNote, id)
}
