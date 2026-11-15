package ports

import (
	"context"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// TopicLocker is implemented by stores that can serialize a topic upsert per
// (project, kind, topic key). The returned release function must always be
// called. A store without it still supports append-only writes.
type TopicLocker interface {
	LockTopic(ctx context.Context, projectID, kind, topicKey string) (func(), error)
}

// TopicReplacer is implemented by stores that can rewrite one record in place
// while keeping its ID, creation time and provenance. The caller supplies the
// new text and the bounded topic history; the store never invents history.
type TopicReplacer interface {
	ReplaceNote(ctx context.Context, note domain.Note) (domain.Note, error)
	ReplaceDecision(ctx context.Context, decision domain.Decision) (domain.Decision, error)
	ReplaceAttempt(ctx context.Context, attempt domain.Attempt) (domain.Attempt, error)
	ReplaceHandoff(ctx context.Context, handoff domain.Handoff) (domain.Handoff, error)
}

// PromptStore keeps the opt-in, machine-local prompt zone. Prompts are never
// curated records: they are excluded from the git tree and from the search
// index.
type PromptStore interface {
	// AppendPrompt writes one prompt to projects/<id>/prompts/<date>.md.
	AppendPrompt(ctx context.Context, prompt domain.Prompt) error
	// ListPrompts returns a project's prompts newest first, bounded by limit.
	// A limit of zero selects the adapter default.
	ListPrompts(ctx context.Context, projectID string, limit int) ([]domain.Prompt, error)
}
