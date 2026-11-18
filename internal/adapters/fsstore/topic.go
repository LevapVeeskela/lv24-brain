package fsstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var _ ports.TopicReplacer = (*Store)(nil)
var _ ports.TopicLocker = (*Store)(nil)

// topicLocksDir is the per-project directory of topic lock files. It holds no
// record files: locks are removed after the write and the whole directory is
// hidden, so it never appears in a listing, a revision or a daily notes file.
const topicLocksDir = ".topics"

// LockTopic serializes a topic upsert across processes. The lock is scoped to
// one (project, kind, topic key), so two records of different topics, or the
// same topic in two projects, never wait for each other. The returned release
// function must always be called.
func (s *Store) LockTopic(ctx context.Context, projectID, kind, topicKey string) (func(), error) {
	dir, err := s.projectDir(projectID)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + topicKey))
	target := filepath.Join(dir, topicLocksDir, kind+"-"+hex.EncodeToString(sum[:8]))
	return acquireFileLock(ctx, target)
}

// ReplaceNote rewrites one marker entry of its daily notes file in place,
// keeping the entry ID, creation time and provenance. The caller supplies the
// new text and the bounded history; the store never invents history itself.
// It is the last-write-wins form of replaceNote; UpdateRecordText adds the
// compare-and-swap used by record editing.
func (s *Store) ReplaceNote(ctx context.Context, note domain.Note) (domain.Note, error) {
	return s.replaceNote(ctx, note, time.Time{})
}

func (s *Store) replaceNote(ctx context.Context, note domain.Note, expectedUpdatedAt time.Time) (domain.Note, error) {
	if err := contextError(ctx); err != nil {
		return domain.Note{}, err
	}
	if err := domain.ValidateRequired("note.id", strings.TrimSpace(note.ID)); err != nil {
		return domain.Note{}, err
	}
	if err := note.Validate(); err != nil {
		return domain.Note{}, err
	}
	dir, err := s.recordDir(ctx, note.ProjectID, notesDir)
	if err != nil {
		return domain.Note{}, err
	}
	path, err := findNoteFile(dir, note.ID)
	if err != nil {
		return domain.Note{}, err
	}
	now := s.nowUTC()
	err = withFileLock(ctx, path, func() error {
		frontmatter, lines, err := loadNoteFile(path)
		if err != nil {
			return err
		}
		if err := checkRecordRevision(ports.RecordNote, note.ID, frontmatter.UpdatedAt, expectedUpdatedAt); err != nil {
			return err
		}
		updatedAt := nextRecordUpdatedAt(frontmatter.UpdatedAt, now)
		start, end, current, err := findNoteEntry(lines, note.ID)
		if err != nil {
			return err
		}
		contentAt := contentUpdatedAt(frontmatter.Frontmatter)
		if noteTextChanged(current, note) {
			contentAt = updatedAt
		}
		note.CreatedAt = current.CreatedAt
		note.UpdatedAt = updatedAt
		note.ContentUpdatedAt = contentAt
		entry := renderNoteEntry(noteMetadata{
			provenanceFrontmatter: provenanceToFrontmatter(current.Provenance),
			validityFrontmatter:   validityToFrontmatter(note.Validity, current.CreatedAt),
			relationsFrontmatter:  relationsToFrontmatter(relationsOrStored(note.Relations, current.Relations)),
			mergeFrontmatter:      mergeToFrontmatter(current.MergeInfo),
			reviewFrontmatter:     reviewToFrontmatter(note.ReviewSchedule),
			taxonomyFrontmatter:   taxonomyToFrontmatter(note.Taxonomy),
			ID:                    current.ID,
			Title:                 note.Title,
			Tags:                  nonNilStrings(note.Tags),
			Permalink:             note.Permalink,
			Source:                current.Source,
			CreatedAt:             current.CreatedAt,
		}, appendHistory(note.Body, note.History))
		replacement := strings.Split(strings.TrimRight(entry, "\n"), "\n")
		if end < len(lines) {
			replacement = append(replacement, "")
		}
		next := make([]string, 0, len(lines)-(end-start)+len(replacement))
		next = append(next, lines[:start]...)
		next = append(next, replacement...)
		next = append(next, lines[end:]...)
		frontmatter.UpdatedAt = updatedAt
		frontmatter.ContentUpdatedAt = contentTimePtr(contentAt)
		frontmatter.SchemaVersion = SchemaVersion
		rendered, err := markdown.Render(frontmatter, strings.Join(next, "\n"))
		if err != nil {
			return fmt.Errorf("fsstore: render notes %s: %w", path, err)
		}
		if err := writeFileAtomic(path, rendered); err != nil {
			return fmt.Errorf("fsstore: write notes %s: %w", path, err)
		}
		return nil
	})
	if err != nil {
		return domain.Note{}, err
	}
	note.TopicHistory = domain.TopicHistory{History: domain.BoundedHistory(note.History)}
	return note, nil
}

// ReplaceDecision rewrites one decision file in place, keeping its ID,
// creation time and provenance. The body is re-rendered with the bounded
// history the caller supplies.
func (s *Store) ReplaceDecision(ctx context.Context, decision domain.Decision) (domain.Decision, error) {
	return s.replaceDecision(ctx, decision, time.Time{})
}

func (s *Store) replaceDecision(ctx context.Context, decision domain.Decision, expectedUpdatedAt time.Time) (domain.Decision, error) {
	if err := contextError(ctx); err != nil {
		return domain.Decision{}, err
	}
	if err := domain.ValidateRequired("decision.id", strings.TrimSpace(decision.ID)); err != nil {
		return domain.Decision{}, err
	}
	if decision.Status == "" {
		decision.Status = domain.DecisionProposed
	}
	if err := decision.Validate(); err != nil {
		return domain.Decision{}, err
	}
	dir, err := s.recordDir(ctx, decision.ProjectID, decisionsDir)
	if err != nil {
		return domain.Decision{}, err
	}
	path, err := findRecordPath(dir, ports.RecordDecision, decision.ID)
	if err != nil {
		return domain.Decision{}, err
	}
	now := s.nowUTC()
	err = withFileLock(ctx, path, func() error {
		current, err := readDecisionFile(path)
		if err != nil {
			return fmt.Errorf("fsstore: read decision %q: %w", decision.ID, err)
		}
		if err := checkRecordRevision(ports.RecordDecision, decision.ID, current.UpdatedAt, expectedUpdatedAt); err != nil {
			return err
		}
		updatedAt := nextRecordUpdatedAt(current.UpdatedAt, now)
		contentAt := current.ContentUpdatedAt
		if decisionTextChanged(current, decision) {
			contentAt = updatedAt
		}
		decision.ProjectID = current.ProjectID
		decision.CreatedAt = current.CreatedAt
		decision.UpdatedAt = updatedAt
		decision.ContentUpdatedAt = contentAt
		frontmatter := decisionFrontmatter{
			Frontmatter: markdown.Frontmatter{
				ProjectID:        current.ProjectID,
				Kind:             "decision",
				UpdatedAt:        updatedAt,
				ContentUpdatedAt: contentTimePtr(contentAt),
				SchemaVersion:    SchemaVersion,
				Permalink:        decision.Permalink,
			},
			provenanceFrontmatter: provenanceToFrontmatter(current.Provenance),
			validityFrontmatter:   validityToFrontmatter(decision.Validity, current.CreatedAt),
			relationsFrontmatter:  relationsToFrontmatter(relationsOrStored(decision.Relations, current.Relations)),
			mergeFrontmatter:      mergeToFrontmatter(current.MergeInfo),
			reviewFrontmatter:     reviewToFrontmatter(decision.ReviewSchedule),
			taxonomyFrontmatter:   taxonomyToFrontmatter(decision.Taxonomy),
			ID:                    current.ID,
			Title:                 decision.Title,
			Status:                decision.Status,
			Source:                sourceRefToFrontmatter(current.Source),
			CreatedAt:             current.CreatedAt,
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
	if err != nil {
		return domain.Decision{}, err
	}
	decision.TopicHistory = domain.TopicHistory{History: domain.BoundedHistory(decision.History)}
	return decision, nil
}

// ReplaceAttempt rewrites one attempt file in place, keeping its ID, creation
// time and provenance.
func (s *Store) ReplaceAttempt(ctx context.Context, attempt domain.Attempt) (domain.Attempt, error) {
	return s.replaceAttempt(ctx, attempt, time.Time{})
}

func (s *Store) replaceAttempt(ctx context.Context, attempt domain.Attempt, expectedUpdatedAt time.Time) (domain.Attempt, error) {
	if err := contextError(ctx); err != nil {
		return domain.Attempt{}, err
	}
	if err := domain.ValidateRequired("attempt.id", strings.TrimSpace(attempt.ID)); err != nil {
		return domain.Attempt{}, err
	}
	if err := attempt.Validate(); err != nil {
		return domain.Attempt{}, err
	}
	dir, err := s.recordDir(ctx, attempt.ProjectID, attemptsDir)
	if err != nil {
		return domain.Attempt{}, err
	}
	path, err := findRecordPath(dir, ports.RecordAttempt, attempt.ID)
	if err != nil {
		return domain.Attempt{}, err
	}
	now := s.nowUTC()
	err = withFileLock(ctx, path, func() error {
		current, err := readAttemptFile(path)
		if err != nil {
			return fmt.Errorf("fsstore: read attempt %q: %w", attempt.ID, err)
		}
		if err := checkRecordRevision(ports.RecordAttempt, attempt.ID, current.UpdatedAt, expectedUpdatedAt); err != nil {
			return err
		}
		updatedAt := nextRecordUpdatedAt(current.UpdatedAt, now)
		contentAt := current.ContentUpdatedAt
		if attemptTextChanged(current, attempt) {
			contentAt = updatedAt
		}
		attempt.ProjectID = current.ProjectID
		attempt.CreatedAt = current.CreatedAt
		attempt.UpdatedAt = updatedAt
		attempt.ContentUpdatedAt = contentAt
		frontmatter := attemptFrontmatter{
			Frontmatter: markdown.Frontmatter{
				ProjectID:        current.ProjectID,
				Kind:             "attempt",
				UpdatedAt:        updatedAt,
				ContentUpdatedAt: contentTimePtr(contentAt),
				SchemaVersion:    SchemaVersion,
				Permalink:        attempt.Permalink,
			},
			provenanceFrontmatter: provenanceToFrontmatter(current.Provenance),
			validityFrontmatter:   validityToFrontmatter(attempt.Validity, current.CreatedAt),
			relationsFrontmatter:  relationsToFrontmatter(relationsOrStored(attempt.Relations, current.Relations)),
			mergeFrontmatter:      mergeToFrontmatter(current.MergeInfo),
			reviewFrontmatter:     reviewToFrontmatter(attempt.ReviewSchedule),
			taxonomyFrontmatter:   taxonomyToFrontmatter(attempt.Taxonomy),
			ID:                    current.ID,
			Title:                 attempt.Title,
			Source:                sourceRefToFrontmatter(current.Source),
			CreatedAt:             current.CreatedAt,
		}
		raw, err := markdown.Render(frontmatter, renderAttemptBody(attempt))
		if err != nil {
			return fmt.Errorf("fsstore: render attempt %q: %w", attempt.ID, err)
		}
		if err := writeFileAtomic(path, raw); err != nil {
			return fmt.Errorf("fsstore: write attempt %q: %w", attempt.ID, err)
		}
		return nil
	})
	if err != nil {
		return domain.Attempt{}, err
	}
	attempt.TopicHistory = domain.TopicHistory{History: domain.BoundedHistory(attempt.History)}
	return attempt, nil
}

// ReplaceHandoff rewrites one handoff file in place, keeping its ID, creation
// time and provenance.
func (s *Store) ReplaceHandoff(ctx context.Context, handoff domain.Handoff) (domain.Handoff, error) {
	return s.replaceHandoff(ctx, handoff, time.Time{})
}

func (s *Store) replaceHandoff(ctx context.Context, handoff domain.Handoff, expectedUpdatedAt time.Time) (domain.Handoff, error) {
	if err := contextError(ctx); err != nil {
		return domain.Handoff{}, err
	}
	if err := domain.ValidateRequired("handoff.id", strings.TrimSpace(handoff.ID)); err != nil {
		return domain.Handoff{}, err
	}
	if err := handoff.Validate(); err != nil {
		return domain.Handoff{}, err
	}
	dir, err := s.recordDir(ctx, handoff.ProjectID, handoffsDir)
	if err != nil {
		return domain.Handoff{}, err
	}
	path, err := findRecordPath(dir, ports.RecordHandoff, handoff.ID)
	if err != nil {
		return domain.Handoff{}, err
	}
	now := s.nowUTC()
	err = withFileLock(ctx, path, func() error {
		current, err := readHandoffFile(path)
		if err != nil {
			return fmt.Errorf("fsstore: read handoff %q: %w", handoff.ID, err)
		}
		// A handoff written before updated_at existed has no revision of its
		// own; its summary falls back to created_at, so the CAS uses it too.
		currentUpdatedAt := current.UpdatedAt
		if currentUpdatedAt.IsZero() {
			currentUpdatedAt = current.CreatedAt
		}
		if err := checkRecordRevision(ports.RecordHandoff, handoff.ID, currentUpdatedAt, expectedUpdatedAt); err != nil {
			return err
		}
		updatedAt := nextRecordUpdatedAt(currentUpdatedAt, now)
		contentAt := current.ContentUpdatedAt
		if handoffTextChanged(current, handoff) {
			contentAt = updatedAt
		}
		handoff.ProjectID = current.ProjectID
		handoff.CreatedAt = current.CreatedAt
		handoff.UpdatedAt = updatedAt
		handoff.ContentUpdatedAt = contentAt
		frontmatter := handoffFrontmatter{
			Frontmatter: markdown.Frontmatter{
				ProjectID:        current.ProjectID,
				Kind:             "handoff",
				UpdatedAt:        updatedAt,
				ContentUpdatedAt: contentTimePtr(contentAt),
				SchemaVersion:    SchemaVersion,
				Permalink:        handoff.Permalink,
			},
			provenanceFrontmatter: provenanceToFrontmatter(current.Provenance),
			validityFrontmatter:   validityToFrontmatter(handoff.Validity, current.CreatedAt),
			relationsFrontmatter:  relationsToFrontmatter(relationsOrStored(handoff.Relations, current.Relations)),
			mergeFrontmatter:      mergeToFrontmatter(current.MergeInfo),
			reviewFrontmatter:     reviewToFrontmatter(handoff.ReviewSchedule),
			taxonomyFrontmatter:   taxonomyToFrontmatter(handoff.Taxonomy),
			ID:                    current.ID,
			Source:                sourceRefToFrontmatter(current.Source),
			CreatedAt:             current.CreatedAt,
		}
		raw, err := markdown.Render(frontmatter, renderHandoffBody(handoff))
		if err != nil {
			return fmt.Errorf("fsstore: render handoff %q: %w", handoff.ID, err)
		}
		if err := writeFileAtomic(path, raw); err != nil {
			return fmt.Errorf("fsstore: write handoff %q: %w", handoff.ID, err)
		}
		return nil
	})
	if err != nil {
		return domain.Handoff{}, err
	}
	handoff.TopicHistory = domain.TopicHistory{History: domain.BoundedHistory(handoff.History)}
	return handoff, nil
}

// checkRecordRevision enforces a record edit compare-and-swap. A zero expected
// revision is last-write-wins; otherwise the stored updated_at must still match
// the value the caller read. The caller runs this while holding the record
// lock, so the check and the write are one step.
func checkRecordRevision(kind, id string, current, expected time.Time) error {
	if expected.IsZero() {
		return nil
	}
	if expected.UTC().Equal(current.UTC()) {
		return nil
	}
	return domain.NewRevisionConflictError(kind, id, current)
}

// nextRecordUpdatedAt advances the revision of an edit. Store timestamps keep
// whole seconds, so two edits in one second would otherwise write the same
// updated_at and a compare-and-swap could not tell them apart. The edit
// revision advances by one nanosecond instead.
func nextRecordUpdatedAt(current, now time.Time) time.Time {
	current = current.UTC()
	now = now.UTC()
	if now.After(current) {
		return now
	}
	return current.Add(time.Nanosecond)
}

// noteTextChanged reports whether a replace changed the title, body or tags of
// a note. History follows a text edit, so it is not compared; provenance,
// validity, review and topic bookkeeping are metadata and must not move the
// content time.
func noteTextChanged(current, next domain.Note) bool {
	return current.Title != next.Title ||
		current.Body != next.Body ||
		!sameStrings(current.Tags, next.Tags)
}

func decisionTextChanged(current, next domain.Decision) bool {
	return current.Title != next.Title ||
		current.Context != next.Context ||
		!sameStrings(current.Alternatives, next.Alternatives) ||
		!sameStrings(current.Consequences, next.Consequences) ||
		!sameStrings(current.Evidence, next.Evidence) ||
		!sameStrings(current.Paths, next.Paths)
}

func attemptTextChanged(current, next domain.Attempt) bool {
	return current.Title != next.Title ||
		current.Hypothesis != next.Hypothesis ||
		current.Result != next.Result ||
		current.Reason != next.Reason
}

func handoffTextChanged(current, next domain.Handoff) bool {
	return current.Summary != next.Summary ||
		current.NextAction != next.NextAction ||
		current.GitContext != next.GitContext
}

// sameStrings compares two string slices; nil and empty are the same content.
func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
