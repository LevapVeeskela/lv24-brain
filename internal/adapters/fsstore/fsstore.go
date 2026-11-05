// Package fsstore implements ports.Store over a Markdown data directory.
//
// Layout under the data directory:
//
//	registry.json
//	projects/<id>/state.md
//	projects/<id>/notes/YYYY-MM-DD.md
//	projects/<id>/decisions/YYYY-MM-DD-<slug>.md
//	projects/<id>/attempts/YYYY-MM-DD-<slug>.md
//	projects/<id>/handoffs/YYYY-MM-DD-HHMMSS.md
//
// registry.json keeps the project index (the legacy {"version":1,"projects":[]}
// shape). Every user-facing Markdown file starts with YAML frontmatter carrying
// project_id, kind, updated_at and schema_version. Writes go through a temporary
// file and rename, and record file names use deterministic UTC dates plus
// slugs. A project must exist in the registry before state or knowledge records
// may be written, mirroring the server's foreign keys.
//
// CurrentRevision is a deterministic SHA-256 over relative file paths and
// contents. It excludes the root-level secrets, index and backups directories
// and any .git directory, so unrelated indexes and credentials never affect the
// brain revision. The returned At timestamp is the newest included file
// modification time and is informational only.
//
// The store assumes a single writer, like a local checkout. Concurrent appends
// to one daily notes file are not serialized beyond atomic replacement.
package fsstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/markdown"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// SchemaVersion is the newest data schema this adapter writes.
	SchemaVersion = 1

	projectsDir  = "projects"
	stateFile    = "state.md"
	notesDir     = "notes"
	decisionsDir = "decisions"
	attemptsDir  = "attempts"
	handoffsDir  = "handoffs"

	tempFilePrefix   = ".tmp-"
	maxNameAttempts  = 1000
	maxProjectIDSize = 100
)

// Store persists brain records as Markdown files under one data directory.
type Store struct {
	root string
	now  func() time.Time
}

var _ ports.Store = (*Store)(nil)

// New opens a filesystem store rooted at dataDir. The directory and its
// projects subdirectory are created when missing.
func New(dataDir string) (*Store, error) {
	trimmed := strings.TrimSpace(dataDir)
	if trimmed == "" {
		return nil, domain.NewValidationError("fsstore.data_dir", "is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return nil, fmt.Errorf("fsstore: resolve data dir %q: %w", trimmed, err)
	}
	if err := os.MkdirAll(filepath.Join(abs, projectsDir), 0o755); err != nil {
		return nil, fmt.Errorf("fsstore: create data dir: %w", err)
	}
	return &Store{root: abs, now: time.Now}, nil
}

// Root returns the absolute data directory.
func (s *Store) Root() string {
	return s.root
}

func (s *Store) nowUTC() time.Time {
	clock := s.now
	if clock == nil {
		clock = time.Now
	}
	return clock().UTC().Truncate(time.Second)
}

func (s *Store) revisionAfterWrite(ctx context.Context) (domain.SyncRevision, error) {
	revision, err := s.CurrentRevision(ctx)
	if err != nil {
		return domain.SyncRevision{}, fmt.Errorf("fsstore: revision after write: %w", err)
	}
	return revision, nil
}

// CurrentRevision hashes the relative path and bytes of every regular data file.
// The value is "sha256:<hex>"; it is stable across runs and operating systems.
// An empty data directory hashes the empty stream and reports a zero At time.
func (s *Store) CurrentRevision(ctx context.Context) (domain.SyncRevision, error) {
	if err := contextError(ctx); err != nil {
		return domain.SyncRevision{}, err
	}
	type entry struct {
		path    string
		content []byte
		modTime time.Time
	}
	files := make([]entry, 0, 64)
	walkErr := filepath.WalkDir(s.root, func(path string, dirEntry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		if dirEntry.IsDir() {
			if rel == "." {
				return nil
			}
			if dirEntry.Name() == ".git" || isExcludedRootDir(rel, dirEntry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if dirEntry.Type()&os.ModeSymlink != 0 || !dirEntry.Type().IsRegular() {
			return nil
		}
		if strings.HasPrefix(dirEntry.Name(), tempFilePrefix) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		info, err := dirEntry.Info()
		if err != nil {
			return err
		}
		files = append(files, entry{
			path:    filepath.ToSlash(rel),
			content: content,
			modTime: info.ModTime().UTC(),
		})
		return nil
	})
	if walkErr != nil {
		return domain.SyncRevision{}, fmt.Errorf("fsstore: scan data dir: %w", walkErr)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	hash := sha256.New()
	var latest time.Time
	for _, file := range files {
		fmt.Fprintf(hash, "%s\x00%d\x00", file.path, len(file.content))
		hash.Write(file.content)
		if file.modTime.After(latest) {
			latest = file.modTime
		}
	}
	return domain.SyncRevision{
		Kind:  domain.SyncKindGit,
		Value: "sha256:" + hex.EncodeToString(hash.Sum(nil)),
		At:    latest,
	}, nil
}

func isExcludedRootDir(rel, name string) bool {
	if strings.ContainsRune(rel, os.PathSeparator) || strings.Contains(rel, "/") {
		return false
	}
	switch strings.ToLower(name) {
	case "secrets", "index", "backups":
		return true
	default:
		return false
	}
}

func (s *Store) projectDir(projectID string) (string, error) {
	if err := validateProjectID(projectID); err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, projectsDir, projectID)
	return s.ensureInsideRoot(dir)
}

func (s *Store) ensureInsideRoot(path string) (string, error) {
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(s.root, clean)
	if err != nil {
		return "", domain.NewValidationError("path", "must stay inside the data directory")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", domain.NewValidationError("path", "must stay inside the data directory")
	}
	return clean, nil
}

func validateProjectID(projectID string) error {
	if strings.TrimSpace(projectID) == "" {
		return domain.NewValidationError("project.id", "is required")
	}
	if projectID != strings.TrimSpace(projectID) {
		return domain.NewValidationError("project.id", "must not contain surrounding whitespace")
	}
	if len(projectID) > maxProjectIDSize {
		return domain.NewValidationError("project.id", fmt.Sprintf("must be at most %d bytes", maxProjectIDSize))
	}
	for _, char := range projectID {
		allowed := char >= 'a' && char <= 'z' ||
			char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' ||
			char == '.' || char == '_' || char == '-'
		if !allowed {
			return domain.NewValidationError("project.id", "may contain only letters, digits, dot, underscore and dash")
		}
	}
	first := projectID[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9') {
		return domain.NewValidationError("project.id", "must start with a letter or digit")
	}
	if strings.Contains(projectID, "..") {
		return domain.NewValidationError("project.id", "must not contain ..")
	}
	if strings.HasSuffix(projectID, ".") {
		return domain.NewValidationError("project.id", "must not end with a dot")
	}
	if isWindowsReservedName(projectID) {
		return domain.NewValidationError("project.id", "is a reserved file name")
	}
	return nil
}

func isWindowsReservedName(name string) bool {
	base := strings.ToUpper(name)
	if idx := strings.IndexByte(base, '.'); idx >= 0 {
		base = base[:idx]
	}
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 {
		prefix := base[:3]
		digit := base[3]
		if (prefix == "COM" || prefix == "LPT") && digit >= '1' && digit <= '9' {
			return true
		}
	}
	return false
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("fsstore: create directory %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, tempFilePrefix+"*")
	if err != nil {
		return fmt.Errorf("fsstore: create temp file in %s: %w", dir, err)
	}
	tempName := temp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempName)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("fsstore: write temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("fsstore: sync temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("fsstore: close temp file: %w", err)
	}
	if err := os.Chmod(tempName, 0o644); err != nil {
		return fmt.Errorf("fsstore: set file mode: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("fsstore: replace %s: %w", path, err)
	}
	cleanup = false
	return nil
}

// reserveRecordFile atomically claims an unused "<base>.md" name in dir and
// returns the reserved path and the final name stem. The caller replaces the
// empty reservation with writeFileAtomic; callers must remove the reservation on
// failure.
func reserveRecordFile(dir, base string) (string, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("fsstore: create directory %s: %w", dir, err)
	}
	for attempt := 1; attempt <= maxNameAttempts; attempt++ {
		name := base
		if attempt > 1 {
			name = fmt.Sprintf("%s-%d", base, attempt)
		}
		path := filepath.Join(dir, name+".md")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", "", fmt.Errorf("fsstore: reserve %s: %w", path, err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return "", "", fmt.Errorf("fsstore: close reservation %s: %w", path, err)
		}
		return path, name, nil
	}
	return "", "", fmt.Errorf("fsstore: no free record name for %q", base)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func notFound(kind, id string) error {
	return fmt.Errorf("fsstore: %s %q: %w", kind, id, domain.ErrNotFound)
}

func validateRecordFrontmatter(path string, fm markdown.Frontmatter, wantKind string) error {
	if fm.Kind != "" && fm.Kind != wantKind {
		return domain.NewValidationError("kind", fmt.Sprintf("%s: expected %q, found %q", path, wantKind, fm.Kind))
	}
	if fm.SchemaVersion > SchemaVersion {
		return domain.NewValidationError(
			"schema_version",
			fmt.Sprintf("%s: data schema %d requires a newer engine (this engine supports %d)", path, fm.SchemaVersion, SchemaVersion),
		)
	}
	return nil
}

func checkProjectMatch(path, frontmatterProjectID, expectedProjectID string) error {
	if frontmatterProjectID == "" || expectedProjectID == "" {
		return nil
	}
	if frontmatterProjectID != expectedProjectID {
		return domain.NewValidationError(
			"project_id",
			fmt.Sprintf("%s belongs to project %q, not %q", path, frontmatterProjectID, expectedProjectID),
		)
	}
	return nil
}
