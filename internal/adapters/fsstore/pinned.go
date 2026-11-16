package fsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var _ ports.RecordPinner = (*Store)(nil)

// SetRecordPinned sets or clears the pinned flag of one stored record. Only the
// pinned key changes: the record text, id, creation time and updated_at stay as
// they are, so pinning neither rewrites prose nor makes the record look newer.
// A note is a marker inside its daily file; its marker line is updated.
func (s *Store) SetRecordPinned(ctx context.Context, projectID, kind, id string, pinned bool) (bool, error) {
	if err := contextError(ctx); err != nil {
		return false, err
	}
	kindDir, err := recordKindDir(kind)
	if err != nil {
		return false, err
	}
	id = strings.TrimSpace(id)
	if err := domain.ValidateRequired("record.id", id); err != nil {
		return false, err
	}
	dir, err := s.recordDir(ctx, projectID, kindDir)
	if err != nil {
		return false, err
	}
	if kind == ports.RecordNote {
		return s.setNotePinned(ctx, dir, id, pinned)
	}
	path, err := findRecordPath(dir, kind, id)
	if err != nil {
		return false, err
	}
	var changed bool
	err = withFileLock(ctx, path, func() error {
		var err error
		changed, err = s.rewritePinnedFile(path, kind, id, pinned)
		return err
	})
	return changed, err
}

// rewritePinnedFile parses a decision, attempt or handoff file and writes it
// back with the new pinned flag, preserving the body byte for byte.
func (s *Store) rewritePinnedFile(path, kind, id string, pinned bool) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, notFound(kind, id)
		}
		return false, fmt.Errorf("fsstore: read %s %s: %w", kind, path, err)
	}
	switch kind {
	case ports.RecordDecision:
		var frontmatter decisionFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return false, fmt.Errorf("fsstore: parse decision %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, kind); err != nil {
			return false, err
		}
		if frontmatter.Pinned == pinned {
			return false, nil
		}
		frontmatter.Pinned = pinned
		frontmatter.SchemaVersion = SchemaVersion
		return writePinnedFile(path, frontmatter, body)
	case ports.RecordAttempt:
		var frontmatter attemptFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return false, fmt.Errorf("fsstore: parse attempt %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, kind); err != nil {
			return false, err
		}
		if frontmatter.Pinned == pinned {
			return false, nil
		}
		frontmatter.Pinned = pinned
		frontmatter.SchemaVersion = SchemaVersion
		return writePinnedFile(path, frontmatter, body)
	case ports.RecordHandoff:
		var frontmatter handoffFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return false, fmt.Errorf("fsstore: parse handoff %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, kind); err != nil {
			return false, err
		}
		if frontmatter.Pinned == pinned {
			return false, nil
		}
		frontmatter.Pinned = pinned
		frontmatter.SchemaVersion = SchemaVersion
		return writePinnedFile(path, frontmatter, body)
	default:
		return false, domain.NewValidationError("kind", fmt.Sprintf("record kind %q has no pinned flag", kind))
	}
}

func writePinnedFile(path string, frontmatter any, body string) (bool, error) {
	rendered, err := markdown.Render(frontmatter, body)
	if err != nil {
		return false, fmt.Errorf("fsstore: render %s: %w", path, err)
	}
	if err := writeFileAtomic(path, rendered); err != nil {
		return false, fmt.Errorf("fsstore: write %s: %w", path, err)
	}
	return true, nil
}

// setNotePinned rewrites one marker entry of a daily notes file.
func (s *Store) setNotePinned(ctx context.Context, dir, id string, pinned bool) (bool, error) {
	path, err := findNoteFile(dir, id)
	if err != nil {
		return false, err
	}
	var changed bool
	err = withFileLock(ctx, path, func() error {
		frontmatter, lines, err := loadNoteFile(path)
		if err != nil {
			return err
		}
		start, _, _, err := findNoteEntry(lines, id)
		if err != nil {
			return err
		}
		metadata, isMarker, err := parseNoteMarker(lines[start])
		if err != nil {
			return fmt.Errorf("fsstore: parse note %q: %w", id, err)
		}
		if !isMarker {
			return domain.NewValidationError("id",
				fmt.Sprintf("note %s is a hand-written section without an id marker; edit it by hand", id))
		}
		if metadata.Pinned == pinned {
			return nil
		}
		metadata.Pinned = pinned
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("fsstore: encode note %q: %w", id, err)
		}
		lines[start] = noteMarkerPrefix + string(encoded) + noteMarkerSuffix
		frontmatter.SchemaVersion = SchemaVersion
		rendered, err := markdown.Render(frontmatter, strings.Join(lines, "\n"))
		if err != nil {
			return fmt.Errorf("fsstore: render notes %s: %w", path, err)
		}
		if err := writeFileAtomic(path, rendered); err != nil {
			return fmt.Errorf("fsstore: write notes %s: %w", path, err)
		}
		changed = true
		return nil
	})
	return changed, err
}

// Aliases for compatibility with alternate naming.
func (s *Store) SetPinned(ctx context.Context, projectID, kind, id string, pinned bool) (bool, error) {
	return s.SetRecordPinned(ctx, projectID, kind, id, pinned)
}
func (s *Store) PinRecord(ctx context.Context, projectID, kind, id string) (bool, error) {
	return s.SetRecordPinned(ctx, projectID, kind, id, true)
}
func (s *Store) UnpinRecord(ctx context.Context, projectID, kind, id string) (bool, error) {
	return s.SetRecordPinned(ctx, projectID, kind, id, false)
}
func (s *Store) SetRecordPinnedBool(ctx context.Context, projectID, kind, id string, pinned bool) (bool, error) {
	return s.SetRecordPinned(ctx, projectID, kind, id, pinned)
}
