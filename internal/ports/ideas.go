package ports

import (
	"context"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
)

// RecordIdea is the record kind of an idea. Ideas are deliberately not part of
// recordKinds or RecordEditor: their trash and restore have dedicated verbs
// (`brain idea rm|restore`), so the generic record commands never move one.
const RecordIdea = "idea"

// IdeaFilter narrows ListIdeas.
//
// ProjectID empty lists every registered project. Statuses empty lists all
// five statuses. Tags keeps only ideas that carry every listed tag
// (case-insensitive exact match; empty keeps all). DueAfter is an inclusive
// lower bound and DueBefore an exclusive upper bound; an undated idea is
// excluded when either is set. Query is a case-insensitive substring over
// title, body and tags. Limit <= 0 means no store limit; the caller caps. The
// store applies Tags and every other predicate before Limit.
type IdeaFilter struct {
	ProjectID string
	Statuses  []domain.IdeaStatus
	Tags      []string
	DueBefore time.Time
	DueAfter  time.Time
	Query     string
	Limit     int
}

// IdeaStore is the optional persistence capability for ideas. It is separate
// from Store so server clients keep satisfying Store; the app type-asserts it
// and reports ErrUnsupported for a store without ideas.
//
// Project resolution (id/alias/path, cwd default, the personal fallback) lives
// in internal/app. A store only accepts a concrete registered project id and
// never invents or registers a project.
type IdeaStore interface {
	// AppendIdea writes one new idea file. It sets created_at/updated_at when
	// zero, defaults status to open and priority to normal, normalizes
	// provenance, allocates an id with domain.NewIdeaID(createdAt, title) when
	// empty, and refuses a non-empty id that is not IsRecordID("idea", id) or
	// an existing id (domain.ErrTargetExists). It never resolves a project
	// selector, registers a project or falls back to personal: ProjectID is an
	// already resolved, concrete registered id.
	AppendIdea(ctx context.Context, idea domain.Idea) (domain.SyncRevision, error)
	// ListIdeas scans the project idea directories and returns a deterministic
	// order: open, doing, someday, done, dropped; dated before undated; due
	// ascending; high, normal, low; updated_at descending; id ascending.
	// Unparsable files are skipped and reported by ProblemReporter.
	ListIdeas(ctx context.Context, filter IdeaFilter) ([]domain.Idea, error)
	// GetIdea finds an idea by its globally unique id across registered
	// projects. A problem file returns its parse error instead of a zero idea.
	GetIdea(ctx context.Context, id string) (domain.Idea, error)
	// UpdateIdea rewrites the stored idea in place, preserving created_at and
	// the id, and moves the file when ProjectID changes. A known problem file
	// is never overwritten.
	UpdateIdea(ctx context.Context, idea domain.Idea) error
	// TrashIdea moves a live idea to projects/<project>/trash/ideas/.
	TrashIdea(ctx context.Context, projectID, id string) error
	// RestoreIdea moves a trashed idea back. ErrNotFound when the trash has no
	// such idea; ErrConflict when a live file already uses the id.
	RestoreIdea(ctx context.Context, projectID, id string) error
}
