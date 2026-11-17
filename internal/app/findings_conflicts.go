package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
)

// Conflict artifacts are written by the merge drivers and by consolidation
// and are never consumed automatically: they are the record of a decision a
// machine could not make alone. Review surfaces them per project and doctor
// counts them for the whole data directory:
//
//	projects/<id>/state-conflicts/*.md      state.md field conflicts
//	projects/<id>/notes/merge-conflicts/*.md notes file conflicts
//	registry-conflicts/*.md                 alias/path claims dropped by a merge
//	merges/*.json                           consolidation merge manifests
//	                                        (status "applied" is finished audit,
//	                                        not a pending conflict)
const (
	stateConflictDir      = "state-conflicts"
	notesConflictDir      = "merge-conflicts"
	registryConflictDir   = "registry-conflicts"
	mergeManifestDir      = "merges"
	maxConflictFileBytes  = 1 << 20
	maxManifestScanFiles  = 500
	maxConflictFileSample = 3
)

// conflictFindings reports the conflict artifacts of one project. It is a
// filesystem scan of already-written files: no git call, no store write, and
// nothing reported when the instance has no data directory (server mode, or a
// configuration without data_dir).
func (a *App) conflictFindings(projectID string) []Finding {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" || a.cfg.Mode == config.ModeServer || strings.TrimSpace(a.cfg.DataDir) == "" {
		return nil
	}
	var findings []Finding
	if files := mergeConflictArtifacts(a.cfg.DataDir, projectID); len(files) > 0 {
		findings = append(findings, newFinding(FindingMergeConflict,
			fmt.Sprintf("%d merge conflict artifacts wait for a decision (%s); the merged file kept one value — compare both sides, fix the record if needed, then delete the artifact",
				len(files), sampleConflictPaths(a.cfg.DataDir, files)),
			files[0]))
	}
	if files := registryConflictArtifacts(a.cfg.DataDir, projectID); len(files) > 0 {
		findings = append(findings, newFinding(FindingRegistryConflict,
			fmt.Sprintf("%d registry conflict artifacts name this project (%s); an alias or checkout path was claimed twice and one owner was kept",
				len(files), sampleConflictPaths(a.cfg.DataDir, files)),
			files[0]))
	}
	return findings
}

// mergeConflictArtifacts lists this project's state/notes conflict files and
// the consolidation manifests recorded for it, newest first. A manifest that
// records a finished merge (status "applied") is audit, not a pending conflict.
func mergeConflictArtifacts(dataDir, projectID string) []string {
	var files []string
	for _, dir := range []string{
		filepath.Join(dataDir, "projects", projectID, stateConflictDir),
		filepath.Join(dataDir, "projects", projectID, "notes", notesConflictDir),
	} {
		files = append(files, globFiles(dir, ".md")...)
	}
	for _, path := range pendingMergeManifests(dataDir) {
		if manifestNamesProject(path, projectID) {
			files = append(files, path)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	return files
}

// pendingMergeManifests lists the merges/*.json manifests that still wait for a
// decision. A manifest with status "applied" records a consolidation that
// already happened; review and doctor must not count it as pending. A manifest
// whose status cannot be read counts as pending, so a real artifact is never
// hidden.
func pendingMergeManifests(dataDir string) []string {
	manifests := globFiles(filepath.Join(dataDir, mergeManifestDir), ".json")
	if len(manifests) > maxManifestScanFiles {
		manifests = manifests[:maxManifestScanFiles]
	}
	pending := make([]string, 0, len(manifests))
	for _, path := range manifests {
		if !mergeManifestApplied(path) {
			pending = append(pending, path)
		}
	}
	return pending
}

// mergeManifestApplied reports whether a merge manifest records a finished
// consolidation. Only the head of the file is read: status is written near the
// top and a manifest embeds the touched files.
func mergeManifestApplied(path string) bool {
	raw, err := readFileHead(path, maxConflictFileBytes)
	if err != nil {
		return false
	}
	match := mergeManifestStatusPattern.FindSubmatch(raw)
	return len(match) == 2 && string(match[1]) == "applied"
}

var mergeManifestStatusPattern = regexp.MustCompile(`"status"\s*:\s*"([^"]*)"`)

// registryConflictArtifacts lists registry conflict files whose kept or
// incoming side names this project, newest first.
func registryConflictArtifacts(dataDir, projectID string) []string {
	files := globFiles(filepath.Join(dataDir, registryConflictDir), ".md")
	matched := make([]string, 0, len(files))
	for _, path := range files {
		if conflictNamesProject(path, projectID) {
			matched = append(matched, path)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matched)))
	return matched
}

func globFiles(dir, ext string) []string {
	files, err := filepath.Glob(filepath.Join(dir, "*"+ext))
	if err != nil {
		return nil
	}
	sort.Strings(files)
	return files
}

// manifestNamesProject reports whether a merges/*.json manifest was recorded
// for projectID. Only the head of the file is read: project_id is written
// near the top and a manifest may embed large record bodies.
func manifestNamesProject(path, projectID string) bool {
	raw, err := readFileHead(path, maxConflictFileBytes)
	if err != nil {
		return false
	}
	return manifestProjectPattern(projectID).Match(raw)
}

func manifestProjectPattern(projectID string) *regexp.Regexp {
	return regexp.MustCompile(`"project_id"\s*:\s*"` + regexp.QuoteMeta(projectID) + `"`)
}

// conflictNamesProject reports whether a registry conflict file kept or
// dropped this project id. Conflict values are bare ids (or JSON records that
// contain one), so an id token search is enough.
func conflictNamesProject(path, projectID string) bool {
	raw, err := readFileHead(path, maxConflictFileBytes)
	if err != nil {
		return false
	}
	pattern := regexp.MustCompile(`(^|[^a-z0-9_-])` + regexp.QuoteMeta(strings.ToLower(projectID)) + `([^a-z0-9_-]|$)`)
	return pattern.MatchString(strings.ToLower(string(raw)))
}

// readFileHead reads at most limit bytes of a file. A larger file is rejected
// instead of truncated, so a caller never parses half a document.
func readFileHead(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("conflict artifact %s exceeds %d bytes", path, limit)
	}
	return os.ReadFile(path)
}

// sampleConflictPaths renders at most maxConflictFileSample artifact paths
// for a finding message, data-directory relative when possible.
func sampleConflictPaths(dataDir string, files []string) string {
	shown := files
	if len(shown) > maxConflictFileSample {
		shown = shown[:maxConflictFileSample]
	}
	rel := make([]string, 0, len(shown))
	for _, path := range shown {
		if relative, err := filepath.Rel(dataDir, path); err == nil && !strings.HasPrefix(relative, "..") {
			path = filepath.ToSlash(relative)
		}
		rel = append(rel, path)
	}
	return strings.Join(rel, ", ")
}
