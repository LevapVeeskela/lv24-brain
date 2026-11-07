package fsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

const (
	noteMarkerPrefix = "<!-- brain:note "
	noteMarkerSuffix = " -->"
)

type noteFileFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	Date                 string `yaml:"date,omitempty"`
}

type noteMetadata struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Tags      []string  `json:"tags,omitempty"`
	Permalink string    `json:"permalink,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type decisionFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	ID                   string                `yaml:"id,omitempty"`
	Title                string                `yaml:"title,omitempty"`
	Status               domain.DecisionStatus `yaml:"status,omitempty"`
	CreatedAt            time.Time             `yaml:"created_at,omitempty"`
}

type attemptFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	ID                   string    `yaml:"id,omitempty"`
	Title                string    `yaml:"title,omitempty"`
	CreatedAt            time.Time `yaml:"created_at,omitempty"`
}

type handoffFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	ID                   string    `yaml:"id,omitempty"`
	CreatedAt            time.Time `yaml:"created_at,omitempty"`
}

// AppendNote appends one note section to projects/<id>/notes/YYYY-MM-DD.md.
// Notes with the same UTC date share one file; IDs default to
// note-<date>-<slug> and gain a numeric suffix when already present.
func (s *Store) AppendNote(ctx context.Context, note domain.Note) (domain.SyncRevision, error) {
	if err := contextError(ctx); err != nil {
		return domain.SyncRevision{}, err
	}
	if err := note.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	if err := s.requireProject(note.ProjectID); err != nil {
		return domain.SyncRevision{}, err
	}
	dir, err := s.projectDir(note.ProjectID)
	if err != nil {
		return domain.SyncRevision{}, err
	}

	createdAt := note.CreatedAt
	if createdAt.IsZero() {
		createdAt = s.nowUTC()
	} else {
		createdAt = createdAt.UTC()
	}
	date := createdAt.Format("2006-01-02")
	updatedAt := s.nowUTC()
	path := filepath.Join(dir, notesDir, date+".md")

	// The daily file is read, extended and rewritten as one step. The lock keeps
	// a concurrent AppendNote from reading the same old content and dropping
	// this note.
	err = withFileLock(ctx, path, func() error {
		return appendNoteLocked(path, note, date, createdAt, updatedAt)
	})
	if err != nil {
		return domain.SyncRevision{}, err
	}
	return s.revisionAfterWrite(ctx)
}

const (
	// quarantineInfix marks a notes file moved aside because it no longer
	// parses: <date>.md.corrupt-<unix-nanos>.
	quarantineInfix  = ".md.corrupt-"
	quarantineReason = "quarantined: the daily notes file did not parse; repair it and merge it back into <date>.md"
)

// notesFileParses reports whether a daily notes file can be read back.
func notesFileParses(raw []byte) bool {
	var frontmatter noteFileFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		return false
	}
	_, err = parseNotesBody(body, time.Time{})
	return err == nil
}

func quarantine(path string, now time.Time) error {
	aside := strings.TrimSuffix(path, ".md") + quarantineInfix + fmt.Sprint(now.UnixNano())
	if err := os.Rename(path, aside); err != nil {
		return fmt.Errorf("fsstore: quarantine unparseable notes %s: %w", path, err)
	}
	return nil
}

// appendNoteLocked performs the read-modify-write of one daily notes file. The
// caller must hold the lock for path.
func appendNoteLocked(path string, note domain.Note, date string, createdAt, updatedAt time.Time) error {
	var frontmatter noteFileFrontmatter
	body := ""
	raw, err := os.ReadFile(path)
	if err == nil && !notesFileParses(raw) {
		// A damaged daily file must not block today's notes: keep it aside
		// for repair (listing reports it) and start a fresh file.
		if err := quarantine(path, updatedAt); err != nil {
			return err
		}
		raw, err = nil, os.ErrNotExist
	}
	switch {
	case err == nil:
		body, err = markdown.Parse(raw, &frontmatter)
		if err != nil {
			return fmt.Errorf("fsstore: parse notes %s: %w", path, err)
		}
		if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, "note"); err != nil {
			return err
		}
		if err := checkProjectMatch(path, frontmatter.ProjectID, note.ProjectID); err != nil {
			return err
		}
	case os.IsNotExist(err):
		body = fmt.Sprintf("# Notes — %s\n\n", date)
		frontmatter = noteFileFrontmatter{
			Frontmatter: markdown.Frontmatter{
				ProjectID:     note.ProjectID,
				Kind:          "note",
				UpdatedAt:     updatedAt,
				SchemaVersion: SchemaVersion,
			},
			Date: date,
		}
	default:
		return fmt.Errorf("fsstore: read notes %s: %w", path, err)
	}

	existing, err := parseNotesBody(body, createdAt)
	if err != nil {
		return fmt.Errorf("fsstore: parse notes %s: %w", path, err)
	}
	usedIDs := make(map[string]struct{}, len(existing))
	for _, existingNote := range existing {
		usedIDs[existingNote.ID] = struct{}{}
	}
	id := strings.TrimSpace(note.ID)
	if id == "" {
		id = nextNoteID(usedIDs, date, note.Title)
	} else if _, conflict := usedIDs[id]; conflict {
		return fmt.Errorf("fsstore: note %q: %w", id, domain.ErrConflict)
	}

	entry := renderNoteEntry(noteMetadata{
		ID:        id,
		Title:     note.Title,
		Tags:      nonNilStrings(note.Tags),
		Permalink: note.Permalink,
		CreatedAt: createdAt,
	}, note.Body)
	if body != "" && !strings.HasSuffix(body, "\n\n") {
		body = strings.TrimRight(body, "\n") + "\n\n"
	}
	body += entry

	frontmatter.ProjectID = note.ProjectID
	frontmatter.Kind = "note"
	frontmatter.UpdatedAt = updatedAt
	frontmatter.SchemaVersion = SchemaVersion
	frontmatter.Date = date
	rendered, err := markdown.Render(frontmatter, body)
	if err != nil {
		return fmt.Errorf("fsstore: render notes %s: %w", path, err)
	}
	if err := writeFileAtomic(path, rendered); err != nil {
		return fmt.Errorf("fsstore: write notes %s: %w", path, err)
	}
	return nil
}

// AppendDecision writes projects/<id>/decisions/YYYY-MM-DD-<slug>.md.
func (s *Store) AppendDecision(ctx context.Context, decision domain.Decision) (domain.SyncRevision, error) {
	if err := contextError(ctx); err != nil {
		return domain.SyncRevision{}, err
	}
	if decision.Status == "" {
		decision.Status = domain.DecisionProposed
	}
	if err := decision.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	if err := s.requireProject(decision.ProjectID); err != nil {
		return domain.SyncRevision{}, err
	}
	dir, err := s.projectDir(decision.ProjectID)
	if err != nil {
		return domain.SyncRevision{}, err
	}
	createdAt := recordCreatedAt(decision.CreatedAt, s.nowUTC())
	updatedAt := s.nowUTC()

	_, err = publishRecordFile(
		filepath.Join(dir, decisionsDir),
		createdAt.Format("2006-01-02")+"-"+slugify(decision.Title),
		func(stem string) ([]byte, error) {
			id := strings.TrimSpace(decision.ID)
			if id == "" {
				id = "decision-" + stem
			}
			frontmatter := decisionFrontmatter{
				Frontmatter: markdown.Frontmatter{
					ProjectID:     decision.ProjectID,
					Kind:          "decision",
					UpdatedAt:     updatedAt,
					SchemaVersion: SchemaVersion,
					Permalink:     decision.Permalink,
				},
				ID:        id,
				Title:     decision.Title,
				Status:    decision.Status,
				CreatedAt: createdAt,
			}
			raw, err := markdown.Render(frontmatter, renderDecisionBody(decision))
			if err != nil {
				return nil, fmt.Errorf("fsstore: render decision %q: %w", id, err)
			}
			return raw, nil
		},
	)
	if err != nil {
		return domain.SyncRevision{}, fmt.Errorf("fsstore: write decision %q: %w", decision.Title, err)
	}
	return s.revisionAfterWrite(ctx)
}

// AppendAttempt writes projects/<id>/attempts/YYYY-MM-DD-<slug>.md.
func (s *Store) AppendAttempt(ctx context.Context, attempt domain.Attempt) (domain.SyncRevision, error) {
	if err := contextError(ctx); err != nil {
		return domain.SyncRevision{}, err
	}
	if err := attempt.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	if err := s.requireProject(attempt.ProjectID); err != nil {
		return domain.SyncRevision{}, err
	}
	dir, err := s.projectDir(attempt.ProjectID)
	if err != nil {
		return domain.SyncRevision{}, err
	}
	createdAt := recordCreatedAt(attempt.CreatedAt, s.nowUTC())
	updatedAt := s.nowUTC()

	_, err = publishRecordFile(
		filepath.Join(dir, attemptsDir),
		createdAt.Format("2006-01-02")+"-"+slugify(attempt.Title),
		func(stem string) ([]byte, error) {
			id := strings.TrimSpace(attempt.ID)
			if id == "" {
				id = "attempt-" + stem
			}
			frontmatter := attemptFrontmatter{
				Frontmatter: markdown.Frontmatter{
					ProjectID:     attempt.ProjectID,
					Kind:          "attempt",
					UpdatedAt:     updatedAt,
					SchemaVersion: SchemaVersion,
					Permalink:     attempt.Permalink,
				},
				ID:        id,
				Title:     attempt.Title,
				CreatedAt: createdAt,
			}
			raw, err := markdown.Render(frontmatter, renderAttemptBody(attempt))
			if err != nil {
				return nil, fmt.Errorf("fsstore: render attempt %q: %w", id, err)
			}
			return raw, nil
		},
	)
	if err != nil {
		return domain.SyncRevision{}, fmt.Errorf("fsstore: write attempt %q: %w", attempt.Title, err)
	}
	return s.revisionAfterWrite(ctx)
}

// AppendHandoff writes projects/<id>/handoffs/YYYY-MM-DD-HHMMSS.md.
func (s *Store) AppendHandoff(ctx context.Context, handoff domain.Handoff) (domain.SyncRevision, error) {
	if err := contextError(ctx); err != nil {
		return domain.SyncRevision{}, err
	}
	if err := handoff.Validate(); err != nil {
		return domain.SyncRevision{}, err
	}
	if err := s.requireProject(handoff.ProjectID); err != nil {
		return domain.SyncRevision{}, err
	}
	dir, err := s.projectDir(handoff.ProjectID)
	if err != nil {
		return domain.SyncRevision{}, err
	}
	createdAt := recordCreatedAt(handoff.CreatedAt, s.nowUTC())

	_, err = publishRecordFile(
		filepath.Join(dir, handoffsDir),
		createdAt.Format("2006-01-02-150405"),
		func(stem string) ([]byte, error) {
			id := strings.TrimSpace(handoff.ID)
			if id == "" {
				id = "handoff-" + stem
			}
			frontmatter := handoffFrontmatter{
				Frontmatter: markdown.Frontmatter{
					ProjectID:     handoff.ProjectID,
					Kind:          "handoff",
					UpdatedAt:     createdAt,
					SchemaVersion: SchemaVersion,
					Permalink:     handoff.Permalink,
				},
				ID:        id,
				CreatedAt: createdAt,
			}
			raw, err := markdown.Render(frontmatter, renderHandoffBody(handoff))
			if err != nil {
				return nil, fmt.Errorf("fsstore: render handoff %q: %w", id, err)
			}
			return raw, nil
		},
	)
	if err != nil {
		return domain.SyncRevision{}, fmt.Errorf("fsstore: write handoff: %w", err)
	}
	return s.revisionAfterWrite(ctx)
}

func recordCreatedAt(value time.Time, fallback time.Time) time.Time {
	if value.IsZero() {
		return fallback
	}
	return value.UTC()
}

func nextNoteID(used map[string]struct{}, date, title string) string {
	base := "note-" + date + "-" + slugify(title)
	if _, exists := used[base]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

func renderNoteEntry(metadata noteMetadata, body string) string {
	rawMetadata, err := json.Marshal(metadata)
	if err != nil {
		rawMetadata = []byte(`{"id":"note"}`)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s%s%s\n", noteMarkerPrefix, rawMetadata, noteMarkerSuffix)
	title := singleLine(metadata.Title)
	if title == "" {
		title = "Note"
	}
	fmt.Fprintf(&out, "## %s\n\n", title)
	out.WriteString(strings.TrimRight(body, "\n"))
	out.WriteString("\n\n")
	return out.String()
}

func renderDecisionBody(decision domain.Decision) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", singleLine(decision.Title))
	fmt.Fprintf(&out, "Status: %s\n\n", decision.Status)
	out.WriteString("## Context\n\n")
	out.WriteString(strings.TrimSpace(decision.Context))
	out.WriteString("\n\n## Alternatives\n\n")
	writeRecordList(&out, decision.Alternatives)
	out.WriteString("\n## Consequences\n\n")
	writeRecordList(&out, decision.Consequences)
	out.WriteString("\n## Evidence\n\n")
	writeRecordList(&out, decision.Evidence)
	return out.String()
}

func renderAttemptBody(attempt domain.Attempt) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", singleLine(attempt.Title))
	out.WriteString("## Hypothesis\n\n")
	out.WriteString(strings.TrimSpace(attempt.Hypothesis))
	out.WriteString("\n\n## Result\n\n")
	out.WriteString(strings.TrimSpace(attempt.Result))
	out.WriteString("\n\n## Why it failed\n\n")
	out.WriteString(strings.TrimSpace(attempt.Reason))
	return out.String()
}

func renderHandoffBody(handoff domain.Handoff) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Handoff — %s\n\n", handoff.CreatedAt.UTC().Format(time.RFC3339))
	out.WriteString("## Summary\n\n")
	out.WriteString(strings.TrimSpace(handoff.Summary))
	out.WriteString("\n\n## Git context\n\n")
	out.WriteString(strings.TrimSpace(handoff.GitContext))
	out.WriteString("\n\n## Next action\n\n")
	out.WriteString(strings.TrimSpace(handoff.NextAction))
	return out.String()
}

func writeRecordList(out *strings.Builder, items []string) {
	if len(items) == 0 {
		out.WriteString("_None recorded._\n")
		return
	}
	for _, item := range items {
		writeListItem(out, item)
	}
}
