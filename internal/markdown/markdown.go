// Package markdown implements the YAML frontmatter plus Markdown body format
// used by the LV24 Brain filesystem store.
//
// A document starts with a "---" line, a YAML mapping, a closing "---" line,
// an optional blank line, and the Markdown body. Parse accepts a UTF-8 BOM and
// CRLF line endings so files edited on Windows keep working. Render always
// emits LF line endings.
package markdown

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const delimiter = "---"

// Errors returned when a document does not start with a well-formed
// frontmatter block.
var (
	// ErrMissingFrontmatter means the document does not start with "---".
	ErrMissingFrontmatter = errors.New("markdown: missing YAML frontmatter")
	// ErrUnterminatedFrontmatter means the closing "---" line is absent.
	ErrUnterminatedFrontmatter = errors.New("markdown: unterminated YAML frontmatter")
)

// Frontmatter is the metadata every brain Markdown document carries.
type Frontmatter struct {
	ProjectID     string    `yaml:"project_id"`
	Kind          string    `yaml:"kind"`
	UpdatedAt     time.Time `yaml:"updated_at"`
	SchemaVersion int       `yaml:"schema_version"`
	Permalink     string    `yaml:"permalink,omitempty"`
}

// Parse splits a document into its YAML frontmatter and Markdown body. When
// out is non-nil the frontmatter is unmarshalled into it; unknown keys are
// ignored. The body is returned with CRLF normalized to LF and one leading
// blank line (the separator after the closing delimiter) removed.
func Parse(data []byte, out any) (string, error) {
	text := normalizeText(string(data))
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != delimiter {
		return "", ErrMissingFrontmatter
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == delimiter {
			end = i
			break
		}
	}
	if end < 0 {
		return "", ErrUnterminatedFrontmatter
	}
	if out != nil {
		header := strings.Join(lines[1:end], "\n")
		if err := yaml.Unmarshal([]byte(header), out); err != nil {
			return "", fmt.Errorf("markdown: decode frontmatter: %w", err)
		}
	}
	body := strings.Join(lines[end+1:], "\n")
	return strings.TrimPrefix(body, "\n"), nil
}

// Render encodes frontmatter as YAML, wraps it in delimiters and appends body.
// The result always ends with a newline. A non-empty body is separated from the
// closing delimiter by one blank line.
func Render(frontmatter any, body string) ([]byte, error) {
	raw, err := yaml.Marshal(frontmatter)
	if err != nil {
		return nil, fmt.Errorf("markdown: encode frontmatter: %w", err)
	}
	body = normalizeText(body)

	var out strings.Builder
	out.WriteString(delimiter)
	out.WriteByte('\n')
	out.Write(raw)
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		out.WriteByte('\n')
	}
	out.WriteString(delimiter)
	out.WriteByte('\n')
	if body != "" {
		out.WriteByte('\n')
		out.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			out.WriteByte('\n')
		}
	}
	return []byte(out.String()), nil
}

func normalizeText(text string) string {
	text = strings.TrimPrefix(text, "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}
