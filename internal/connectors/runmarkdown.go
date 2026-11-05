package connectors

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
)

const runMarkdownSchemaVersion = 1

type runMarkdownFrontmatter struct {
	markdown.Frontmatter `yaml:",inline"`
	ID                   string                 `yaml:"id"`
	Source               string                 `yaml:"source"`
	Status               domain.ConnectorStatus `yaml:"status"`
	StartedAt            time.Time              `yaml:"started_at"`
	FinishedAt           *time.Time             `yaml:"finished_at,omitempty"`
	Counts               map[string]int         `yaml:"counts,omitempty"`
	Error                string                 `yaml:"error,omitempty"`
}

// RenderRunMarkdown renders one connector run as a self-contained Markdown
// document with the same frontmatter and body shape as local
// connectors/runs/*.md records. Server mode uses it for the export endpoint;
// no connector dump content is included.
func RenderRunMarkdown(run domain.ConnectorRun) ([]byte, error) {
	started := run.StartedAt.UTC()
	var finished *time.Time
	updated := started
	if run.FinishedAt != nil {
		normalized := run.FinishedAt.UTC()
		finished = &normalized
		updated = normalized
	}
	frontmatter := runMarkdownFrontmatter{
		Frontmatter: markdown.Frontmatter{
			Kind:          "connector-run",
			UpdatedAt:     updated,
			SchemaVersion: runMarkdownSchemaVersion,
		},
		ID:         run.ID,
		Source:     run.Source,
		Status:     run.Status,
		StartedAt:  started,
		FinishedAt: finished,
		Counts:     run.Counts,
		Error:      run.Error,
	}
	raw, err := markdown.Render(frontmatter, renderRunMarkdownBody(frontmatter))
	if err != nil {
		return nil, fmt.Errorf("connectors: render run markdown: %w", err)
	}
	return raw, nil
}

func renderRunMarkdownBody(frontmatter runMarkdownFrontmatter) string {
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
