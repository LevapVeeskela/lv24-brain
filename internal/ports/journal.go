package ports

import (
	"context"
	"time"
)

// JournalEntry is one append-only line of a per-user activity journal
// (journal/<user>.jsonl in local and git modes). Each user writes only their
// own file, so concurrent git-mode writers never conflict on it.
type JournalEntry struct {
	Time    time.Time
	Event   string
	User    string
	Project string
	// ID and Title identify the record an event is about, when any.
	ID    string
	Title string
	// Fields carries event-specific values (for a commit: branch, head,
	// subject, remote). Keys must not collide with the fields above.
	Fields map[string]string
}

// JournalQuery filters a journal read. Zero values match everything; Limit
// <= 0 uses the adapter default.
type JournalQuery struct {
	User    string
	Project string
	Event   string
	Since   time.Time
	Limit   int
}

// Journal appends and reads per-user activity journals.
type Journal interface {
	Append(ctx context.Context, entry JournalEntry) error
	// Read returns matching entries from every journal file, newest first.
	Read(ctx context.Context, query JournalQuery) ([]JournalEntry, error)
}
