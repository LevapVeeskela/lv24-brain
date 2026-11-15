package fsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var _ ports.RecordRelationSetter = (*Store)(nil)

// relationFrontmatter is the on-disk form of one typed relation. The keys are
// snake_case, like the rest of the record frontmatter; the note marker uses
// the same keys through its JSON tags.
type relationFrontmatter struct {
	Target     string  `yaml:"target" json:"target"`
	Relation   string  `yaml:"relation" json:"relation"`
	Reason     string  `yaml:"reason,omitempty" json:"reason,omitempty"`
	Confidence float64 `yaml:"confidence,omitempty" json:"confidence,omitempty"`
	Status     string  `yaml:"status,omitempty" json:"status,omitempty"`
	JudgedAt   string  `yaml:"judged_at,omitempty" json:"judgedAt,omitempty"`
	VerdictBy  string  `yaml:"verdict_by,omitempty" json:"verdictBy,omitempty"`
}

// relationsFrontmatter embeds the relation list in every record frontmatter.
// An empty list writes nothing, so older records keep a clean file.
type relationsFrontmatter struct {
	Relations []relationFrontmatter `yaml:"relations,omitempty" json:"relations,omitempty"`
}

// relationsToFrontmatter projects the domain relations. Only non-default
// values are written: an accepted relation needs no status key, and an
// unjudged one needs no judged_at.
func relationsToFrontmatter(relations domain.RelationList) relationsFrontmatter {
	normalized := relations.Normalize()
	if len(normalized) == 0 {
		return relationsFrontmatter{}
	}
	out := relationsFrontmatter{Relations: make([]relationFrontmatter, 0, len(normalized))}
	for _, relation := range normalized {
		entry := relationFrontmatter{
			Target:     relation.Target,
			Relation:   string(relation.Relation),
			Reason:     relation.Reason,
			Confidence: relation.Confidence,
			VerdictBy:  string(relation.VerdictBy),
		}
		if relation.Status != domain.RelationAccepted {
			entry.Status = string(relation.Status)
		}
		if !relation.JudgedAt.IsZero() {
			entry.JudgedAt = formatItemTime(relation.JudgedAt)
		}
		out.Relations = append(out.Relations, entry)
	}
	return out
}

// relationsFromFrontmatter reads and validates the relation list. A malformed
// entry is an error: silently dropping it could turn a rejected conflict into
// an accepted one.
func relationsFromFrontmatter(relations relationsFrontmatter, field, selfID string) (domain.RelationList, error) {
	if len(relations.Relations) == 0 {
		return nil, nil
	}
	out := make(domain.RelationList, 0, len(relations.Relations))
	for i, entry := range relations.Relations {
		judgedAt, err := parseValidityTime(fmt.Sprintf("%s[%d].judged_at", field, i), entry.JudgedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, domain.Relation{
			Target:     entry.Target,
			Relation:   domain.RelationKind(entry.Relation),
			Reason:     entry.Reason,
			Confidence: entry.Confidence,
			Status:     domain.RelationStatus(entry.Status),
			JudgedAt:   judgedAt,
			VerdictBy:  domain.VerdictBy(entry.VerdictBy),
		})
	}
	out = out.Normalize()
	if err := out.Validate(field, selfID); err != nil {
		return nil, err
	}
	return out, nil
}

// mergeFrontmatter is the on-disk form of domain.MergeInfo.
type mergeFrontmatter struct {
	MergedFrom []string `yaml:"merged_from,omitempty" json:"mergedFrom,omitempty"`
	MergedInto string   `yaml:"merged_into,omitempty" json:"mergedInto,omitempty"`
}

// mergeToFrontmatter projects the merge history of a record.
func mergeToFrontmatter(merge domain.MergeInfo) mergeFrontmatter {
	merge = merge.Normalized()
	return mergeFrontmatter{MergedFrom: merge.MergedFrom, MergedInto: merge.MergedInto}
}

// mergeFromFrontmatter reads and validates the merge history. An empty record
// stays clean, so records written before merging existed keep their file.
func mergeFromFrontmatter(merge mergeFrontmatter, field, selfID string) (domain.MergeInfo, error) {
	out := domain.MergeInfo{MergedFrom: merge.MergedFrom, MergedInto: merge.MergedInto}.Normalized()
	if err := out.Validate(field, selfID); err != nil {
		return domain.MergeInfo{}, err
	}
	return out, nil
}

// SetRecordRelations replaces the relation list of one stored record. Only the
// relations key changes: the record text, id, creation time and updated_at
// stay as they are, so judging a pair is a small reviewable diff and does not
// make the record look newer. A note is a marker inside its daily file; its
// marker line is updated.
//
// It is idempotent: repeating the same list writes nothing and reports
// changed=false.
func (s *Store) SetRecordRelations(ctx context.Context, projectID, kind, id string, relations domain.RelationList) (bool, error) {
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
	relations = relations.Normalize()
	if err := relations.Validate("relations", id); err != nil {
		return false, err
	}
	dir, err := s.recordDir(ctx, projectID, kindDir)
	if err != nil {
		return false, err
	}
	if kind == ports.RecordNote {
		return s.setNoteRelations(ctx, dir, id, relations)
	}
	path, err := findRecordPath(dir, kind, id)
	if err != nil {
		return false, err
	}
	var changed bool
	err = withFileLock(ctx, path, func() error {
		var err error
		changed, err = rewriteRelationsFile(path, kind, id, relations)
		return err
	})
	return changed, err
}

// rewriteRelationsFile parses a decision, attempt or handoff file and writes it
// back with the new relation list, preserving the body byte for byte. The
// caller holds the lock for path.
func rewriteRelationsFile(path, kind, id string, relations domain.RelationList) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, notFound(kind, id)
		}
		return false, fmt.Errorf("fsstore: read %s %s: %w", kind, path, err)
	}
	target := relationsToFrontmatter(relations)
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
		if reflect.DeepEqual(frontmatter.relationsFrontmatter, target) {
			return false, nil
		}
		frontmatter.relationsFrontmatter = target
		return writeRelationsFile(path, frontmatter, body)
	case ports.RecordAttempt:
		var frontmatter attemptFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return false, fmt.Errorf("fsstore: parse attempt %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, kind); err != nil {
			return false, err
		}
		if reflect.DeepEqual(frontmatter.relationsFrontmatter, target) {
			return false, nil
		}
		frontmatter.relationsFrontmatter = target
		return writeRelationsFile(path, frontmatter, body)
	case ports.RecordHandoff:
		var frontmatter handoffFrontmatter
		body, err := markdown.Parse(raw, &frontmatter)
		if err != nil {
			return false, fmt.Errorf("fsstore: parse handoff %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, kind); err != nil {
			return false, err
		}
		if reflect.DeepEqual(frontmatter.relationsFrontmatter, target) {
			return false, nil
		}
		frontmatter.relationsFrontmatter = target
		return writeRelationsFile(path, frontmatter, body)
	default:
		return false, domain.NewValidationError("kind", fmt.Sprintf("record kind %q has no frontmatter relations", kind))
	}
}

func writeRelationsFile(path string, frontmatter any, body string) (bool, error) {
	rendered, err := markdown.Render(frontmatter, body)
	if err != nil {
		return false, fmt.Errorf("fsstore: render %s: %w", path, err)
	}
	if err := writeFileAtomic(path, rendered); err != nil {
		return false, fmt.Errorf("fsstore: write %s: %w", path, err)
	}
	return true, nil
}

// setNoteRelations rewrites one marker entry of a daily notes file. Hand-written
// sections have no marker to edit; they need a hand edit.
func (s *Store) setNoteRelations(ctx context.Context, dir, id string, relations domain.RelationList) (bool, error) {
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
		target := relationsToFrontmatter(relations)
		if reflect.DeepEqual(metadata.relationsFrontmatter, target) {
			return nil
		}
		metadata.relationsFrontmatter = target
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("fsstore: encode note %q: %w", id, err)
		}
		lines[start] = noteMarkerPrefix + string(encoded) + noteMarkerSuffix
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
