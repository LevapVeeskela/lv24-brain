package gitstore

import (
	"context"
	"fmt"

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
// prevents future publication.
func (p *Provider) PrepareConnectionPublish(ctx context.Context, id string, policy domain.PublishPolicy) error {
	if err := domain.ValidateConnectionID("connection.id", id); err != nil {
		return err
	}
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
