package fsstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// mergesDir holds the merge manifests under the data directory.
	mergesDir = "merges"
	// mergeManifestVersion is bumped when the manifest layout changes.
	mergeManifestVersion = 1
	// The manifest lifecycle: pending is written before the first record
	// changes, applied after the last one, aborted when a write failed and the
	// merge rolled itself back, rolled_back after a successful restore.
	mergeManifestPending  = "pending"
	mergeManifestApplied  = "applied"
	mergeManifestAborted  = "aborted"
	mergeManifestRollback = "rolled_back"
)

// mergeManifest is the on-disk record of one applied merge. It stores the
// before bytes so a rollback can restore the exact state, not a re-rendering.
type mergeManifest struct {
	Version      int                 `json:"version"`
	CreatedAt    time.Time           `json:"created_at"`
	Status       string              `json:"status"`
	RolledBackAt time.Time           `json:"rolled_back_at,omitzero"`
	ProjectID    string              `json:"project_id"`
	Kind         string              `json:"kind"`
	Survivor     string              `json:"survivor"`
	Duplicates   []string            `json:"duplicates"`
	MergedFrom   []string            `json:"merged_from"`
	Fingerprint  string              `json:"fingerprint"`
	Files        []mergeManifestFile `json:"files"`
}

// mergeManifestFile is the recorded state of one touched file.
type mergeManifestFile struct {
	Path          string `json:"path"`
	Change        string `json:"change"`
	BeforeSHA256  string `json:"before_sha256"`
	AfterSHA256   string `json:"after_sha256"`
	BeforeBase64  string `json:"before_base64,omitempty"`
	BeforeMissing bool   `json:"before_missing,omitempty"`
	MovedTo       string `json:"moved_to,omitempty"`
	// After is the full on-disk identity of the applied result. Old manifests
	// have no stamp; their rollback falls back to the hash checks.
	After mergeFileStamp `json:"after,omitzero"`
}

// mergeFileStamp is the on-disk identity of a merge target: existence plus
// SHA-256, size and modification time. Apply and rollback compare it under the
// per-file lock before writing, so a plan built against other bytes is refused
// instead of overwriting newer work.
type mergeFileStamp struct {
	Exists  bool      `json:"exists,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"mtime,omitzero"`
}

// StalePlanError reports that the files a merge plan or merge rollback targets
// changed after the plan was computed. Nothing was written. It unwraps to
// domain.ErrConflict and carries the "stale_plan" code for machine callers.
type StalePlanError struct {
	Operation   string
	Paths       []string
	Fingerprint string
}

// Error implements error.
func (e *StalePlanError) Error() string {
	message := "stale_plan: " + e.Operation + " refused because the records changed since the plan was shown; nothing was written, run the plan again"
	if len(e.Paths) > 0 {
		message += ": " + strings.Join(e.Paths, ", ")
	}
	if e.Fingerprint != "" {
		message += " (plan " + e.Fingerprint + ")"
	}
	return message
}

// Unwrap maps a stale plan to the domain conflict sentinel so existing error
// branching keeps working.
func (e *StalePlanError) Unwrap() error { return domain.ErrConflict }

// Code returns the stable machine-readable code of the refusal.
func (e *StalePlanError) Code() string { return "stale_plan" }

// ApplyMerge writes a merge plan in two phases: the manifest is published
// before the first record changes, and every byte the merge replaced is stored
// in it, so a failed apply or a later rollback can restore the exact state. A
// plan whose inputs changed since it was shown is refused with a conflict.
func (s *Store) ApplyMerge(ctx context.Context, plan ports.MergePlan) (ports.MergeOutcome, error) {
	if err := contextError(ctx); err != nil {
		return ports.MergeOutcome{}, err
	}
	if len(plan.Conflicts) > 0 {
		return ports.MergeOutcome{}, domain.NewValidationError("merge", "the plan has conflicts: "+strings.Join(plan.Conflicts, "; "))
	}
	request := ports.MergeRequest{
		ProjectID: plan.ProjectID, Survivor: plan.Survivor.ID, Duplicates: duplicateIDs(plan),
	}
	work, err := s.planMerge(ctx, request)
	if err != nil {
		return ports.MergeOutcome{}, err
	}
	if len(work.plan.Conflicts) > 0 {
		return ports.MergeOutcome{}, domain.NewValidationError("merge", "the plan has conflicts: "+strings.Join(work.plan.Conflicts, "; "))
	}
	if !work.plan.Changed {
		// Everything the caller asked for is already in place: an idempotent
		// repeat is a success with no writes.
		return ports.MergeOutcome{}, nil
	}
	if plan.Fingerprint != "" && work.plan.Fingerprint != plan.Fingerprint {
		return ports.MergeOutcome{}, fmt.Errorf("merge %s: %w", plan.Survivor.ID, &StalePlanError{
			Operation: "merge apply", Fingerprint: plan.Fingerprint,
		})
	}
	stamps, err := planMergeStamps(work.files)
	if err != nil {
		return ports.MergeOutcome{}, fmt.Errorf("merge %s: %w", plan.Survivor.ID, err)
	}
	// Every target takes its own lock before the plan is re-checked, so a
	// writer that changed a file after the plan was computed is refused
	// instead of being overwritten. Locks are taken in sorted order, which
	// keeps concurrent merges from deadlocking on each other.
	release, err := lockMergePaths(ctx, mergeWorkLockPaths(work.files))
	if err != nil {
		return ports.MergeOutcome{}, err
	}
	defer release()
	if err := s.verifyMergeStamps(stamps); err != nil {
		return ports.MergeOutcome{}, fmt.Errorf("merge %s: %w", plan.Survivor.ID, err)
	}
	manifest := s.newMergeManifest(work)
	manifestPath, err := s.writeMergeManifest(manifest)
	if err != nil {
		return ports.MergeOutcome{}, err
	}
	applied, applyErr := s.applyMergeWork(work)
	outcome := ports.MergeOutcome{Manifest: s.manifestRef(manifestPath), Files: applied}
	if applyErr != nil {
		manifest.Status = mergeManifestAborted
		_ = s.rewriteMergeManifest(manifestPath, manifest)
		return outcome, applyErr
	}
	// Record what the merge actually wrote, so a rollback can prove the files
	// are still the applied result and not a newer edit.
	s.stampAppliedMergeFiles(manifest)
	manifest.Status = mergeManifestApplied
	if err := s.rewriteMergeManifest(manifestPath, manifest); err != nil {
		// The merge happened; a manifest that could not be re-stamped still
		// holds the before bytes, and rollback accepts a pending manifest.
		return outcome, fmt.Errorf("merge applied but the manifest could not be finalized: %w", err)
	}
	return outcome, nil
}

// RollbackMerge restores the state recorded in a merge manifest byte for byte.
// It refuses the whole rollback when a file changed after the merge, naming the
// files instead of overwriting newer work.
func (s *Store) RollbackMerge(ctx context.Context, manifestRef string) (ports.RollbackOutcome, error) {
	if err := contextError(ctx); err != nil {
		return ports.RollbackOutcome{}, err
	}
	path, err := s.resolveMergeManifest(manifestRef)
	if err != nil {
		return ports.RollbackOutcome{}, err
	}
	manifest, err := readMergeManifest(path)
	if err != nil {
		return ports.RollbackOutcome{}, err
	}
	outcome := ports.RollbackOutcome{Manifest: s.manifestRef(path)}
	if manifest.Status == mergeManifestRollback {
		outcome.AlreadyRolledBack = true
		return outcome, nil
	}
	lockPaths, err := s.mergeManifestLockPaths(manifest.Files)
	if err != nil {
		return outcome, err
	}
	release, err := lockMergePaths(ctx, lockPaths)
	if err != nil {
		return outcome, err
	}
	defer release()
	var conflicts []string
	var stale []string
	for _, file := range manifest.Files {
		conflict, err := s.mergeRollbackConflict(file)
		if err != nil {
			return outcome, err
		}
		if conflict == "" {
			conflict, err = s.mergeRollbackStampConflict(file)
			if err != nil {
				return outcome, err
			}
		}
		if conflict != "" {
			conflicts = append(conflicts, conflict)
			stale = append(stale, file.Path)
		}
	}
	if len(conflicts) > 0 {
		outcome.Conflicts = conflicts
		return outcome, fmt.Errorf("merge rollback %s refused, files changed since the merge: %s: %w",
			s.manifestRef(path), strings.Join(conflicts, "; "), &StalePlanError{Operation: "merge rollback", Paths: stale})
	}
	for _, file := range manifest.Files {
		if err := s.restoreMergeFile(file); err != nil {
			return outcome, err
		}
		outcome.Files = append(outcome.Files, ports.MergeFile{Path: file.Path, Change: file.Change})
	}
	manifest.Status = mergeManifestRollback
	manifest.RolledBackAt = s.nowUTC()
	if err := s.rewriteMergeManifest(path, manifest); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// newMergeManifest records the before and after state of every touched file.
func (s *Store) newMergeManifest(work *mergeWork) *mergeManifest {
	manifest := &mergeManifest{
		Version: mergeManifestVersion, CreatedAt: s.nowUTC(), Status: mergeManifestPending,
		ProjectID: work.plan.ProjectID, Kind: work.plan.Kind,
		Survivor: work.plan.Survivor.ID, Duplicates: duplicateIDs(work.plan),
		MergedFrom: work.plan.MergedFrom, Fingerprint: work.plan.Fingerprint,
	}
	for _, file := range work.files {
		entry := mergeManifestFile{
			Path: file.relPath, Change: file.change,
			BeforeSHA256: hashBytes(file.before), AfterSHA256: hashBytes(file.after),
			MovedTo: file.movedTo,
		}
		if file.beforeMissing {
			entry.BeforeMissing = true
		} else {
			entry.BeforeBase64 = base64.StdEncoding.EncodeToString(file.before)
		}
		manifest.Files = append(manifest.Files, entry)
	}
	return manifest
}

// writeMergeManifest publishes a new manifest under merges/ and returns its
// absolute path. The file is appended, never replaced, so manifests are an
// audit trail of every merge.
func (s *Store) writeMergeManifest(manifest *mergeManifest) (string, error) {
	dir := filepath.Join(s.root, mergesDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("fsstore: create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("fsstore: encode merge manifest: %w", err)
	}
	base := manifest.CreatedAt.UTC().Format("2006-01-02T150405.000000000Z")
	for attempt := 1; attempt <= maxNameAttempts; attempt++ {
		name := base + ".json"
		if attempt > 1 {
			name = fmt.Sprintf("%s-%d.json", base, attempt)
		}
		path := filepath.Join(dir, name)
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", fmt.Errorf("fsstore: reserve merge manifest %s: %w", path, err)
		}
		_ = file.Close()
		if err := writeFileAtomic(path, data); err != nil {
			_ = os.Remove(path)
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("fsstore: no free merge manifest name for %s", base)
}

// rewriteMergeManifest updates a manifest in place.
func (s *Store) rewriteMergeManifest(path string, manifest *mergeManifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("fsstore: encode merge manifest: %w", err)
	}
	return writeFileAtomic(path, data)
}

// readMergeManifest decodes one manifest.
func readMergeManifest(path string) (*mergeManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fsstore: read merge manifest %s: %w", path, err)
	}
	var manifest mergeManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("fsstore: parse merge manifest %s: %w", path, err)
	}
	if manifest.Version > mergeManifestVersion {
		return nil, domain.NewValidationError("merge.manifest",
			fmt.Sprintf("%s: version %d requires a newer engine", path, manifest.Version))
	}
	return &manifest, nil
}

// resolveMergeManifest maps a user-provided manifest reference to a path inside
// the data directory. A bare name is looked up under merges/.
func (s *Store) resolveMergeManifest(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", domain.NewValidationError("manifest", "is required")
	}
	var candidates []string
	if filepath.IsAbs(ref) {
		candidates = append(candidates, ref)
	} else {
		candidates = append(candidates, filepath.Join(s.root, filepath.FromSlash(ref)))
		base := filepath.Base(ref)
		candidates = append(candidates, filepath.Join(s.root, mergesDir, base))
		if !strings.HasSuffix(base, ".json") {
			candidates = append(candidates, filepath.Join(s.root, mergesDir, base+".json"))
		}
	}
	for _, candidate := range candidates {
		clean := filepath.Clean(candidate)
		rel, err := filepath.Rel(s.root, clean)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
			continue
		}
		if info, err := os.Stat(clean); err == nil && !info.IsDir() {
			return clean, nil
		}
	}
	return "", fmt.Errorf("merge manifest %q: %w", ref, domain.ErrNotFound)
}

// manifestRef renders the data-directory-relative path of a manifest.
func (s *Store) manifestRef(path string) string {
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

// duplicateIDs lists the records a plan absorbs, including the ones already
// absorbed, so a repeated plan names the same set.
func duplicateIDs(plan ports.MergePlan) []string {
	ids := make([]string, 0, len(plan.Duplicates)+len(plan.AlreadyMerged))
	for _, record := range plan.Duplicates {
		ids = append(ids, record.ID)
	}
	for _, record := range plan.AlreadyMerged {
		ids = append(ids, record.ID)
	}
	return ids
}

// applyMergeWork writes every planned file and moves absorbed records to the
// trash. On failure it restores the files it already changed, so the merge
// leaves no half-applied state behind.
func (s *Store) applyMergeWork(work *mergeWork) ([]ports.MergeFile, error) {
	applied := make([]*mergeFileWork, 0, len(work.files))
	for _, file := range work.files {
		if err := writeFileAtomic(file.absPath, file.after); err != nil {
			s.undoMergeWork(applied)
			return mergeFiles(applied), fmt.Errorf("fsstore: merge write %s: %w", file.relPath, err)
		}
		if file.movedTo != "" {
			if err := os.MkdirAll(filepath.Dir(file.moveToAbs), 0o755); err != nil {
				s.undoMergeWork(append(applied, file))
				return mergeFiles(applied), fmt.Errorf("fsstore: create trash directory: %w", err)
			}
			if err := renameFile(file.absPath, file.moveToAbs); err != nil {
				s.undoMergeWork(append(applied, file))
				return mergeFiles(applied), fmt.Errorf("fsstore: move %s to trash: %w", file.relPath, err)
			}
		}
		applied = append(applied, file)
	}
	return mergeFiles(applied), nil
}

// undoMergeWork restores the files a failed apply already changed, newest
// first.
func (s *Store) undoMergeWork(files []*mergeFileWork) {
	for i := len(files) - 1; i >= 0; i-- {
		file := files[i]
		if file.movedTo != "" {
			if err := renameFile(file.moveToAbs, file.absPath); err != nil && !os.IsNotExist(err) {
				continue
			}
		}
		if file.beforeMissing {
			_ = os.Remove(file.absPath)
			continue
		}
		_ = writeFileAtomic(file.absPath, file.before)
	}
}

func mergeFiles(files []*mergeFileWork) []ports.MergeFile {
	out := make([]ports.MergeFile, 0, len(files))
	for _, file := range files {
		out = append(out, ports.MergeFile{Path: file.relPath, Change: file.change})
	}
	return out
}

// mergeRollbackConflict reports why one file cannot be restored, or "" when it
// is safe. A file restored by an interrupted rollback is accepted.
func (s *Store) mergeRollbackConflict(file mergeManifestFile) (string, error) {
	abs, err := s.manifestPath(file.Path)
	if err != nil {
		return "", err
	}
	if file.MovedTo != "" {
		trashAbs, err := s.manifestPath(file.MovedTo)
		if err != nil {
			return "", err
		}
		trashHash, trashExists, err := hashFile(trashAbs)
		if err != nil {
			return "", err
		}
		liveHash, liveExists, err := hashFile(abs)
		if err != nil {
			return "", err
		}
		switch {
		case liveExists && liveHash == file.BeforeSHA256 && trashExists && trashHash == file.AfterSHA256:
			return "", nil
		case !liveExists && trashExists && trashHash == file.AfterSHA256:
			return "", nil
		case !trashExists:
			return file.Path + " is missing from the trash", nil
		case liveExists:
			return file.Path + " was recreated after the merge", nil
		default:
			return file.Path + " changed in the trash after the merge", nil
		}
	}
	hash, exists, err := hashFile(abs)
	if err != nil {
		return "", err
	}
	if file.BeforeMissing {
		switch {
		case !exists, hash == file.AfterSHA256:
			return "", nil
		default:
			return file.Path + " was created and edited after the merge", nil
		}
	}
	if !exists {
		return file.Path + " is missing", nil
	}
	if hash != file.AfterSHA256 {
		return file.Path + " changed after the merge", nil
	}
	return "", nil
}

// restoreMergeFile rewrites one file to the bytes the manifest recorded.
func (s *Store) restoreMergeFile(file mergeManifestFile) error {
	abs, err := s.manifestPath(file.Path)
	if err != nil {
		return err
	}
	if file.BeforeMissing {
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("fsstore: remove %s: %w", file.Path, err)
		}
	} else {
		raw, err := base64.StdEncoding.DecodeString(file.BeforeBase64)
		if err != nil {
			return fmt.Errorf("fsstore: decode recorded content of %s: %w", file.Path, err)
		}
		if err := writeFileAtomic(abs, raw); err != nil {
			return err
		}
	}
	if file.MovedTo != "" {
		trashAbs, err := s.manifestPath(file.MovedTo)
		if err != nil {
			return err
		}
		if err := os.Remove(trashAbs); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("fsstore: remove %s: %w", file.MovedTo, err)
		}
	}
	return nil
}

// manifestPath resolves a recorded relative path inside the data directory.
func (s *Store) manifestPath(rel string) (string, error) {
	clean := filepath.Clean(filepath.Join(s.root, filepath.FromSlash(rel)))
	inside, err := filepath.Rel(s.root, clean)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(os.PathSeparator)) || filepath.IsAbs(inside) {
		return "", domain.NewValidationError("merge.manifest", "path "+rel+" is outside the data directory")
	}
	return clean, nil
}

// hashFile returns the SHA-256 of a file and whether it exists.
func hashFile(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("fsstore: read %s: %w", path, err)
	}
	return hashBytes(raw), true, nil
}

// captureMergeStamp reads the current identity of a merge target. A missing
// file is a valid identity (Exists false), so a plan that creates a file can
// still be verified.
func captureMergeStamp(path string) (mergeFileStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return mergeFileStamp{}, nil
		}
		return mergeFileStamp{}, fmt.Errorf("fsstore: inspect merge target %s: %w", path, err)
	}
	if info.IsDir() {
		return mergeFileStamp{}, domain.NewValidationError("merge.target", path+" is a directory")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return mergeFileStamp{}, nil
		}
		return mergeFileStamp{}, fmt.Errorf("fsstore: read merge target %s: %w", path, err)
	}
	return mergeFileStamp{
		Exists: true, SHA256: hashBytes(raw), Size: int64(len(raw)), ModTime: info.ModTime().UTC(),
	}, nil
}

// sameMergeStamp reports whether two identities name the same unchanged file.
func sameMergeStamp(a, b mergeFileStamp) bool {
	if a.Exists != b.Exists {
		return false
	}
	if !a.Exists {
		return true
	}
	return a.SHA256 == b.SHA256 && a.Size == b.Size && a.ModTime.Equal(b.ModTime)
}

// mergeTargetStamp is one expected on-disk identity and the label used in
// refusal messages.
type mergeTargetStamp struct {
	path  string
	label string
	stamp mergeFileStamp
}

// planMergeStamps records every target right after the plan was built and
// refuses the plan when a file no longer matches the bytes it was planned
// against. A move destination must still be free. The apply re-checks these
// stamps under the per-file lock.
func planMergeStamps(files []*mergeFileWork) ([]mergeTargetStamp, error) {
	stamps := make([]mergeTargetStamp, 0, len(files)*2)
	var stale []string
	for _, file := range files {
		stamp, err := captureMergeStamp(file.absPath)
		if err != nil {
			return nil, err
		}
		switch {
		case file.beforeMissing:
			if stamp.Exists {
				stale = append(stale, file.relPath)
				continue
			}
		case !stamp.Exists, stamp.SHA256 != hashBytes(file.before):
			stale = append(stale, file.relPath)
			continue
		}
		stamps = append(stamps, mergeTargetStamp{path: file.absPath, label: file.relPath, stamp: stamp})
		if file.moveToAbs == "" {
			continue
		}
		destination, err := captureMergeStamp(file.moveToAbs)
		if err != nil {
			return nil, err
		}
		if destination.Exists {
			stale = append(stale, file.movedTo)
			continue
		}
		stamps = append(stamps, mergeTargetStamp{path: file.moveToAbs, label: file.movedTo, stamp: destination})
	}
	if len(stale) > 0 {
		return nil, &StalePlanError{Operation: "merge apply", Paths: stale}
	}
	return stamps, nil
}

// verifyMergeStamps re-reads every target while its lock is held and reports a
// stale plan when sha256, size or mtime differs from the recorded identity.
func (s *Store) verifyMergeStamps(stamps []mergeTargetStamp) error {
	var stale []string
	for _, target := range stamps {
		current, err := captureMergeStamp(target.path)
		if err != nil {
			return err
		}
		if !sameMergeStamp(target.stamp, current) {
			stale = append(stale, target.label)
		}
	}
	if len(stale) > 0 {
		return &StalePlanError{Operation: "merge apply", Paths: stale}
	}
	return nil
}

// lockMergePaths takes the per-file lock of every path, in sorted order, and
// returns one release function. Sorting makes concurrent merges acquire the
// same set in the same order, so they cannot deadlock.
func lockMergePaths(ctx context.Context, paths []string) (func(), error) {
	unique := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		unique = append(unique, path)
	}
	sort.Strings(unique)
	releases := make([]func(), 0, len(unique))
	releaseAll := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, path := range unique {
		release, err := acquireFileLock(ctx, path)
		if err != nil {
			releaseAll()
			return nil, err
		}
		releases = append(releases, release)
	}
	return releaseAll, nil
}

// mergeWorkLockPaths lists every file an apply writes, including trash targets.
func mergeWorkLockPaths(files []*mergeFileWork) []string {
	paths := make([]string, 0, len(files)*2)
	for _, file := range files {
		paths = append(paths, file.absPath)
		if file.moveToAbs != "" {
			paths = append(paths, file.moveToAbs)
		}
	}
	return paths
}

// mergeManifestLockPaths lists every file a rollback restores.
func (s *Store) mergeManifestLockPaths(files []mergeManifestFile) ([]string, error) {
	paths := make([]string, 0, len(files)*2)
	for _, file := range files {
		for _, rel := range []string{file.Path, file.MovedTo} {
			if rel == "" {
				continue
			}
			abs, err := s.manifestPath(rel)
			if err != nil {
				return nil, err
			}
			paths = append(paths, abs)
		}
	}
	return paths, nil
}

// mergeRollbackStampConflict reports a target whose recorded applied identity
// no longer matches the disk. A file already restored by an interrupted
// rollback has a different hash and is left to mergeRollbackConflict.
func (s *Store) mergeRollbackStampConflict(file mergeManifestFile) (string, error) {
	if file.After.SHA256 == "" {
		return "", nil
	}
	rel := file.Path
	if file.MovedTo != "" {
		rel = file.MovedTo
	}
	abs, err := s.manifestPath(rel)
	if err != nil {
		return "", err
	}
	current, err := captureMergeStamp(abs)
	if err != nil {
		return "", err
	}
	if !current.Exists || current.SHA256 != file.After.SHA256 {
		return file.Path + " changed after the merge", nil
	}
	if current.Size != file.After.Size || !current.ModTime.Equal(file.After.ModTime) {
		return file.Path + " changed after the merge", nil
	}
	return "", nil
}

// stampAppliedMergeFiles records the identity of what the merge wrote, so a
// later rollback can prove the files were not edited since.
func (s *Store) stampAppliedMergeFiles(manifest *mergeManifest) {
	for i := range manifest.Files {
		file := &manifest.Files[i]
		rel := file.Path
		if file.MovedTo != "" {
			rel = file.MovedTo
		}
		abs, err := s.manifestPath(rel)
		if err != nil {
			continue
		}
		stamp, err := captureMergeStamp(abs)
		if err != nil {
			continue
		}
		file.After = stamp
	}
}
