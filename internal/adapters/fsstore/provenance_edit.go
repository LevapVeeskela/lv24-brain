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

var _ ports.RecordReviewer = (*Store)(nil)

// ReviewRecord marks one record reviewed and optionally sets its trust level.
// It is idempotent: a record that is already reviewed (and already carries the
// requested trust) is left untouched. Notes keep their provenance in the
// per-entry marker, so only that line is rewritten; every other entry in the
// daily file stays byte-for-byte the same.
func (s *Store) ReviewRecord(ctx context.Context, projectID, kind, id string, trust domain.Trust) (bool, error) {
	if err := contextError(ctx); err != nil {
		return false, err
	}
	if trust != "" && !trust.Valid() {
		return false, domain.NewValidationError("trust", fmt.Sprintf("unknown trust %q (verified, stated, inferred)", trust))
	}
	kindDir, err := recordKindDir(kind)
	if err != nil {
		return false, err
	}
	if err := domain.ValidateRequired("record.id", strings.TrimSpace(id)); err != nil {
		return false, err
	}
	dir, err := s.recordDir(ctx, projectID, kindDir)
	if err != nil {
		return false, err
	}
	if kind == ports.RecordNote {
		return s.reviewNote(ctx, projectID, dir, id, trust)
	}
	path, err := findRecordPath(dir, kind, id)
	if err != nil {
		return false, err
	}
	return s.reviewRecordFile(ctx, path, kind, id, trust)
}

// provenanceFile is one parsed decision, attempt or handoff file: its current
// provenance, the untouched body, and a renderer that writes new provenance
// into the same frontmatter.
type provenanceFile struct {
	provenance domain.Provenance
	body       string
	render     func(domain.Provenance) ([]byte, error)
}

func parseProvenanceFile(kind string, raw []byte) (provenanceFile, error) {
	switch kind {
	case ports.RecordDecision:
		var frontmatter decisionFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return provenanceFile{}, err
		}
		return provenanceFile{
			provenance: provenanceFromFrontmatter(frontmatter.provenanceFrontmatter),
			body:       body,
			render: func(provenance domain.Provenance) ([]byte, error) {
				frontmatter.provenanceFrontmatter = provenanceToFrontmatter(provenance)
				return markdown.Render(frontmatter, body)
			},
		}, nil
	case ports.RecordAttempt:
		var frontmatter attemptFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return provenanceFile{}, err
		}
		return provenanceFile{
			provenance: provenanceFromFrontmatter(frontmatter.provenanceFrontmatter),
			body:       body,
			render: func(provenance domain.Provenance) ([]byte, error) {
				frontmatter.provenanceFrontmatter = provenanceToFrontmatter(provenance)
				return markdown.Render(frontmatter, body)
			},
		}, nil
	case ports.RecordHandoff:
		var frontmatter handoffFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return provenanceFile{}, err
		}
		return provenanceFile{
			provenance: provenanceFromFrontmatter(frontmatter.provenanceFrontmatter),
			body:       body,
			render: func(provenance domain.Provenance) ([]byte, error) {
				frontmatter.provenanceFrontmatter = provenanceToFrontmatter(provenance)
				return markdown.Render(frontmatter, body)
			},
		}, nil
	default:
		return provenanceFile{}, domain.NewValidationError("kind", fmt.Sprintf("record kind %q has no provenance", kind))
	}
}

// reviewRecordFile rewrites only the provenance keys of one record file. The
// body and every other frontmatter value are kept as they are, including
// created_at and updated_at: a review does not make a record look newer.
func (s *Store) reviewRecordFile(ctx context.Context, path, kind, id string, trust domain.Trust) (changed bool, err error) {
	err = withFileLock(ctx, path, func() error {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return notFound(kind, id)
			}
			return fmt.Errorf("fsstore: read %s %q: %w", kind, id, err)
		}
		parsed, err := parseProvenanceFile(kind, raw)
		if err != nil {
			return fmt.Errorf("fsstore: parse %s %q: %w", kind, id, err)
		}
		if alreadyReviewed(parsed.provenance, trust) {
			return nil
		}
		rendered, err := parsed.render(reviewedProvenance(parsed.provenance, trust))
		if err != nil {
			return fmt.Errorf("fsstore: render %s %q: %w", kind, id, err)
		}
		if err := writeFileAtomic(path, rendered); err != nil {
			return fmt.Errorf("fsstore: write %s %q: %w", kind, id, err)
		}
		changed = true
		return nil
	})
	return changed, err
}

// reviewNote rewrites the marker of one note entry. A hand-written section has
// no marker to change and is refused with instructions to edit it by hand.
func (s *Store) reviewNote(ctx context.Context, projectID, dir, id string, trust domain.Trust) (changed bool, err error) {
	path, err := findNoteFile(dir, id)
	if err != nil {
		return false, err
	}
	err = withFileLock(ctx, path, func() error {
		frontmatter, lines, err := loadNoteFile(path)
		if err != nil {
			return err
		}
		for i, line := range lines {
			metadata, isMarker, err := parseNoteMarker(line)
			if err != nil {
				return fmt.Errorf("fsstore: parse notes %s: %w", path, err)
			}
			if !isMarker || metadata.ID != id {
				continue
			}
			current := provenanceFromFrontmatter(metadata.provenanceFrontmatter)
			if alreadyReviewed(current, trust) {
				return nil
			}
			metadata.provenanceFrontmatter = provenanceToFrontmatter(reviewedProvenance(current, trust))
			encoded, err := json.Marshal(metadata)
			if err != nil {
				return fmt.Errorf("fsstore: render note marker %s: %w", id, err)
			}
			lines[i] = noteMarkerPrefix + string(encoded) + noteMarkerSuffix
			rendered, err := markdown.Render(frontmatter, strings.Join(lines, "\n"))
			if err != nil {
				return fmt.Errorf("fsstore: render notes %s: %w", path, err)
			}
			if err := writeFileAtomic(path, rendered); err != nil {
				return fmt.Errorf("fsstore: write notes %s: %w", path, err)
			}
			changed = true
			return nil
		}
		return domain.NewValidationError("id", fmt.Sprintf(
			"note %s is a hand-written section without an id marker; edit projects/%s/notes/ by hand", id, projectID))
	})
	return changed, err
}

// alreadyReviewed reports whether a review would not change anything: the
// record is reviewed and either keeps its trust or already has the requested
// one.
func alreadyReviewed(provenance domain.Provenance, trust domain.Trust) bool {
	if !provenance.ReviewedState() {
		return false
	}
	return trust == "" || provenance.Trust == trust
}

func reviewedProvenance(provenance domain.Provenance, trust domain.Trust) domain.Provenance {
	provenance.Reviewed = true
	if trust != "" {
		provenance.Trust = trust
	}
	return provenance
}
