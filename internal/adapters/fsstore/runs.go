package fsstore

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	connectorsDir       = "connectors"
	connectorRunsDir    = "runs"
	connectorRunKind    = "connector-run"
	defaultRunLimit     = 20
	maxConnectorRunList = 500
	// connectorRunMaxSuffix bounds the numeric suffix used when two raw ids
	// sanitize to the same file name (-2 through -99).
	connectorRunMaxSuffix = 99
)

var runNameSanitizer = regexp.MustCompile(`[^a-z0-9-]+`)

var _ ports.ConnectorRunStore = (*Store)(nil)

type connectorRunFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	ID                   string                 `yaml:"id"`
	Source               string                 `yaml:"source"`
	Status               domain.ConnectorStatus `yaml:"status"`
	StartedAt            time.Time              `yaml:"started_at"`
	FinishedAt           *time.Time             `yaml:"finished_at,omitempty"`
	Counts               map[string]int         `yaml:"counts,omitempty"`
	Error                string                 `yaml:"error,omitempty"`
}

// AppendConnectorRun writes one run record under connectors/runs. A run id is
// unique per source: re-appending the same raw id and source rewrites the file
// that already stores it, regardless of the new StartedAt, so retries stay
// idempotent. The same id under a different source is a separate record. File
// names are reserved atomically; ids that sanitize to the same name are stored
// side by side with a numeric suffix.
func (s *Store) AppendConnectorRun(ctx context.Context, run domain.ConnectorRun) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := run.Validate(); err != nil {
		return err
	}
	started := run.StartedAt.UTC()
	if started.IsZero() {
		started = s.nowUTC()
	}
	id := strings.TrimSpace(run.ID)
	if id == "" {
		id = fmt.Sprintf("run-%d", started.UnixNano())
	}
	source := safeRunNamePart(run.Source)
	idPart := safeRunNamePart(id)
	if source == "" || idPart == "" {
		return domain.NewValidationError("connectorRun", "source and id need at least one letter or digit")
	}

	finished := run.FinishedAt
	if finished != nil {
		normalized := finished.UTC()
		finished = &normalized
	}
	updatedAt := started
	if finished != nil {
		updatedAt = *finished
	}
	frontmatter := connectorRunFrontmatter{
		Frontmatter: markdown.Frontmatter{
			Kind:          connectorRunKind,
			UpdatedAt:     updatedAt,
			SchemaVersion: SchemaVersion,
		},
		ID:         id,
		Source:     run.Source,
		Status:     run.Status,
		StartedAt:  started,
		FinishedAt: finished,
		Counts:     run.Counts,
		Error:      run.Error,
	}
	raw, err := markdown.Render(frontmatter, renderConnectorRunBody(frontmatter))
	if err != nil {
		return fmt.Errorf("fsstore: render connector run: %w", err)
	}
	dir, err := s.ensureInsideRoot(filepath.Join(s.root, connectorsDir, connectorRunsDir))
	if err != nil {
		return err
	}
	base := started.Format("20060102T150405Z") + "-" + source + "-" + idPart
	// The lock on the runs directory serializes the reuse lookup with the write,
	// so two appends of one id cannot both create a file.
	return withFileLock(ctx, dir, func() error {
		path, reserved, err := reserveConnectorRunPath(dir, base, id, run.Source)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(path, raw); err != nil {
			if reserved {
				_ = os.Remove(path)
			}
			return err
		}
		return nil
	})
}

// ListConnectorRuns returns stored runs newest first. An empty source matches
// every source; a limit of zero selects the adapter default. Files without
// frontmatter or with unparseable frontmatter are skipped, so a hand-written
// note cannot hide valid run records; read errors are still surfaced.
func (s *Store) ListConnectorRuns(ctx context.Context, source string, limit int) ([]domain.ConnectorRun, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultRunLimit
	}
	if limit > maxConnectorRunList {
		limit = maxConnectorRunList
	}
	dir := filepath.Join(s.root, connectorsDir, connectorRunsDir)
	names, err := connectorRunFiles(dir)
	if err != nil {
		return nil, err
	}
	wanted := strings.ToLower(strings.TrimSpace(source))
	runs := make([]domain.ConnectorRun, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("fsstore: read connector run %s: %w", path, err)
		}
		frontmatter, ok := parseConnectorRun(raw)
		if !ok {
			// Unparseable frontmatter may be a human note, not a run record.
			continue
		}
		if frontmatter.Kind != connectorRunKind {
			continue
		}
		if wanted != "" && strings.ToLower(frontmatter.Source) != wanted {
			continue
		}
		runs = append(runs, frontmatter.run())
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].ID < runs[j].ID
		}
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
	if len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

// reserveConnectorRunPath returns the file that stores one run and whether it
// was newly reserved. A run id is unique per source: an existing record with
// the same raw id and source is reused, so a retry overwrites it regardless of
// StartedAt. Otherwise a new name is claimed atomically with reserveRecordFile,
// first at base and then at the base-2 through base-99 suffixes when earlier
// names are taken.
func reserveConnectorRunPath(dir, base, id, source string) (string, bool, error) {
	names, err := connectorRunFiles(dir)
	if err != nil {
		return "", false, err
	}
	for _, name := range names {
		frontmatter, ok := readConnectorRun(filepath.Join(dir, name))
		if ok && frontmatter.Kind == connectorRunKind &&
			frontmatter.ID == id && frontmatter.Source == source {
			return filepath.Join(dir, name), false, nil
		}
	}
	path, stem, err := reserveRecordFile(dir, base)
	if err != nil {
		return "", false, fmt.Errorf("fsstore: reserve connector run %q: %w", base, err)
	}
	if !connectorRunNameWithinLimit(base, stem) {
		_ = os.Remove(path)
		return "", false, fmt.Errorf("fsstore: no free connector run name for %q", base)
	}
	return path, true, nil
}

// connectorRunNameWithinLimit reports whether stem is base or base-2..base-99.
func connectorRunNameWithinLimit(base, stem string) bool {
	if stem == base {
		return true
	}
	suffix, ok := strings.CutPrefix(stem, base+"-")
	if !ok {
		return false
	}
	n, err := strconv.Atoi(suffix)
	return err == nil && n >= 2 && n <= connectorRunMaxSuffix
}

// connectorRunFiles lists the Markdown files under dir in sorted order.
func connectorRunFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fsstore: read connector runs %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// parseConnectorRun parses a run record. It reports false for files without
// frontmatter or with invalid YAML instead of returning an error.
func parseConnectorRun(raw []byte) (connectorRunFrontmatter, bool) {
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("---")) {
		return connectorRunFrontmatter{}, false
	}
	var frontmatter connectorRunFrontmatter
	if _, err := markdown.Parse(raw, &frontmatter); err != nil {
		return connectorRunFrontmatter{}, false
	}
	return frontmatter, true
}

// readConnectorRun parses a run file, treating read failures as "not a run".
func readConnectorRun(path string) (connectorRunFrontmatter, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return connectorRunFrontmatter{}, false
	}
	return parseConnectorRun(raw)
}

func (f connectorRunFrontmatter) run() domain.ConnectorRun {
	return domain.ConnectorRun{
		ID:         f.ID,
		Source:     f.Source,
		StartedAt:  f.StartedAt.UTC(),
		FinishedAt: f.FinishedAt,
		Status:     f.Status,
		Counts:     f.Counts,
		Error:      f.Error,
	}
}

func renderConnectorRunBody(frontmatter connectorRunFrontmatter) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Connector run: %s\n\n", frontmatter.Source)
	fmt.Fprintf(&out, "- id: `%s`\n", frontmatter.ID)
	fmt.Fprintf(&out, "- status: `%s`\n", frontmatter.Status)
	fmt.Fprintf(&out, "- started: %s\n", frontmatter.StartedAt.UTC().Format(time.RFC3339))
	if frontmatter.FinishedAt != nil {
		fmt.Fprintf(&out, "- finished: %s\n", frontmatter.FinishedAt.UTC().Format(time.RFC3339))
	}
	if len(frontmatter.Counts) > 0 {
		out.WriteString("\n## Counts\n\n")
		keys := make([]string, 0, len(frontmatter.Counts))
		for key := range frontmatter.Counts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&out, "- %s: %d\n", key, frontmatter.Counts[key])
		}
	}
	if frontmatter.Error != "" {
		out.WriteString("\n## Error\n\n")
		out.WriteString(frontmatter.Error)
		out.WriteString("\n")
	}
	return out.String()
}

func safeRunNamePart(value string) string {
	lower := strings.ToLower(strings.TrimSpace(value))
	lower = runNameSanitizer.ReplaceAllString(lower, "-")
	return strings.Trim(lower, "-")
}
