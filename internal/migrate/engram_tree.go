package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hashTargetTree hashes the curated records under <target>/projects only. The
// search index, backups, journal and connector dumps are deliberately outside
// the fingerprint: a dry run must not read or touch them, and the index rewrites
// itself after every run. A dry run reports the same value before and after and
// fails if anything was written.
func hashTargetTree(target string) (string, error) {
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyTreeHash(), nil
		}
		return "", fmt.Errorf("migrate: inspect target %s: %w", target, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("migrate: target %s is not a directory", target)
	}
	root := filepath.Join(target, projectsDir)
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return emptyTreeHash(), nil
		}
		return "", fmt.Errorf("migrate: inspect target %s: %w", root, err)
	}
	type file struct {
		rel  string
		full string
	}
	files := []file{}
	err = walkRegularFiles(root, func(rel, full string, entry os.DirEntry) error {
		base := filepath.Base(full)
		if strings.HasPrefix(base, ".tmp-") || strings.HasSuffix(base, ".lock") {
			return nil
		}
		files = append(files, file{rel: filepath.ToSlash(filepath.Join(projectsDir, filepath.FromSlash(rel))), full: full})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("migrate: scan target %s: %w", root, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	hash := sha256.New()
	for _, file := range files {
		raw, err := os.ReadFile(file.full)
		if err != nil {
			return "", fmt.Errorf("migrate: hash target file %s: %w", file.rel, err)
		}
		hash.Write([]byte(file.rel))
		hash.Write([]byte{0})
		hash.Write(raw)
		hash.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func emptyTreeHash() string {
	sum := sha256.Sum256(nil)
	return "sha256:" + hex.EncodeToString(sum[:])
}
