package gitstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

var _ ports.GitPathLister = (*Provider)(nil)

// PrepareConnectionPublish removes a connection's machine-local raw files from
// the git index before ingestion writes payloads: the whole directory for a
// local-only connection, only checkpoints/ for a connection that publishes its
// items. Checkpoint bytes are host cursors, so they never belong to the shared
// tree, including data published by an earlier engine.
//
// Files remain on disk. Retrying is harmless, including after a failed run or a
// previous policy change. Previously published history is retained; this
// prevents future publication. The git index mutation is serialized with the
// checkout sync lock so it cannot interleave with sync/index operations.
func (p *Provider) PrepareConnectionPublish(ctx context.Context, id string, policy domain.PublishPolicy) error {
	if err := domain.ValidateConnectionID("connection.id", id); err != nil {
		return err
	}
	release, err := p.lockSync(ctx)
	if err != nil {
		return err
	}
	defer release()
	if policy == domain.PublishLocalOnly {
		if _, err := p.run(ctx, "rm", "--cached", "-r", "--ignore-unmatch", "--", "connectors/"+id+"/"); err != nil {
			return fmt.Errorf("untrack local-only connection: %w", err)
		}
		return nil
	}
	if _, err := p.run(ctx, "rm", "--cached", "-r", "--ignore-unmatch", "--", "connectors/"+id+"/checkpoints/"); err != nil {
		return fmt.Errorf("untrack checkpoints: %w", err)
	}
	return nil
}

func (p *Provider) lockSync(ctx context.Context) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := filepath.Join(p.dir, "sync.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("gitstore: create sync lock dir: %w", err)
	}
	const wait = 10 * time.Second
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			released := false
			return func() {
				if released {
					return
				}
				released = true
				_ = os.Remove(path)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("gitstore: acquire sync lock: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("gitstore: acquire sync lock: timed out waiting for %s", path)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("gitstore: acquire sync lock: %w", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// TrackedPaths lists the files under prefix that the repository index tracks
// (git ls-files). It reads the index only, never the working tree or a remote,
// and reports an empty list when the prefix has no tracked files.
func (p *Provider) TrackedPaths(ctx context.Context, prefix string) ([]string, error) {
	args := []string{"ls-files", "-z"}
	if prefix != "" {
		args = append(args, "--", prefix)
	}
	out, err := p.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return splitNul(out), nil
}

// TrackedRawZonePaths lists the items and checkpoints of a connection that the
// repository index tracks.
func (p *Provider) TrackedRawZonePaths(ctx context.Context, id string) ([]string, error) {
	if err := domain.ValidateConnectionID("connection.id", id); err != nil {
		return nil, err
	}
	paths, err := p.TrackedPaths(ctx, "connectors/"+id)
	if err != nil {
		return nil, err
	}
	raw := make([]string, 0, len(paths))
	for _, path := range paths {
		if rawZoneFilePath(path) {
			raw = append(raw, path)
		}
	}
	return raw, nil
}

// RawZoneTracked reports whether the index tracks items or checkpoints of the
// connection. The ingestion host uses it to grandfather a raw zone that an
// earlier engine published: a true answer with no explicit publish policy keeps
// the connection shared instead of untracking it.
func (p *Provider) RawZoneTracked(ctx context.Context, id string) (bool, error) {
	paths, err := p.TrackedRawZonePaths(ctx, id)
	if err != nil {
		return false, err
	}
	return len(paths) > 0, nil
}

// rawZoneFilePath reports whether a repository path is an item or checkpoint
// file of a connection (connectors/<id>/items/... or .../checkpoints/...).
func rawZoneFilePath(path string) bool {
	slash := filepath.ToSlash(path)
	return strings.Contains(slash, "/items/") || strings.Contains(slash, "/checkpoints/")
}
