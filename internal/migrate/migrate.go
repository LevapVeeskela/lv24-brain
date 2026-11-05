// Package migrate imports knowledge from a legacy Company Brain ".brain" data
// directory into the LV24 Brain filesystem store layout.
//
// Only knowledge records are migrated: projects/<id>/state.md and the notes,
// decisions, attempts and handoffs directories. Mail, connectors, journal,
// registry.json and every other legacy directory are ignored on purpose.
//
// Every target file is normalized to data schema version 1. The importer is
// idempotent: an existing target file with identical bytes is reported as
// unchanged, while a differing target file is reported as a conflict and is
// never overwritten. Paths are validated so a corrupt or hostile source tree
// cannot write outside the target data directory.
package migrate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	// SchemaVersion is the data schema version written by the importer. It must
	// match fsstore.SchemaVersion.
	SchemaVersion = 1

	projectsDir  = "projects"
	stateFile    = "state.md"
	notesDir     = "notes"
	decisionsDir = "decisions"
	attemptsDir  = "attempts"
	handoffsDir  = "handoffs"

	tempFilePrefix = ".tmp-"
)

// Options selects the legacy source, the fsstore target and the dry-run mode.
type Options struct {
	// SourceDir is the legacy .brain data directory. It is opened read-only.
	SourceDir string
	// TargetDir is the LV24 Brain data directory to import into.
	TargetDir string
	// DryRun reports what would happen without creating or changing files.
	DryRun bool
}

// Status is the outcome for one source file.
type Status string

const (
	// StatusCreated means the target file was (or, in dry-run, would be) written.
	StatusCreated Status = "created"
	// StatusUnchanged means the target file already had identical content.
	StatusUnchanged Status = "unchanged"
	// StatusConflict means the target file exists with different content.
	StatusConflict Status = "conflict"
	// StatusFailed means the source file could not be read or converted.
	StatusFailed Status = "failed"
)

// FileResult describes one source file and what happened to it.
type FileResult struct {
	ProjectID  string `json:"project_id"`
	Kind       string `json:"kind"`
	SourcePath string `json:"source_path"`
	TargetPath string `json:"target_path,omitempty"`
	Status     Status `json:"status"`
	Error      string `json:"error,omitempty"`
}

// Result is the deterministic report of one migration run.
type Result struct {
	SourceDir string       `json:"source_dir"`
	TargetDir string       `json:"target_dir"`
	DryRun    bool         `json:"dry_run"`
	Projects  []string     `json:"projects"`
	Files     []FileResult `json:"files"`
	Created   int          `json:"created"`
	Unchanged int          `json:"unchanged"`
	Conflicts int          `json:"conflicts"`
	Failed    int          `json:"failed"`
}

// IncompleteError is returned when at least one file conflicted or failed.
// The result returned with it contains the per-file details.
type IncompleteError struct {
	Conflicts int
	Failed    int
}

func (e *IncompleteError) Error() string {
	return fmt.Sprintf("migrate: %d conflict(s), %d failed file(s); resolve them and rerun", e.Conflicts, e.Failed)
}

// Run imports the legacy knowledge records. A nil context is treated as
// context.Background.
func Run(ctx context.Context, opts Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	source, target, err := resolveDirs(opts)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		SourceDir: source,
		TargetDir: target,
		DryRun:    opts.DryRun,
		Projects:  []string{},
		Files:     []FileResult{},
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	projectsPath := filepath.Join(source, projectsDir)
	entries, err := os.ReadDir(projectsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return result, fmt.Errorf("migrate: %s is not a legacy brain: missing %s directory", source, projectsDir)
		}
		return result, fmt.Errorf("migrate: read source projects: %w", err)
	}
	if !opts.DryRun {
		if err := os.MkdirAll(filepath.Join(target, projectsDir), 0o755); err != nil {
			return result, fmt.Errorf("migrate: create target data dir: %w", err)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		projectID := entry.Name()
		sourceRel := path.Join(projectsDir, projectID)
		if entry.Type()&os.ModeSymlink != 0 {
			result.fail(projectID, "project", sourceRel, "", "symlinked project directories are not imported")
			continue
		}
		if !entry.IsDir() {
			continue
		}
		if err := validateProjectID(projectID); err != nil {
			result.fail(projectID, "project", sourceRel, "", err.Error())
			continue
		}
		result.Projects = append(result.Projects, projectID)
		processProject(ctx, filepath.Join(projectsPath, projectID), target, projectID, opts.DryRun, &result)
	}

	if err := ctx.Err(); err != nil {
		return result, err
	}
	if result.Conflicts > 0 || result.Failed > 0 {
		return result, &IncompleteError{Conflicts: result.Conflicts, Failed: result.Failed}
	}
	return result, nil
}

type converter func(projectID string, fileName string, raw []byte, modTime time.Time, sourceRel string) ([]byte, error)

type recordGroup struct {
	kind    string
	dir     string
	convert converter
}

func processProject(ctx context.Context, sourceDir, targetRoot, projectID string, dryRun bool, result *Result) {
	if err := ctx.Err(); err != nil {
		return
	}
	processState(ctx, sourceDir, targetRoot, projectID, dryRun, result)

	groups := []recordGroup{
		{kind: "note", dir: notesDir, convert: convertNotes},
		{kind: "decision", dir: decisionsDir, convert: convertDecision},
		{kind: "attempt", dir: attemptsDir, convert: convertAttempt},
		{kind: "handoff", dir: handoffsDir, convert: convertHandoff},
	}
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return
		}
		processGroup(ctx, sourceDir, targetRoot, projectID, group, dryRun, result)
	}
}

func processState(ctx context.Context, sourceDir, targetRoot, projectID string, dryRun bool, result *Result) {
	sourcePath := filepath.Join(sourceDir, stateFile)
	sourceRel := path.Join(projectsDir, projectID, stateFile)
	targetRel := path.Join(projectsDir, projectID, stateFile)
	info, err := os.Lstat(sourcePath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		result.fail(projectID, "state", sourceRel, targetRel, fmt.Sprintf("read source file: %v", err))
		return
	}
	if info.Mode()&os.ModeSymlink != 0 {
		result.fail(projectID, "state", sourceRel, targetRel, "symlinked source files are not imported")
		return
	}
	if !info.Mode().IsRegular() {
		return
	}
	raw, err := os.ReadFile(sourcePath)
	if err != nil {
		result.fail(projectID, "state", sourceRel, targetRel, fmt.Sprintf("read source file: %v", err))
		return
	}
	content, err := convertState(projectID, stateFile, raw, info.ModTime(), sourceRel)
	if err != nil {
		result.fail(projectID, "state", sourceRel, targetRel, err.Error())
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	storeFile(targetRoot, targetRel, content, projectID, "state", sourceRel, dryRun, result)
}

func processGroup(ctx context.Context, sourceDir, targetRoot, projectID string, group recordGroup, dryRun bool, result *Result) {
	dir := filepath.Join(sourceDir, group.dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		result.fail(projectID, group.kind, path.Join(projectsDir, projectID, group.dir), "", fmt.Sprintf("read source directory: %v", err))
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return
		}
		name := entry.Name()
		if strings.HasPrefix(name, tempFilePrefix) || !strings.EqualFold(filepath.Ext(name), ".md") {
			continue
		}
		sourceRel := path.Join(projectsDir, projectID, group.dir, name)
		targetRel := path.Join(projectsDir, projectID, group.dir, name)
		if entry.Type()&os.ModeSymlink != 0 {
			result.fail(projectID, group.kind, sourceRel, targetRel, "symlinked source files are not imported")
			continue
		}
		if !entry.Type().IsRegular() {
			continue
		}
		if err := validateFileName(name); err != nil {
			result.fail(projectID, group.kind, sourceRel, targetRel, err.Error())
			continue
		}
		info, err := entry.Info()
		if err != nil {
			result.fail(projectID, group.kind, sourceRel, targetRel, fmt.Sprintf("read source file info: %v", err))
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			result.fail(projectID, group.kind, sourceRel, targetRel, fmt.Sprintf("read source file: %v", err))
			continue
		}
		content, err := group.convert(projectID, name, raw, info.ModTime(), sourceRel)
		if err != nil {
			result.fail(projectID, group.kind, sourceRel, targetRel, err.Error())
			continue
		}
		storeFile(targetRoot, targetRel, content, projectID, group.kind, sourceRel, dryRun, result)
	}
}

func storeFile(targetRoot, targetRel string, content []byte, projectID, kind, sourceRel string, dryRun bool, result *Result) {
	targetPath, err := joinWithin(targetRoot, targetRel)
	if err != nil {
		result.fail(projectID, kind, sourceRel, targetRel, err.Error())
		return
	}
	if err := ensureNoSymlinkAncestors(targetRoot, filepath.Dir(targetPath)); err != nil {
		result.fail(projectID, kind, sourceRel, targetRel, err.Error())
		return
	}
	info, statErr := os.Lstat(targetPath)
	switch {
	case statErr == nil:
		if !info.Mode().IsRegular() {
			result.conflict(projectID, kind, sourceRel, targetRel, "target exists and is not a regular file")
			return
		}
		existing, readErr := os.ReadFile(targetPath)
		if readErr != nil {
			result.fail(projectID, kind, sourceRel, targetRel, fmt.Sprintf("read existing target: %v", readErr))
			return
		}
		if bytes.Equal(existing, content) {
			result.unchanged(projectID, kind, sourceRel, targetRel)
			return
		}
		result.conflict(projectID, kind, sourceRel, targetRel, "target exists with different content")
		return
	case !os.IsNotExist(statErr):
		result.fail(projectID, kind, sourceRel, targetRel, fmt.Sprintf("inspect target: %v", statErr))
		return
	}
	if dryRun {
		result.created(projectID, kind, sourceRel, targetRel)
		return
	}
	if err := writeFileAtomic(targetPath, content); err != nil {
		result.fail(projectID, kind, sourceRel, targetRel, err.Error())
		return
	}
	result.created(projectID, kind, sourceRel, targetRel)
}

func (r *Result) created(projectID, kind, sourceRel, targetRel string) {
	r.Created++
	r.Files = append(r.Files, FileResult{ProjectID: projectID, Kind: kind, SourcePath: sourceRel, TargetPath: targetRel, Status: StatusCreated})
}

func (r *Result) unchanged(projectID, kind, sourceRel, targetRel string) {
	r.Unchanged++
	r.Files = append(r.Files, FileResult{ProjectID: projectID, Kind: kind, SourcePath: sourceRel, TargetPath: targetRel, Status: StatusUnchanged})
}

func (r *Result) conflict(projectID, kind, sourceRel, targetRel, message string) {
	r.Conflicts++
	r.Files = append(r.Files, FileResult{ProjectID: projectID, Kind: kind, SourcePath: sourceRel, TargetPath: targetRel, Status: StatusConflict, Error: message})
}

func (r *Result) fail(projectID, kind, sourceRel, targetRel, message string) {
	r.Failed++
	r.Files = append(r.Files, FileResult{ProjectID: projectID, Kind: kind, SourcePath: sourceRel, TargetPath: targetRel, Status: StatusFailed, Error: message})
}

func resolveDirs(opts Options) (string, string, error) {
	sourceValue := strings.TrimSpace(opts.SourceDir)
	if sourceValue == "" {
		return "", "", fmt.Errorf("migrate: source dir is required")
	}
	targetValue := strings.TrimSpace(opts.TargetDir)
	if targetValue == "" {
		return "", "", fmt.Errorf("migrate: target dir is required")
	}
	source, err := filepath.Abs(sourceValue)
	if err != nil {
		return "", "", fmt.Errorf("migrate: resolve source dir: %w", err)
	}
	target, err := filepath.Abs(targetValue)
	if err != nil {
		return "", "", fmt.Errorf("migrate: resolve target dir: %w", err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return "", "", fmt.Errorf("migrate: source dir: %w", err)
	}
	if !sourceInfo.IsDir() {
		return "", "", fmt.Errorf("migrate: source %s is not a directory", source)
	}
	if rel, err := filepath.Rel(source, target); err == nil && rel == "." {
		return "", "", fmt.Errorf("migrate: source and target must be different directories")
	}
	if isWithin(source, target) || isWithin(target, source) {
		return "", "", fmt.Errorf("migrate: source and target directories must not be nested")
	}
	if targetInfo, err := os.Stat(target); err == nil {
		if !targetInfo.IsDir() {
			return "", "", fmt.Errorf("migrate: target %s is not a directory", target)
		}
	} else if !os.IsNotExist(err) {
		return "", "", fmt.Errorf("migrate: target dir: %w", err)
	}
	return source, target, nil
}

func isWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}

func joinWithin(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("migrate: unsafe target path %q", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("migrate: unsafe target path %q", rel)
	}
	full := filepath.Join(root, clean)
	check, err := filepath.Rel(root, full)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(os.PathSeparator)) || filepath.IsAbs(check) {
		return "", fmt.Errorf("migrate: unsafe target path %q", rel)
	}
	return full, nil
}

// ensureNoSymlinkAncestors rejects target directories that escape through a
// symlink. The data root itself may be a user-managed symlink; every component
// below it must be a real directory.
func ensureNoSymlinkAncestors(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return fmt.Errorf("migrate: resolve target directory: %w", err)
	}
	if rel == "." {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("migrate: unsafe target directory %q", dir)
	}
	current := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("migrate: inspect target directory %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("migrate: target path %s contains a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("migrate: target path %s is not a directory", current)
		}
	}
	return nil
}

func validateProjectID(projectID string) error {
	if projectID == "" {
		return fmt.Errorf("project id is empty")
	}
	if len(projectID) > 100 {
		return fmt.Errorf("project id %q is longer than 100 bytes", projectID)
	}
	if projectID != strings.TrimSpace(projectID) {
		return fmt.Errorf("project id %q has surrounding whitespace", projectID)
	}
	for _, char := range projectID {
		allowed := char >= 'a' && char <= 'z' ||
			char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' ||
			char == '.' || char == '_' || char == '-'
		if !allowed {
			return fmt.Errorf("project id %q may contain only letters, digits, dot, underscore and dash", projectID)
		}
	}
	first := projectID[0]
	if !(first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z' || first >= '0' && first <= '9') {
		return fmt.Errorf("project id %q must start with a letter or digit", projectID)
	}
	if strings.Contains(projectID, "..") {
		return fmt.Errorf("project id %q must not contain ..", projectID)
	}
	if strings.HasSuffix(projectID, ".") {
		return fmt.Errorf("project id %q must not end with a dot", projectID)
	}
	if isWindowsReservedName(projectID) {
		return fmt.Errorf("project id %q is a reserved file name", projectID)
	}
	return nil
}

func validateFileName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("unsafe file name %q", name)
	}
	if filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("unsafe file name %q", name)
	}
	for _, char := range name {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("unsafe file name %q", name)
		}
	}
	if strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return fmt.Errorf("unsafe file name %q", name)
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return fmt.Errorf("file name %q contains characters that are not portable", name)
	}
	if runtime.GOOS == "windows" && isWindowsReservedName(name) {
		return fmt.Errorf("file name %q is a reserved file name", name)
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

func writeFileAtomic(targetPath string, data []byte) error {
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("migrate: create directory %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, tempFilePrefix+"*")
	if err != nil {
		return fmt.Errorf("migrate: create temp file in %s: %w", dir, err)
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
		return fmt.Errorf("migrate: write temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("migrate: sync temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("migrate: close temp file: %w", err)
	}
	if err := os.Chmod(tempName, 0o644); err != nil {
		return fmt.Errorf("migrate: set file mode: %w", err)
	}
	if err := os.Rename(tempName, targetPath); err != nil {
		return fmt.Errorf("migrate: replace %s: %w", targetPath, err)
	}
	cleanup = false
	return nil
}
