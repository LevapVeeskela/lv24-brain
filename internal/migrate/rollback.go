package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Migration backups live under <target>/backups/migrate-<UTC timestamp>/ and
// hold manifest.json plus the pre-migration copies of the files that the CLI
// rewrites after the importer runs (registry.json and the brain.yaml config).

const (
	// ManifestFormatVersion is the manifest format written and accepted by this build.
	ManifestFormatVersion = 1

	backupsDir       = "backups"
	backupDirPrefix  = "migrate-"
	backupTimeLayout = "20060102T150405Z"
	manifestFile     = "manifest.json"
	registryName     = "registry.json"
	backupSuffix     = ".before"
)

// Manifest records what one non-dry-run migration changed so it can be reverted.
type Manifest struct {
	FormatVersion int    `json:"format_version"`
	CreatedAt     string `json:"created_at"`
	SourceDir     string `json:"source_dir"`
	TargetDir     string `json:"target_dir"`
	ConfigPath    string `json:"config_path"`
	// DirsBefore lists target-relative directories under projects/ that already
	// existed before the migration. Rollback never removes them.
	DirsBefore []string `json:"dirs_before"`
	// Files lists every file the importer created, with the hash of the bytes written.
	Files    []ManifestFile    `json:"files"`
	Registry ManifestFileState `json:"registry"`
	Config   ManifestFileState `json:"config"`
}

// ManifestFile is one file created by the migration.
type ManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// ManifestFileState describes a file rewritten by the CLI after the importer ran.
type ManifestFileState struct {
	ExistedBefore bool   `json:"existed_before"`
	SHA256Before  string `json:"sha256_before,omitempty"`
	// SHA256After is the hash after the migration; empty when the file was absent.
	SHA256After string `json:"sha256_after,omitempty"`
}

// Backup is a migration backup in progress. BeginBackup captures the files that
// the migration may rewrite before any write happens; Finish writes the manifest.
type Backup struct {
	dir        string
	targetDir  string
	sourceDir  string
	configPath string
	createdAt  time.Time
	dirsBefore []string
	registry   ManifestFileState
	config     ManifestFileState
}

// BeginBackup creates <target>/backups/migrate-<UTC timestamp>/ and copies the
// current registry.json and config file (when they exist) into it. It must run
// before migrate.Run and before any CLI write.
func BeginBackup(opts Options, configPath string, now time.Time) (*Backup, error) {
	source, target, err := resolveDirs(opts)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(configPath) == "" {
		return nil, fmt.Errorf("migrate: config path is required for the migration backup")
	}
	cfgAbs, err := filepath.Abs(strings.TrimSpace(configPath))
	if err != nil {
		return nil, fmt.Errorf("migrate: resolve config path: %w", err)
	}
	backupsRoot := filepath.Join(target, backupsDir)
	if err := ensureNoSymlinkAncestors(target, backupsRoot); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(backupsRoot, 0o755); err != nil {
		return nil, fmt.Errorf("migrate: create backups directory: %w", err)
	}
	createdAt := now.UTC().Truncate(time.Second)
	dir, err := createBackupDir(backupsRoot, backupDirPrefix+createdAt.Format(backupTimeLayout))
	if err != nil {
		return nil, err
	}
	dirs, err := listProjectDirs(target)
	if err != nil {
		return nil, err
	}
	registry, err := captureState(filepath.Join(target, registryName), filepath.Join(dir, registryName+backupSuffix))
	if err != nil {
		return nil, err
	}
	cfgState, err := captureState(cfgAbs, filepath.Join(dir, filepath.Base(cfgAbs)+backupSuffix))
	if err != nil {
		return nil, err
	}
	return &Backup{
		dir:        dir,
		targetDir:  target,
		sourceDir:  source,
		configPath: cfgAbs,
		createdAt:  createdAt,
		dirsBefore: dirs,
		registry:   registry,
		config:     cfgState,
	}, nil
}

// createBackupDir creates base under root. When a backup with the same second
// already exists, it appends a fixed-width suffix (-02, -03, ...) so names keep
// sorting chronologically and two runs in one second never collide.
func createBackupDir(root, base string) (string, error) {
	for attempt := 1; attempt < 100; attempt++ {
		name := base
		if attempt > 1 {
			name = fmt.Sprintf("%s-%02d", base, attempt)
		}
		dir := filepath.Join(root, name)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", fmt.Errorf("migrate: create backup directory: %w", err)
		}
	}
	return "", fmt.Errorf("migrate: too many backups created at %s", base)
}

// Finish writes manifest.json for the migration described by result and returns
// its path. Call it even when the migration returned an IncompleteError or the
// CLI failed afterwards, so files that were created can still be rolled back.
func (b *Backup) Finish(result Result) (string, error) {
	manifest := Manifest{
		FormatVersion: ManifestFormatVersion,
		CreatedAt:     b.createdAt.Format(time.RFC3339),
		SourceDir:     b.sourceDir,
		TargetDir:     b.targetDir,
		ConfigPath:    b.configPath,
		DirsBefore:    b.dirsBefore,
		Files:         []ManifestFile{},
		Registry:      b.registry,
		Config:        b.config,
	}
	for _, file := range result.Files {
		if file.Status == StatusCreated {
			manifest.Files = append(manifest.Files, ManifestFile{Path: file.TargetPath, SHA256: file.SHA256})
		}
	}
	registryAfter, err := hashState(filepath.Join(b.targetDir, registryName))
	if err != nil {
		return "", err
	}
	manifest.Registry.SHA256After = registryAfter
	configAfter, err := hashState(b.configPath)
	if err != nil {
		return "", err
	}
	manifest.Config.SHA256After = configAfter
	manifestPath := filepath.Join(b.dir, manifestFile)
	if err := WriteManifest(manifestPath, manifest); err != nil {
		return "", err
	}
	return manifestPath, nil
}

// WriteManifest atomically writes manifest as indented JSON.
func WriteManifest(manifestPath string, manifest Manifest) error {
	if manifest.FormatVersion == 0 {
		manifest.FormatVersion = ManifestFormatVersion
	}
	if manifest.DirsBefore == nil {
		manifest.DirsBefore = []string{}
	}
	if manifest.Files == nil {
		manifest.Files = []ManifestFile{}
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("migrate: encode manifest: %w", err)
	}
	return writeFileAtomic(manifestPath, append(raw, '\n'))
}

// LoadManifest reads and format-checks a migration manifest. Paths inside it are
// validated by Rollback, not here.
func LoadManifest(manifestPath string) (Manifest, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("migrate: read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("migrate: parse manifest %s: %w", manifestPath, err)
	}
	if manifest.FormatVersion != ManifestFormatVersion {
		return Manifest{}, fmt.Errorf("migrate: unsupported manifest format version %d in %s", manifest.FormatVersion, manifestPath)
	}
	return manifest, nil
}

// FindLatestManifest returns the manifest of the newest migrate-* backup under
// <targetDir>/backups. Backup directory names sort chronologically.
func FindLatestManifest(targetDir string) (string, error) {
	target, err := filepath.Abs(strings.TrimSpace(targetDir))
	if err != nil {
		return "", fmt.Errorf("migrate: resolve target dir: %w", err)
	}
	root := filepath.Join(target, backupsDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("migrate: no migration backups under %s", root)
		}
		return "", fmt.Errorf("migrate: read backups directory: %w", err)
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), backupDirPrefix) {
			names = append(names, entry.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names {
		manifest := filepath.Join(root, name, manifestFile)
		if info, statErr := os.Stat(manifest); statErr == nil && info.Mode().IsRegular() {
			return manifest, nil
		}
	}
	return "", fmt.Errorf("migrate: no migration manifest under %s", root)
}

// RollbackOptions selects the manifest to revert and whether to only report.
type RollbackOptions struct {
	ManifestPath string
	DryRun       bool
}

// RollbackStatus is the outcome of one rollback step.
type RollbackStatus string

const (
	// RollbackRemoved means a migration-created file or directory was deleted.
	RollbackRemoved RollbackStatus = "removed"
	// RollbackWouldRemove is RollbackRemoved in dry-run mode.
	RollbackWouldRemove RollbackStatus = "would remove"
	// RollbackRestored means a pre-migration copy was written back.
	RollbackRestored RollbackStatus = "restored"
	// RollbackWouldRestore is RollbackRestored in dry-run mode.
	RollbackWouldRestore RollbackStatus = "would restore"
	// RollbackAlreadyRemoved means a migration-created file was already gone.
	RollbackAlreadyRemoved RollbackStatus = "already removed"
	// RollbackModified means the file changed after the migration and was kept.
	RollbackModified RollbackStatus = "modified since migration"
	// RollbackSkipped means the file was not touched for a safety reason.
	RollbackSkipped RollbackStatus = "skipped"
	// RollbackKept means a directory was left because it is not empty.
	RollbackKept RollbackStatus = "kept"
	// RollbackUnchanged means there was nothing to revert for this file.
	RollbackUnchanged RollbackStatus = "unchanged"
)

// RollbackEntry is one line of the rollback report.
type RollbackEntry struct {
	Kind   string         `json:"kind"`
	Path   string         `json:"path"`
	Status RollbackStatus `json:"status"`
	Detail string         `json:"detail,omitempty"`
}

// RollbackResult is the deterministic report of one rollback run.
type RollbackResult struct {
	ManifestPath string          `json:"manifest_path"`
	TargetDir    string          `json:"target_dir"`
	DryRun       bool            `json:"dry_run"`
	Entries      []RollbackEntry `json:"entries"`
	// Skipped counts files kept because they were modified or could not be
	// reverted safely. A non-zero value means the target is not fully reverted.
	Skipped int `json:"skipped"`
}

type rollbackAction int

const (
	actionNone rollbackAction = iota
	actionDelete
	actionRestore
)

// rollbackStep is one planned change. Planning is read-only, so dry-run and a
// real run report identical statuses.
type rollbackStep struct {
	entry  RollbackEntry
	action rollbackAction
	target string
	data   []byte
}

type plannedFile struct {
	rel  string
	full string
	sha  string
}

// Rollback reverts the migration described by opts.ManifestPath. Every manifest
// path is validated before any change is made. Files that changed after the
// migration are kept and reported; the result's Skipped count is non-zero then.
func Rollback(ctx context.Context, opts RollbackOptions) (RollbackResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return RollbackResult{}, err
	}
	manifestPath, err := filepath.Abs(strings.TrimSpace(opts.ManifestPath))
	if err != nil {
		return RollbackResult{}, fmt.Errorf("migrate: resolve manifest path: %w", err)
	}
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return RollbackResult{}, err
	}
	target, backupDir, err := manifestLocation(manifestPath, manifest)
	if err != nil {
		return RollbackResult{}, err
	}
	files, err := planManifestFiles(target, manifest.Files)
	if err != nil {
		return RollbackResult{}, err
	}
	cfgPath := manifest.ConfigPath
	cfgInside := isWithin(target, cfgPath)
	steps := []rollbackStep{}
	for _, file := range files {
		step, planErr := planCreatedFile(file)
		if planErr != nil {
			return RollbackResult{}, planErr
		}
		steps = append(steps, step)
	}
	registryStep, err := planState("registry", registryName, filepath.Join(target, registryName), true,
		manifest.Registry, filepath.Join(backupDir, registryName+backupSuffix))
	if err != nil {
		return RollbackResult{}, err
	}
	configStep, err := planState("config", cfgPath, cfgPath, cfgInside,
		manifest.Config, filepath.Join(backupDir, filepath.Base(cfgPath)+backupSuffix))
	if err != nil {
		return RollbackResult{}, err
	}
	steps = append(steps, registryStep, configStep)

	result := RollbackResult{
		ManifestPath: manifestPath,
		TargetDir:    target,
		DryRun:       opts.DryRun,
		Entries:      []RollbackEntry{},
	}
	gone := map[string]bool{}
	for _, step := range steps {
		switch step.action {
		case actionDelete:
			if !opts.DryRun {
				if err := os.Remove(step.target); err != nil {
					return result, fmt.Errorf("migrate: remove %s: %w", step.target, err)
				}
			}
			gone[step.target] = true
		case actionRestore:
			if !opts.DryRun {
				if err := writeFileAtomic(step.target, step.data); err != nil {
					return result, err
				}
			}
		}
		result.add(step.entry, opts.DryRun)
	}

	dirSteps, err := planDirectories(target, files, manifest.DirsBefore, gone, opts.DryRun)
	if err != nil {
		return result, err
	}
	for _, entry := range dirSteps {
		result.add(entry, opts.DryRun)
	}
	return result, nil
}

func (r *RollbackResult) add(entry RollbackEntry, dryRun bool) {
	if dryRun {
		switch entry.Status {
		case RollbackRemoved:
			entry.Status = RollbackWouldRemove
		case RollbackRestored:
			entry.Status = RollbackWouldRestore
		}
	}
	if entry.Status == RollbackModified || entry.Status == RollbackSkipped {
		r.Skipped++
	}
	r.Entries = append(r.Entries, entry)
}

// manifestLocation validates the manifest's target and checks that the manifest
// sits in <target>/backups/<dir>/manifest.json.
func manifestLocation(manifestPath string, manifest Manifest) (string, string, error) {
	if !filepath.IsAbs(manifest.TargetDir) {
		return "", "", fmt.Errorf("migrate: manifest target_dir must be absolute")
	}
	target := filepath.Clean(manifest.TargetDir)
	backupDir := filepath.Dir(manifestPath)
	if !samePath(filepath.Dir(backupDir), filepath.Join(target, backupsDir)) {
		return "", "", fmt.Errorf("migrate: manifest %s is not inside %s", manifestPath, filepath.Join(target, backupsDir))
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", "", fmt.Errorf("migrate: rollback target: %w", err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("migrate: rollback target %s is not a directory", target)
	}
	if !filepath.IsAbs(manifest.ConfigPath) {
		return "", "", fmt.Errorf("migrate: manifest config_path must be absolute")
	}
	return target, backupDir, nil
}

func planManifestFiles(target string, files []ManifestFile) ([]plannedFile, error) {
	planned := make([]plannedFile, 0, len(files))
	for _, file := range files {
		rel, err := validateManifestRel(file.Path)
		if err != nil {
			return nil, err
		}
		if !validSHA256(file.SHA256) {
			return nil, fmt.Errorf("migrate: manifest hash for %q is invalid", file.Path)
		}
		full, err := joinWithin(target, rel)
		if err != nil {
			return nil, err
		}
		if err := ensureNoSymlinkAncestors(target, filepath.Dir(full)); err != nil {
			return nil, err
		}
		planned = append(planned, plannedFile{rel: rel, full: full, sha: file.SHA256})
	}
	sort.Slice(planned, func(i, j int) bool { return planned[i].rel < planned[j].rel })
	return planned, nil
}

// validateManifestRel accepts only clean slash paths under projects/<id>/.
func validateManifestRel(rel string) (string, error) {
	if rel == "" || path.IsAbs(rel) || strings.ContainsAny(rel, `\:`) {
		return "", fmt.Errorf("migrate: unsafe manifest path %q", rel)
	}
	clean := path.Clean(rel)
	if clean != rel || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("migrate: unsafe manifest path %q", rel)
	}
	parts := strings.Split(clean, "/")
	if len(parts) < 3 || parts[0] != projectsDir {
		return "", fmt.Errorf("migrate: manifest path %q is outside %s/", rel, projectsDir)
	}
	return clean, nil
}

func planCreatedFile(file plannedFile) (rollbackStep, error) {
	step := rollbackStep{
		entry:  RollbackEntry{Kind: "file", Path: file.rel},
		target: file.full,
	}
	info, err := os.Lstat(file.full)
	if err != nil {
		if os.IsNotExist(err) {
			step.entry.Status = RollbackAlreadyRemoved
			return step, nil
		}
		return step, fmt.Errorf("migrate: inspect %s: %w", file.full, err)
	}
	if !info.Mode().IsRegular() {
		step.entry.Status = RollbackModified
		step.entry.Detail = "not a regular file; kept"
		return step, nil
	}
	raw, err := os.ReadFile(file.full)
	if err != nil {
		return step, fmt.Errorf("migrate: read %s: %w", file.full, err)
	}
	if sha256Hex(raw) != file.sha {
		step.entry.Status = RollbackModified
		step.entry.Detail = "edited after migration; kept"
		return step, nil
	}
	step.action = actionDelete
	step.entry.Status = RollbackRemoved
	return step, nil
}

// planState decides what to do with registry.json or the config file. display is
// the path shown in the report. inside reports whether the file may be touched.
func planState(kind, display, file string, inside bool, state ManifestFileState, backupFile string) (rollbackStep, error) {
	step := rollbackStep{entry: RollbackEntry{Kind: kind, Path: display}, target: file}
	info, err := os.Lstat(file)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return step, fmt.Errorf("migrate: inspect %s: %w", file, err)
	}
	regular := exists && info.Mode().IsRegular()
	current := ""
	if regular {
		raw, readErr := os.ReadFile(file)
		if readErr != nil {
			return step, fmt.Errorf("migrate: read %s: %w", file, readErr)
		}
		current = sha256Hex(raw)
	}
	switch {
	case !exists && !state.ExistedBefore && state.SHA256After == "":
		step.entry.Status = RollbackUnchanged
		return step, nil
	case !exists && !state.ExistedBefore:
		step.entry.Status = RollbackAlreadyRemoved
		return step, nil
	case !exists:
		step.entry.Status = RollbackModified
		step.entry.Detail = "missing since migration; not restored"
		return step, nil
	case !regular || state.SHA256After == "" || current != state.SHA256After:
		step.entry.Status = RollbackModified
		step.entry.Detail = "changed since migration; left in place"
		return step, nil
	}
	if !inside {
		step.entry.Status = RollbackSkipped
		step.entry.Detail = "outside target directory; revert by hand"
		return step, nil
	}
	if !state.ExistedBefore {
		step.action = actionDelete
		step.entry.Status = RollbackRemoved
		return step, nil
	}
	data, err := os.ReadFile(backupFile)
	if err != nil {
		return step, fmt.Errorf("migrate: read backup %s: %w", backupFile, err)
	}
	if sha256Hex(data) != state.SHA256Before {
		return step, fmt.Errorf("migrate: backup %s does not match the manifest", backupFile)
	}
	step.action = actionRestore
	step.data = data
	step.entry.Status = RollbackRestored
	return step, nil
}

// planDirectories returns removal steps for directories that the migration
// created (parents of created files, not listed in dirsBefore). A directory is
// removed only when it is empty once the planned deletions in gone are applied.
func planDirectories(target string, files []plannedFile, dirsBefore []string, gone map[string]bool, dryRun bool) ([]RollbackEntry, error) {
	before := make(map[string]bool, len(dirsBefore))
	for _, dir := range dirsBefore {
		before[path.Clean(dir)] = true
	}
	candidates := map[string]bool{}
	for _, file := range files {
		for dir := path.Dir(file.rel); dir != "." && dir != ""; dir = path.Dir(dir) {
			if !before[dir] {
				candidates[dir] = true
			}
		}
	}
	ordered := make([]string, 0, len(candidates))
	for dir := range candidates {
		ordered = append(ordered, dir)
	}
	sort.Slice(ordered, func(i, j int) bool {
		di, dj := strings.Count(ordered[i], "/"), strings.Count(ordered[j], "/")
		if di != dj {
			return di > dj
		}
		return ordered[i] < ordered[j]
	})

	entries := []RollbackEntry{}
	for _, rel := range ordered {
		full := filepath.Join(target, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("migrate: inspect %s: %w", full, err)
		}
		if !info.IsDir() {
			continue
		}
		empty, err := dirEmptyExcept(full, gone)
		if err != nil {
			return nil, err
		}
		entry := RollbackEntry{Kind: "directory", Path: rel}
		if !empty {
			entry.Status = RollbackKept
			entry.Detail = "not empty"
			entries = append(entries, entry)
			continue
		}
		if !dryRun {
			if err := os.Remove(full); err != nil {
				entry.Status = RollbackKept
				entry.Detail = fmt.Sprintf("could not remove: %v", err)
				entries = append(entries, entry)
				continue
			}
		}
		gone[full] = true
		entry.Status = RollbackRemoved
		entries = append(entries, entry)
	}
	return entries, nil
}

func dirEmptyExcept(dir string, gone map[string]bool) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("migrate: read directory %s: %w", dir, err)
	}
	for _, entry := range entries {
		if !gone[filepath.Join(dir, entry.Name())] {
			return false, nil
		}
	}
	return true, nil
}

// captureState copies an existing regular file into the backup directory and
// records its hash. A missing file is recorded as absent.
func captureState(file, backupFile string) (ManifestFileState, error) {
	info, err := os.Lstat(file)
	if err != nil {
		if os.IsNotExist(err) {
			return ManifestFileState{}, nil
		}
		return ManifestFileState{}, fmt.Errorf("migrate: inspect %s: %w", file, err)
	}
	if !info.Mode().IsRegular() {
		return ManifestFileState{}, fmt.Errorf("migrate: %s is not a regular file; refusing to migrate without a backup", file)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return ManifestFileState{}, fmt.Errorf("migrate: read %s: %w", file, err)
	}
	if err := writeFileAtomic(backupFile, raw); err != nil {
		return ManifestFileState{}, err
	}
	return ManifestFileState{ExistedBefore: true, SHA256Before: sha256Hex(raw)}, nil
}

// hashState returns the hash of a regular file, or "" when it is absent or not a
// regular file.
func hashState(file string) (string, error) {
	info, err := os.Lstat(file)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("migrate: inspect %s: %w", file, err)
	}
	if !info.Mode().IsRegular() {
		return "", nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("migrate: read %s: %w", file, err)
	}
	return sha256Hex(raw), nil
}

// listProjectDirs returns the target-relative slash paths of every directory
// under projects/, including projects itself, as they exist before a run.
func listProjectDirs(target string) ([]string, error) {
	root := filepath.Join(target, projectsDir)
	dirs := []string{}
	if _, err := os.Lstat(root); err != nil {
		if os.IsNotExist(err) {
			return dirs, nil
		}
		return nil, fmt.Errorf("migrate: inspect %s: %w", root, err)
	}
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(target, current)
		if relErr != nil {
			return relErr
		}
		dirs = append(dirs, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("migrate: scan %s: %w", root, err)
	}
	sort.Strings(dirs)
	return dirs, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
