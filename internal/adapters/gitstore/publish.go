package gitstore

import (
	"context"
	"fmt"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// PrepareConnectionPublish removes a local-only connection from the git index
// before ingestion writes private payloads. Files remain on disk. Retrying is
// harmless, including after a failed run or a previous policy change.
// Previously published history is retained; this prevents future publication.
func (p *Provider) PrepareConnectionPublish(ctx context.Context, id string, policy domain.PublishPolicy) error {
	if err := domain.ValidateConnectionID("connection.id", id); err != nil {
		return err
	}
	if policy != domain.PublishLocalOnly {
		return nil
	}
	_, err := p.run(ctx, "rm", "--cached", "-r", "--ignore-unmatch", "--", "connectors/"+id+"/")
	if err != nil {
		return fmt.Errorf("untrack local-only connection: %w", err)
	}
	return nil
}
