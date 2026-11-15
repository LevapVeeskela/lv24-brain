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
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var _ ports.PromptStore = (*Store)(nil)

// promptsDirName is the per-project prompt zone. It is machine-local: a
// .gitignore inside it keeps every prompt out of the shared tree, and
// CurrentRevision skips the directory entirely.
const promptsDirName = "prompts"

// promptVisibility is the only visibility the store writes. A file with any
// other value is not treated as a prompt file.
const promptVisibility = "local"

// promptGitignore is the content of projects/<id>/prompts/.gitignore. Every
// name, including the ignore file itself, is untracked.
const promptGitignore = "*"

// promptFileFrontmatter is the on-disk header of one daily prompt file.
type promptFileFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	Visibility           string `yaml:"visibility"`
}

// AppendPrompt adds one prompt to projects/<id>/prompts/<date>.md. The daily
// file is read, extended and rewritten under its lock, so concurrent saves
// serialize instead of dropping a prompt.
func (s *Store) AppendPrompt(ctx context.Context, prompt domain.Prompt) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := s.requireProject(prompt.ProjectID); err != nil {
		return err
	}
	text := strings.TrimSpace(prompt.Text)
	if err := domain.ValidateRequired("prompt.text", text); err != nil {
		return err
	}
	at := prompt.At
	if at.IsZero() {
		at = s.nowUTC()
	} else {
		at = at.UTC()
	}
	dir, err := s.recordDir(ctx, prompt.ProjectID, promptsDirName)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, at.Format("2006-01-02")+".md")
	prompt.At = at
	prompt.Text = text
	prompt.Session = strings.TrimSpace(prompt.Session)
	return withFileLock(ctx, path, func() error {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("fsstore: create prompts %s: %w", dir, err)
		}
		if err := ensurePromptGitignore(dir); err != nil {
			return err
		}
		return s.appendPromptLocked(path, prompt)
	})
}

// ensurePromptGitignore writes the `*` ignore file when it is missing or was
// edited to publish prompts. Prompts are the most sensitive local data, so the
// file is repaired instead of trusted.
func ensurePromptGitignore(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	if raw, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(raw)) == promptGitignore {
		return nil
	}
	if err := os.WriteFile(path, []byte(promptGitignore+"\n"), 0o644); err != nil {
		return fmt.Errorf("fsstore: write prompt ignore file %s: %w", path, err)
	}
	return nil
}

// appendPromptLocked appends one entry to the daily prompt file. The caller
// holds the lock for path.
func (s *Store) appendPromptLocked(path string, prompt domain.Prompt) error {
	now := s.nowUTC()
	frontmatter := promptFileFrontmatter{
		Frontmatter: markdown.Frontmatter{
			ProjectID:     prompt.ProjectID,
			Kind:          "prompt",
			UpdatedAt:     now,
			SchemaVersion: SchemaVersion,
		},
		Visibility: promptVisibility,
	}
	body := fmt.Sprintf("# Prompts — %s\n", prompt.At.Format("2006-01-02"))
	if raw, err := os.ReadFile(path); err == nil {
		var stored promptFileFrontmatter
		parsed, parseErr := markdown.Parse(raw, &stored)
		if parseErr != nil {
			return fmt.Errorf("fsstore: parse prompts %s: %w", path, parseErr)
		}
		if err := validateRecordFrontmatter(path, stored.Frontmatter, "prompt"); err != nil {
			return err
		}
		if stored.Visibility != "" && stored.Visibility != promptVisibility {
			return domain.NewValidationError("visibility",
				fmt.Sprintf("%s: prompt file must be local-only", path))
		}
		body = parsed
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("fsstore: read prompts %s: %w", path, err)
	}
	body = strings.TrimRight(body, "\n")
	if body == "" {
		body = fmt.Sprintf("# Prompts — %s", prompt.At.Format("2006-01-02"))
	}
	body += "\n\n" + renderPromptEntry(prompt)
	rendered, err := markdown.Render(frontmatter, body)
	if err != nil {
		return fmt.Errorf("fsstore: render prompts %s: %w", path, err)
	}
	if err := writeFileAtomic(path, rendered); err != nil {
		return fmt.Errorf("fsstore: write prompts %s: %w", path, err)
	}
	return nil
}

// renderPromptEntry renders one prompt. The session, when present, is a
// labelled line so a reader can tell prompt text from provenance.
func renderPromptEntry(prompt domain.Prompt) string {
	var builder strings.Builder
	builder.WriteString("## ")
	builder.WriteString(prompt.At.UTC().Format(time.RFC3339))
	builder.WriteString("\n\n")
	if prompt.Session != "" {
		builder.WriteString("session: ")
		builder.WriteString(prompt.Session)
		builder.WriteString("\n\n")
	}
	builder.WriteString(strings.TrimSpace(prompt.Text))
	builder.WriteString("\n")
	return builder.String()
}

// ListPrompts returns a project's prompts newest first. An unreadable or
// non-local file is skipped: a prompt zone is best-effort local data, never a
// reason to fail a read of curated records.
func (s *Store) ListPrompts(ctx context.Context, projectID string, limit int) ([]domain.Prompt, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := s.requireProject(projectID); err != nil {
		return nil, err
	}
	dir, err := s.recordDir(ctx, projectID, promptsDirName)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []domain.Prompt{}, nil
		}
		return nil, fmt.Errorf("fsstore: read prompts %s: %w", dir, err)
	}
	prompts := make([]domain.Prompt, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		parsed, err := s.readPromptFile(filepath.Join(dir, entry.Name()), projectID)
		if err != nil {
			continue
		}
		prompts = append(prompts, parsed...)
	}
	sort.SliceStable(prompts, func(i, j int) bool {
		if !prompts[i].At.Equal(prompts[j].At) {
			return prompts[i].At.After(prompts[j].At)
		}
		return prompts[i].Text < prompts[j].Text
	})
	if limit <= 0 {
		limit = 50
	}
	if len(prompts) > limit {
		prompts = prompts[:limit]
	}
	return prompts, nil
}

// readPromptFile parses one daily prompt file into its entries.
func (s *Store) readPromptFile(path, projectID string) ([]domain.Prompt, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var frontmatter promptFileFrontmatter
	body, err := markdown.Parse(raw, &frontmatter)
	if err != nil {
		return nil, err
	}
	if err := validateRecordFrontmatter(path, frontmatter.Frontmatter, "prompt"); err != nil {
		return nil, err
	}
	if frontmatter.Visibility != "" && frontmatter.Visibility != promptVisibility {
		return nil, domain.NewValidationError("visibility",
			fmt.Sprintf("%s: prompt file is not local-only", path))
	}
	rel, relErr := filepath.Rel(s.root, path)
	if relErr != nil {
		rel = path
	}
	rel = filepath.ToSlash(rel)
	entries := make([]domain.Prompt, 0, 4)
	var (
		at      time.Time
		session string
		lines   []string
	)
	flush := func() {
		text := strings.TrimSpace(strings.Join(lines, "\n"))
		if !at.IsZero() && text != "" {
			entries = append(entries, domain.Prompt{
				ProjectID: projectID, At: at, Session: session, Text: text, Path: rel,
			})
		}
		lines = nil
	}
	for _, line := range strings.Split(body, "\n") {
		if parsed, ok := parsePromptHeading(line); ok {
			flush()
			at, session = parsed, ""
			continue
		}
		if at.IsZero() {
			continue
		}
		if session == "" {
			if value, ok := strings.CutPrefix(strings.TrimSpace(line), "session: "); ok {
				session = strings.TrimSpace(value)
				continue
			}
		}
		lines = append(lines, line)
	}
	flush()
	return entries, nil
}

// parsePromptHeading recognizes the "## <RFC3339>" heading of one prompt.
func parsePromptHeading(line string) (time.Time, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "## ") {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(strings.TrimPrefix(trimmed, "## ")))
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}
