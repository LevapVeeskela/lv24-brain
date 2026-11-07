package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// indexDirtyMarker (under the data directory, next to the gitignored index)
// records that the search projection may miss records: a write could not
// update it, or a sync pulled records written elsewhere. The next search
// reindexes and a successful Reindex removes the marker.
const indexDirtyMarker = "index/dirty"

func (a *App) indexDirtyPath() string {
	if a.cfg.DataDir == "" || a.deps.Index == nil {
		return ""
	}
	return filepath.Join(a.cfg.DataDir, filepath.FromSlash(indexDirtyMarker))
}

// markIndexDirty is best effort: the marker only speeds up recovery; doctor
// and `brain index` still repair the index without it.
func (a *App) markIndexDirty() {
	path := a.indexDirtyPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(a.now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

func (a *App) indexDirty() bool {
	path := a.indexDirtyPath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (a *App) clearIndexDirty() {
	if path := a.indexDirtyPath(); path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
}

// repairIndexIfDirty reindexes when the marker is set. A failed reindex
// keeps the marker so a later run retries.
func (a *App) repairIndexIfDirty(ctx context.Context) error {
	if !a.indexDirty() {
		return nil
	}
	_, err := a.Reindex(ctx, IndexOptions{})
	return err
}
