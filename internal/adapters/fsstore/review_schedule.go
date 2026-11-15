package fsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var _ ports.RecordScheduler = (*Store)(nil)

// SetReviewAfter moves the review date of one stored record. Only the
// review_after and reviewed_at keys change: the record text, id, creation time
// and updated_at stay as they are, so rescheduling a review neither rewrites
// prose nor makes the record look newer. A note is a marker inside its daily
// file; its marker line is updated.
func (s *Store) SetReviewAfter(ctx context.Context, projectID, kind, id string, at time.Time) (bool, error) {
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
	if !at.IsZero() {
		// The frontmatter keeps whole seconds; normalize before comparing so a
		// repeat of the same command is an idempotent no-op.
		at = at.UTC().Truncate(time.Second)
	}
	dir, err := s.recordDir(ctx, projectID, kindDir)
	if err != nil {
		return false, err
	}
	if kind == ports.RecordNote {
		return s.setNoteReviewAfter(ctx, dir, id, at)
	}
	path, err := findRecordPath(dir, kind, id)
	if err != nil {
		return false, err
	}
	var changed bool
	err = withFileLock(ctx, path, func() error {
		var err error
		changed, err = s.rewriteReviewFile(path, kind, id, at)
		return err
	})
	return changed, err
}

// rewriteReviewFile parses a decision, attempt or handoff file and writes it
// back with the new review date, preserving the body byte for byte. The caller
// holds the lock for path.
func (s *Store) rewriteReviewFile(path, kind, id string, at time.Time) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, notFound(kind, id)
		}
		return false, fmt.Errorf("fsstore: read %s %s: %w", kind, path, err)
	}
	reviewedAt := s.nowUTC()
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
		if sameReviewAfter(frontmatter.reviewFrontmatter, at) {
			return false, nil
		}
		frontmatter.reviewFrontmatter = reviewToFrontmatter(domain.ReviewSchedule{ReviewAfter: at, ReviewedAt: reviewedAt})
		frontmatter.SchemaVersion = SchemaVersion
		return writeReviewedFile(path, frontmatter, body)
	case ports.RecordAttempt:
		var frontmatter attemptFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return false, fmt.Errorf("fsstore: parse attempt %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, kind); err != nil {
			return false, err
		}
		if sameReviewAfter(frontmatter.reviewFrontmatter, at) {
			return false, nil
		}
		frontmatter.reviewFrontmatter = reviewToFrontmatter(domain.ReviewSchedule{ReviewAfter: at, ReviewedAt: reviewedAt})
		frontmatter.SchemaVersion = SchemaVersion
		return writeReviewedFile(path, frontmatter, body)
	case ports.RecordHandoff:
		var frontmatter handoffFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return false, fmt.Errorf("fsstore: parse handoff %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, kind); err != nil {
			return false, err
		}
		if sameReviewAfter(frontmatter.reviewFrontmatter, at) {
			return false, nil
		}
		frontmatter.reviewFrontmatter = reviewToFrontmatter(domain.ReviewSchedule{ReviewAfter: at, ReviewedAt: reviewedAt})
		frontmatter.SchemaVersion = SchemaVersion
		return writeReviewedFile(path, frontmatter, body)
	default:
		return false, domain.NewValidationError("kind", fmt.Sprintf("record kind %q has no review date", kind))
	}
}

func writeReviewedFile(path string, frontmatter any, body string) (bool, error) {
	rendered, err := markdown.Render(frontmatter, body)
	if err != nil {
		return false, fmt.Errorf("fsstore: render %s: %w", path, err)
	}
	if err := writeFileAtomic(path, rendered); err != nil {
		return false, fmt.Errorf("fsstore: write %s: %w", path, err)
	}
	return true, nil
}

// sameReviewAfter compares the stored date with the requested one. Both sides
// are whole seconds, so a repeat of the same command writes nothing instead of
// rewriting the reviewed_at stamp forever.
func sameReviewAfter(frontmatter reviewFrontmatter, at time.Time) bool {
	stored, err := parseValidityTime("review_after", frontmatter.ReviewAfter)
	if err != nil {
		return false
	}
	if at.IsZero() {
		return stored.IsZero()
	}
	return !stored.IsZero() && stored.UTC().Truncate(time.Second).Equal(at.UTC().Truncate(time.Second))
}

// setNoteReviewAfter rewrites one marker entry of a daily notes file.
// Hand-written sections have no marker to edit; they need a hand edit.
func (s *Store) setNoteReviewAfter(ctx context.Context, dir, id string, at time.Time) (bool, error) {
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
		if sameReviewAfter(metadata.reviewFrontmatter, at) {
			return nil
		}
		metadata.reviewFrontmatter = reviewToFrontmatter(domain.ReviewSchedule{ReviewAfter: at, ReviewedAt: s.nowUTC()})
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
