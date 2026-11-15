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

var _ ports.RecordValiditySetter = (*Store)(nil)

// SetRecordValidity rewrites the validity fields of one stored record in
// place. Only frontmatter changes: the record text, id and creation time stay
// as they are, so a supersede is a reviewable git diff with no rewritten
// prose. A note is a marker inside its daily file; its marker line is updated.
func (s *Store) SetRecordValidity(ctx context.Context, projectID, kind, id string, validity domain.Validity) error {
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
	dir, err := s.recordDir(ctx, projectID, kindDir)
	if err != nil {
		return err
	}
	if kind == ports.RecordNote {
		return s.setNoteValidity(ctx, dir, strings.TrimSpace(id), validity)
	}
	path, err := findRecordPath(dir, kind, strings.TrimSpace(id))
	if err != nil {
		return err
	}
	return withFileLock(ctx, path, func() error {
		return s.rewriteValidityFile(path, kind, strings.TrimSpace(id), validity)
	})
}

// rewriteValidityFile parses a decision, attempt or handoff file and writes it
// back with the new validity, preserving the body byte for byte. The caller
// holds the lock for path.
func (s *Store) rewriteValidityFile(path, kind, id string, validity domain.Validity) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return notFound(kind, id)
		}
		return fmt.Errorf("fsstore: read %s %s: %w", kind, path, err)
	}

	switch kind {
	case ports.RecordDecision:
		var frontmatter decisionFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return fmt.Errorf("fsstore: parse decision %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, kind); err != nil {
			return err
		}
		createdAt := frontmatter.CreatedAt.UTC()
		validity = validity.NormalizedFrom(createdAt)
		if err := validity.Validate("decision.validity", createdAt, frontmatter.ID); err != nil {
			return err
		}
		frontmatter.validityFrontmatter = validityToFrontmatter(validity, createdAt)
		frontmatter.UpdatedAt = s.nowUTC()
		frontmatter.SchemaVersion = SchemaVersion
		rendered, err := markdown.Render(frontmatter, body)
		if err != nil {
			return fmt.Errorf("fsstore: render decision %s: %w", path, err)
		}
		return writeFileAtomic(path, rendered)
	case ports.RecordAttempt:
		var frontmatter attemptFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return fmt.Errorf("fsstore: parse attempt %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, kind); err != nil {
			return err
		}
		createdAt := frontmatter.CreatedAt.UTC()
		validity = validity.NormalizedFrom(createdAt)
		if err := validity.Validate("attempt.validity", createdAt, frontmatter.ID); err != nil {
			return err
		}
		frontmatter.validityFrontmatter = validityToFrontmatter(validity, createdAt)
		frontmatter.UpdatedAt = s.nowUTC()
		frontmatter.SchemaVersion = SchemaVersion
		rendered, err := markdown.Render(frontmatter, body)
		if err != nil {
			return fmt.Errorf("fsstore: render attempt %s: %w", path, err)
		}
		return writeFileAtomic(path, rendered)
	case ports.RecordHandoff:
		var frontmatter handoffFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return fmt.Errorf("fsstore: parse handoff %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, kind); err != nil {
			return err
		}
		createdAt := frontmatter.CreatedAt.UTC()
		validity = validity.NormalizedFrom(createdAt)
		if err := validity.Validate("handoff.validity", createdAt, frontmatter.ID); err != nil {
			return err
		}
		frontmatter.validityFrontmatter = validityToFrontmatter(validity, createdAt)
		frontmatter.UpdatedAt = s.nowUTC()
		frontmatter.SchemaVersion = SchemaVersion
		rendered, err := markdown.Render(frontmatter, body)
		if err != nil {
			return fmt.Errorf("fsstore: render handoff %s: %w", path, err)
		}
		return writeFileAtomic(path, rendered)
	default:
		return domain.NewValidationError("kind", fmt.Sprintf("record kind %q has no frontmatter validity", kind))
	}
}

// setNoteValidity rewrites one marker entry of a daily notes file. Hand-written
// sections have no marker to edit; they need a hand edit.
func (s *Store) setNoteValidity(ctx context.Context, dir, id string, validity domain.Validity) error {
	path, err := findNoteFile(dir, id)
	if err != nil {
		return err
	}
	return withFileLock(ctx, path, func() error {
		frontmatter, lines, err := loadNoteFile(path)
		if err != nil {
			return err
		}
		start, _, note, err := findNoteEntry(lines, id)
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
		createdAt := note.CreatedAt.UTC()
		validity = validity.NormalizedFrom(createdAt)
		if err := validity.Validate("note.validity", createdAt, id); err != nil {
			return err
		}
		metadata.validityFrontmatter = validityToFrontmatter(validity, createdAt)
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("fsstore: encode note %q: %w", id, err)
		}
		lines[start] = noteMarkerPrefix + string(encoded) + noteMarkerSuffix
		frontmatter.UpdatedAt = s.nowUTC()
		frontmatter.SchemaVersion = SchemaVersion
		rendered, err := markdown.Render(frontmatter, strings.Join(lines, "\n"))
		if err != nil {
			return fmt.Errorf("fsstore: render notes %s: %w", path, err)
		}
		if err := writeFileAtomic(path, rendered); err != nil {
			return fmt.Errorf("fsstore: write notes %s: %w", path, err)
		}
		return nil
	})
}
