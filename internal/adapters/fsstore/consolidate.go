package fsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// backupsDir holds machine-local consolidation manifests under the data
	// directory. It is excluded from the sync revision (see
	// isExcludedRootDir): a rollback book does not change the knowledge.
	backupsDir = "backups"
	// consolidationManifestVersion is bumped when the manifest layout changes.
	consolidationManifestVersion = 1
)

var _ ports.ConsolidationRecorder = (*Store)(nil)

// SaveConsolidationManifest writes a new manifest under
// backups/consolidate-<ts>/manifest.json and returns the data-directory
// relative reference. Each run gets its own directory, so a rollback input is
// unambiguous.
func (s *Store) SaveConsolidationManifest(ctx context.Context, manifest ports.ConsolidationManifest) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	manifest.Version = consolidationManifestVersion
	if manifest.Status == "" {
		manifest.Status = ports.ConsolidationApplied
	}
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = s.nowUTC()
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("fsstore: encode consolidation manifest: %w", err)
	}
	dir := filepath.Join(s.root, backupsDir, "consolidate-"+manifest.CreatedAt.UTC().Format("20060102T150405.000000000Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("fsstore: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "manifest.json")
	if err := writeFileAtomic(path, data); err != nil {
		return "", err
	}
	return s.manifestRef(path), nil
}

// LoadConsolidationManifest reads one consolidation manifest by reference. A
// bare name is looked up under backups/ (with or without manifest.json).
func (s *Store) LoadConsolidationManifest(ctx context.Context, ref string) (ports.ConsolidationManifest, error) {
	if err := contextError(ctx); err != nil {
		return ports.ConsolidationManifest{}, err
	}
	path, err := s.resolveConsolidationManifest(ref)
	if err != nil {
		return ports.ConsolidationManifest{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ports.ConsolidationManifest{}, fmt.Errorf("fsstore: read consolidation manifest %s: %w", path, err)
	}
	var manifest ports.ConsolidationManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return ports.ConsolidationManifest{}, fmt.Errorf("fsstore: parse consolidation manifest %s: %w", path, err)
	}
	if manifest.Version > consolidationManifestVersion {
		return ports.ConsolidationManifest{}, domain.NewValidationError("consolidation.manifest",
			fmt.Sprintf("%s: version %d requires a newer engine", path, manifest.Version))
	}
	return manifest, nil
}

// MarkConsolidationRolledBack stamps a manifest as rolled back.
func (s *Store) MarkConsolidationRolledBack(ctx context.Context, ref string, at time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	path, err := s.resolveConsolidationManifest(ref)
	if err != nil {
		return err
	}
	manifest, err := s.LoadConsolidationManifest(ctx, path)
	if err != nil {
		return err
	}
	manifest.Status = ports.ConsolidationRolledBack
	manifest.RolledBackAt = at.UTC()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("fsstore: encode consolidation manifest: %w", err)
	}
	return writeFileAtomic(path, data)
}

// resolveConsolidationManifest maps a user-provided reference to a manifest
// path inside the data directory. A bare name is looked up under backups/.
func (s *Store) resolveConsolidationManifest(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", domain.NewValidationError("consolidation.manifest", "is required")
	}
	var candidates []string
	if filepath.IsAbs(ref) {
		candidates = append(candidates, ref)
	} else {
		candidates = append(candidates, filepath.Join(s.root, filepath.FromSlash(ref)))
		base := filepath.Base(ref)
		candidates = append(candidates, filepath.Join(s.root, backupsDir, base))
		if !strings.HasSuffix(base, ".json") {
			candidates = append(candidates, filepath.Join(s.root, backupsDir, base, "manifest.json"))
		}
		if base != "manifest.json" {
			candidates = append(candidates, filepath.Join(s.root, backupsDir, base+".json"))
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
	return "", fmt.Errorf("consolidation manifest %q: %w", ref, domain.ErrNotFound)
}
