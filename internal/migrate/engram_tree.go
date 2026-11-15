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

// hashTargetTree hashes every curated file under target, excluding migration
// backups and git internals. A dry run reports the same value before and after
// and fails if anything was written.
func hashTargetTree(target string) (string, error) {
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			sum := sha256.Sum256(nil)
			return "sha256:" + hex.EncodeToString(sum[:]), nil
		}
		return "", fmt.Errorf("migrate: inspect target %s: %w", target, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("migrate: target %s is not a directory", target)
	}
	type file struct {
		rel  string
		full string
	}
	files := []file{}
	err = walkRegularFiles(target, func(rel, full string, entry os.DirEntry) error {
		base := filepath.Base(full)
		if strings.HasPrefix(base, ".tmp-") || strings.HasSuffix(base, ".lock") {
			return nil
		}
		files = append(files, file{rel: rel, full: full})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("migrate: scan target %s: %w", target, err)
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
