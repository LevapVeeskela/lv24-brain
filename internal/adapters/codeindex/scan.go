package codeindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// skippedDirs are never part of a Go module's build. dot- and
// underscore-prefixed directories are skipped by the go tool as well.
var skippedDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"vendor": true, "node_modules": true, "testdata": true,
	"venv": true, "__pycache__": true, "site-packages": true,
}

// fileState is one indexed source file.
type fileState struct {
	Path     string
	SHA256   string
	Size     int64
	MTimeNS  int64
	Package  string
	Language string
}

// scanResult is the file set of one checkout scan.
type scanResult struct {
	Files   map[string]fileState
	Skipped int
	Notes   []string
}

// scan walks the checkout and hashes every Go, TypeScript, TSX, JavaScript,
// JSX and Python file that belongs to the platform build. known supplies the
// previous size and mtime so an unchanged file is not read again; a size or
// mtime difference forces a hash.
func (i *Index) scan(ctx context.Context, known map[string]fileState) (scanResult, error) {
	result := scanResult{Files: map[string]fileState{}}
	buildCtx := build.Default
	buildCtx.BuildTags = append([]string(nil), i.tags...)
	moduleRoot := moduleDir(i.root)
	root := i.root

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				result.Skipped++
				return fs.SkipDir
			}
			result.Skipped++
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if path == root {
				return nil
			}
			// A nested Git checkout is a different repository with its own
			// index and its own file set; following it would mix two
			// checkouts into one path-keyed store.
			if isNestedRepo(path, root) {
				return fs.SkipDir
			}
			if skippedDirs[name] || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			// The Go tool ignores underscore-prefixed directories, but a Python
			// package may legitimately use that spelling; keep it when it is a
			// package, not a build directory.
			if strings.HasPrefix(name, "_") && !pythonPackageDir(path) {
				return fs.SkipDir
			}
			if rel, ok := i.relPath(path); ok && i.excluded(rel) {
				return fs.SkipDir
			}
			if isNestedModule(path, moduleRoot) {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		language := LanguageForPath(name)
		if language == "" {
			return nil
		}
		// The Go tool ignores underscore-prefixed files; a TypeScript or Python
		// module with that spelling is still importable and is indexed.
		if strings.HasPrefix(name, "_") && language == Language {
			return nil
		}
		rel, ok := i.relPath(path)
		if !ok || i.excluded(rel) {
			return nil
		}
		if language == Language {
			matched, matchErr := buildCtx.MatchFile(filepath.Dir(path), name)
			if matchErr != nil || !matched {
				return nil
			}
		} else if !polyglotPathIndexable(rel) {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			result.Skipped++
			return nil
		}
		if info.Size() > maxSourceFileBytes {
			result.Skipped++
			return nil
		}
		state := fileState{Path: rel, Size: info.Size(), MTimeNS: info.ModTime().UnixNano(), Language: language}
		if previous, ok := known[rel]; ok && previous.Size == state.Size && previous.MTimeNS == state.MTimeNS && previous.Language == state.Language {
			state.SHA256 = previous.SHA256
			state.Package = previous.Package
			result.Files[rel] = state
			return nil
		}
		sum, readErr := hashFile(path)
		if readErr != nil {
			result.Skipped++
			return nil
		}
		state.SHA256 = sum
		result.Files[rel] = state
		return nil
	})
	if err != nil {
		return scanResult{}, fmt.Errorf("codeindex: scan %s: %w", root, err)
	}
	if result.Skipped > 0 {
		result.Notes = append(result.Notes, fmt.Sprintf("%d file(s) skipped: oversize, unreadable or build-constraint excluded", result.Skipped))
	}
	return result, nil
}

// pythonPackageDir reports whether a directory is an importable Python package,
// which keeps an underscore-prefixed package in the walk.
func pythonPackageDir(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "__init__.py"))
	return err == nil
}

// excluded reports whether a repository path matches an exclude prefix.
func (i *Index) excluded(rel string) bool {
	for _, prefix := range i.exclude {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}

// hashFile returns the SHA-256 of one file.
func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// moduleDir returns the directory of the nearest go.mod at or above root.
func moduleDir(root string) string {
	dir := filepath.Clean(root)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// isNestedModule reports whether dir starts another Go module below the
// module root. Its packages are not part of ./... and indexing them would
// produce an inconsistent file-to-package map.
func isNestedModule(dir, moduleRoot string) bool {
	if moduleRoot == "" || filepath.Clean(dir) == filepath.Clean(moduleRoot) {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return true
	}
	return false
}

// isNestedRepo reports whether dir is a separate Git checkout below the
// indexed root. One store describes one checkout, so a nested clone or linked
// worktree is not walked.
func isNestedRepo(dir, root string) bool {
	if filepath.Clean(dir) == filepath.Clean(root) {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return true
	}
	return false
}

// packageDirs returns the sorted distinct directories of the given paths.
func packageDirs(paths []string) []string {
	seen := map[string]bool{}
	dirs := make([]string, 0, len(paths))
	for _, path := range paths {
		dir := filepath.ToSlash(filepath.Dir(path))
		if dir == "." {
			dir = ""
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// dirPattern renders a package directory as a package pattern relative to the
// checkout root.
func dirPattern(dir string) string {
	if dir == "" {
		return "."
	}
	return "./" + dir
}

// pathMatchesAny reports whether path equals or is below one of the prefixes.
func pathMatchesAny(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// normalizeFilter normalizes and cleans a Sync path filter.
func normalizeFilter(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, value := range paths {
		value = normalizeRepoPath(value)
		value = strings.TrimSuffix(value, "/")
		if value == "" || value == "." {
			continue
		}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
