package gitstore

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// conflictDirs are the directories a deterministic merge driver writes its
// records into. A record left in one of them is real preserved knowledge, so it
// must be committed in the same sync that produced it and reported by status.
var conflictDirs = map[string]struct{}{
	"state-conflicts":    {},
	"registry-conflicts": {},
	"merge-conflicts":    {},
}

// conflictRecordPaths lists every file a merge driver wrote under a conflict
// directory in the working tree, as repository-relative slash paths. Ignored
// and git-internal directories are skipped. The scan is bounded to these named
// directories so an ordinary data tree is cheap to walk.
func (p *Provider) conflictRecordPaths(ctx context.Context) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(p.dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != p.dir && (name == ".git" || name == "index" || name == "backups" ||
				name == "secrets" || strings.HasPrefix(name, ".tmp-")) {
				return filepath.SkipDir
			}
			return nil
		}
		parent := filepath.Base(filepath.Dir(path))
		_, inConflictDir := conflictDirs[parent]
		// The items driver keeps a genuinely divergent incoming item under a
		// deterministic <name>-<12hex>.md sibling. It is driver output too and
		// must be committed and surfaced like any other conflict record.
		inItemsDir := parent == "items" && isDivergentItemSibling(entry.Name())
		if !inConflictDir && !inItemsDir {
			return nil
		}
		rel, err := filepath.Rel(p.dir, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitstore: scan conflict records: %w", err)
	}
	return uniquePaths(paths), nil
}

// isDivergentItemSibling reports whether name is an items-driver sibling:
// <canonical-stem>-<12 lowercase hex>.md. Real item files are named by a bare
// 40-hex hash, so this pattern cannot capture an ordinary stored item.
func isDivergentItemSibling(name string) bool {
	if !strings.HasSuffix(name, ".md") {
		return false
	}
	stem := strings.TrimSuffix(name, ".md")
	index := strings.LastIndex(stem, "-")
	if index < 0 {
		return false
	}
	suffix := stem[index+1:]
	if len(suffix) != 12 {
		return false
	}
	for _, r := range suffix {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// CommitConflictRecords stages and commits the driver-generated conflict
// records left in the working tree. Only those paths are staged: a concurrent
// writer's unrelated changes must never be folded into this commit. When no
// record is new (or all are already committed) it is a no-op.
func (p *Provider) CommitConflictRecords(ctx context.Context, message string) ([]string, bool, error) {
	paths, err := p.conflictRecordPaths(ctx)
	if err != nil || len(paths) == 0 {
		return paths, false, err
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return paths, false, domain.NewValidationError("gitstore.message", "is required")
	}
	addArgs := append([]string{"add", "--"}, paths...)
	if _, err := p.run(ctx, addArgs...); err != nil {
		return paths, false, fmt.Errorf("gitstore: stage conflict records: %w", err)
	}
	stagedArgs := append([]string{"diff", "--cached", "--name-only", "-z", "--"}, paths...)
	staged, err := p.run(ctx, stagedArgs...)
	if err != nil {
		return paths, false, err
	}
	if len(splitNul(staged)) == 0 {
		return paths, false, nil
	}
	if _, err := p.run(ctx, "commit", "--message="+message); err != nil {
		return paths, false, fmt.Errorf("gitstore: commit conflict records: %w", err)
	}
	return paths, true, nil
}

// VerifySyncLayout reports a data directory that is nested inside a larger git
// work tree. Drivers and .gitattributes apply to the whole repository, so a
// nested data directory would apply brain merge rules to unrelated files and
// can never be made safe by register-missing-drivers. A data directory that is
// not in a git repository yet is fine (nothing to register); the work tree root
// itself is fine.
func (p *Provider) VerifySyncLayout(ctx context.Context) error {
	root, err := p.workTreeRoot(ctx)
	if err != nil {
		return err
	}
	if root == "" {
		return nil
	}
	if !samePath(root, p.dir) {
		return fmt.Errorf("gitstore: data directory %s is nested in git work tree %s; brain sync needs the data directory to be the repository root (move .git there or point data_dir at the work tree root)", p.dir, root)
	}
	return nil
}
