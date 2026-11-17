package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// syncPreflight makes a clone safe to sync before any pull. It repairs the two
// setup gaps that audit found on second machines: .gitattributes that predates
// the deterministic drivers, and merge drivers registered only on the first
// clone. Both repairs are idempotent and land in the same sync's commit.
//
// A data directory nested inside a larger git work tree cannot be made safe
// (the drivers would apply to unrelated files), so it is reported as an error
// instead of silently doing nothing.
func (a *App) syncPreflight(ctx context.Context) error {
	if a.cfg.Mode != config.ModeGit {
		return nil
	}
	if strings.TrimSpace(a.cfg.DataDir) == "" {
		return domain.NewValidationError("config.data_dir", "is required in git mode")
	}
	if verifier, ok := a.deps.Sync.(ports.GitLayoutVerifier); ok {
		if err := verifier.VerifySyncLayout(ctx); err != nil {
			return err
		}
	}
	if err := ensureGitattributes(a.cfg.DataDir); err != nil {
		return err
	}
	if err := ensureGitignore(a.cfg.DataDir); err != nil {
		return err
	}
	missingAttributes, err := missingGitattributesLines(a.cfg.DataDir)
	if err != nil {
		return err
	}
	installer := a.deps.MergeDrivers
	if installer == nil {
		if len(missingAttributes) > 0 {
			return fmt.Errorf("brain sync: .gitattributes is incomplete and merge driver registration is unavailable; run brain doctor --fix")
		}
		return nil
	}
	missing, err := installer.MissingMergeDrivers(ctx)
	if errors.Is(err, domain.ErrUnsupported) {
		// Not a git repository yet: there is nowhere to register drivers and
		// no pull to protect. init/onboarding registers them later.
		return nil
	}
	if err != nil {
		return err
	}
	if len(missing) == 0 && len(missingAttributes) == 0 {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("brain sync: resolve the executable for merge drivers: %w", err)
	}
	if _, err := installer.RegisterMergeDrivers(ctx, executable); err != nil {
		return err
	}
	return nil
}

// commitConflictRecords stages and commits driver-generated conflict records in
// the same sync that produced them, then reports their paths so they appear in
// `sync --status --json`. Providers without the capability are a no-op.
func (a *App) commitConflictRecords(ctx context.Context, result *SyncResult, message string) error {
	committer, ok := a.deps.Sync.(ports.SyncConflictCommitter)
	if !ok {
		return nil
	}
	paths, changed, err := committer.CommitConflictRecords(ctx, message)
	if len(paths) > 0 {
		result.ConflictRecords = appendUnique(result.ConflictRecords, paths...)
	}
	if err != nil {
		return err
	}
	if changed {
		result.Changed++
	}
	return nil
}

func appendUnique(base []string, values ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(values))
	for _, value := range base {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		base = append(base, value)
	}
	return base
}
