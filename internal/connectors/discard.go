package connectors

import "context"

// DiscardSink accepts connector dumps without persisting them. Server mode
// stores the run metadata in PostgreSQL; the Markdown dumps are not part of the
// server-side v1 surface.
type DiscardSink struct{}

var _ MarkdownSink = DiscardSink{}

// Write drops one connector dump.
func (DiscardSink) Write(context.Context, string, []byte) error {
	return nil
}
