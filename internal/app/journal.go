package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/levapveeskela/lv24-brain/internal/config"
	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

// Journal event names written by the engine.
const (
	JournalNote     = "note"
	JournalDecision = "decision"
	JournalAttempt  = "attempt"
	JournalHandoff  = "handoff"
	JournalCommit   = "commit"
)

// JournalUser is the identity journal lines are attributed to: brain.yaml
// `user`, else the OS account name (the legacy tool used USERNAME too).
func (a *App) JournalUser() string {
	for _, candidate := range []string{a.cfg.User, os.Getenv("USERNAME"), os.Getenv("USER")} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return "unknown"
}

// journalWrite appends an activity line for a record write. Server mode has
// no journal (the server keeps revisions and audit instead), so a nil journal
// is not an error.
func (a *App) journalWrite(ctx context.Context, event, projectID, id, title string) error {
	if a.deps.Journal == nil {
		return nil
	}
	return a.deps.Journal.Append(ctx, ports.JournalEntry{
		Time:    a.now(),
		Event:   event,
		User:    a.JournalUser(),
		Project: projectID,
		ID:      id,
		Title:   title,
	})
}

// afterWrite runs the post-write steps shared by every record kind: journal
// first, then the search index. The record is already stored, so failures
// are reported as "written but ..." rather than as a failed write.
func (a *App) afterWrite(ctx context.Context, event, projectID, id, title string, doc ports.Document) error {
	journalErr := a.journalWrite(ctx, event, projectID, id, title)
	if err := a.upsertIndex(ctx, doc); err != nil {
		return fmt.Errorf("%s %s was written but the search index update failed: %w", event, id, err)
	}
	if journalErr != nil {
		return fmt.Errorf("%s %s was written but the journal append failed: %w", event, id, journalErr)
	}
	return nil
}

// CommitInput describes a git commit to record in the journal, typically
// from a post-commit hook.
type CommitInput struct {
	ProjectID string
	Branch    string
	Head      string
	Subject   string
	Remote    string
}

// RecordCommit appends a commit event to the current user's journal.
func (a *App) RecordCommit(ctx context.Context, input CommitInput) (ports.JournalEntry, error) {
	if err := a.requireJournal(); err != nil {
		return ports.JournalEntry{}, err
	}
	if strings.TrimSpace(input.Head) == "" {
		return ports.JournalEntry{}, domain.NewValidationError("commit.head", "is required")
	}
	fields := map[string]string{}
	for key, value := range map[string]string{
		"branch": input.Branch, "head": input.Head, "subject": input.Subject, "remote": input.Remote,
	} {
		if value = strings.TrimSpace(value); value != "" {
			fields[key] = value
		}
	}
	entry := ports.JournalEntry{
		Time:    a.now(),
		Event:   JournalCommit,
		User:    a.JournalUser(),
		Project: strings.TrimSpace(input.ProjectID),
		Fields:  fields,
	}
	if err := a.deps.Journal.Append(ctx, entry); err != nil {
		return ports.JournalEntry{}, err
	}
	return entry, nil
}

// ReadJournal returns journal entries newest first.
func (a *App) ReadJournal(ctx context.Context, query ports.JournalQuery) ([]ports.JournalEntry, error) {
	if err := a.requireJournal(); err != nil {
		return nil, err
	}
	return a.deps.Journal.Read(ctx, query)
}

func (a *App) requireJournal() error {
	if a.cfg.Mode == config.ModeServer {
		return fmt.Errorf("%w: the journal is local/git only; the server keeps revisions and an audit log", domain.ErrUnsupported)
	}
	if a.deps.Journal == nil {
		return fmt.Errorf("%w: journal is not available; check data_dir", domain.ErrUnsupported)
	}
	return nil
}
