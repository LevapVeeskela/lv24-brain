package fsstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// dumpRoots are the data-directory folders legacy connectors write dumps to.
// Raw-zone items (connectors/<connectionID>/items, see items.go) are not
// dumps: they are listed through ItemStore and indexed as kind "item".
var dumpRoots = []string{"mail", "github", "gitlab"}

// maxDumpBodyBytes bounds the indexed text of one dump file.
const maxDumpBodyBytes = 64 << 10

// dumpFrontmatter is lenient: legacy mail dumps carry kind "mail",
// project_id null and a date instead of updated_at.
type dumpFrontmatter struct {
	ProjectID string `yaml:"project_id"`
	UpdatedAt string `yaml:"updated_at"`
	Date      string `yaml:"date"`
}

var _ ports.DumpLister = (*Store)(nil)

// ListDumps returns every Markdown file under mail/, github/ and gitlab/.
// Connector output is external data: a file that does not parse is indexed
// as plain text rather than skipped, and an unreadable one is skipped.
func (s *Store) ListDumps(ctx context.Context) ([]ports.Dump, error) {
	var dumps []ports.Dump
	for _, root := range dumpRoots {
		base := filepath.Join(s.root, root)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
				return nil
			}
			dump, ok := s.readDump(path)
			if ok {
				dumps = append(dumps, dump)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return dumps, nil
}

func (s *Store) readDump(path string) (ports.Dump, bool) {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return ports.Dump{}, false
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return ports.Dump{}, false
	}
	var front dumpFrontmatter
	body, err := markdown.Parse(raw, &front)
	if err != nil {
		body, front = string(raw), dumpFrontmatter{}
	}
	if len(body) > maxDumpBodyBytes {
		body = strings.ToValidUTF8(body[:maxDumpBodyBytes], "")
	}
	dump := ports.Dump{
		Path:      filepath.ToSlash(rel),
		ProjectID: dumpProjectID(front.ProjectID, filepath.ToSlash(rel)),
		Title:     dumpTitle(body, rel),
		Body:      body,
		UpdatedAt: dumpTime(front, path),
	}
	return dump, true
}

// dumpProjectID is the frontmatter project_id, else one derived from the path
// the connectors use: mail/<account>/... -> mail:<account>,
// github/<owner>/<repo>.md -> github:<owner>/<repo>.
func dumpProjectID(fromFrontmatter, rel string) string {
	if id := strings.TrimSpace(fromFrontmatter); id != "" {
		return id
	}
	parts := strings.Split(strings.TrimSuffix(rel, filepath.Ext(rel)), "/")
	if len(parts) < 2 {
		return parts[0]
	}
	if parts[0] == "mail" {
		return "mail:" + parts[1]
	}
	return parts[0] + ":" + strings.Join(parts[1:], "/")
}

func dumpTitle(body, rel string) string {
	for _, line := range strings.Split(body, "\n") {
		if title, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok {
			return strings.TrimSpace(title)
		}
	}
	return filepath.ToSlash(rel)
}

func dumpTime(front dumpFrontmatter, path string) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(front.UpdatedAt)); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse("2006-01-02", strings.TrimSpace(front.Date)); err == nil {
		return t.UTC()
	}
	if info, err := os.Stat(path); err == nil {
		return info.ModTime().UTC()
	}
	return time.Time{}
}
