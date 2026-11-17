// Package journal stores per-user activity journals as JSON Lines under
// <data>/journal/<user>.jsonl. The format matches the legacy PowerShell brain
// (flat objects with "ts", "event", "project", ...), so old and new lines
// coexist in one file and both tools can read them.
package journal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// Dir is the journal directory inside the data directory.
	Dir = "journal"
	// DefaultLimit bounds a read without an explicit limit.
	DefaultLimit = 50
	// MaxLimit bounds any read.
	MaxLimit = 1000
	// maxLineBytes bounds one appended line; longer values are truncated so
	// a line stays a single small append.
	maxLineBytes = 4 << 10
	// maxFieldRunes bounds every string value written.
	maxFieldRunes = 500

	// lockSuffix names the cross-process lock of one journal file.
	lockSuffix = ".lock"
	// appendLockWait bounds how long an append waits for a live holder.
	appendLockWait = 10 * time.Second
	// readLockWait is short: a reader falls back to an unlocked read rather
	// than block a listing while a writer holds the lock.
	readLockWait = 250 * time.Millisecond
	// lockStaleAfter is how long a lock may belong to a crashed writer before
	// another process clears it.
	lockStaleAfter = 30 * time.Second
	// lockRetryMin and lockRetryMax bound the acquisition backoff.
	lockRetryMin = 2 * time.Millisecond
	lockRetryMax = 50 * time.Millisecond
)

var reserved = map[string]struct{}{
	"ts": {}, "event": {}, "user": {}, "project": {}, "id": {}, "title": {},
}

// Store is a filesystem journal rooted at a data directory.
type Store struct {
	dir string
	mu  sync.Mutex
}

var _ ports.Journal = (*Store)(nil)

// New returns a journal for dataDir. The directory is created on first write.
func New(dataDir string) *Store {
	return &Store{dir: filepath.Join(dataDir, Dir)}
}

// FileName maps a user to a safe journal file name. Anything outside
// [A-Za-z0-9._-] becomes "-", so a user value can never escape the journal
// directory.
func FileName(user string) string {
	user = strings.TrimSpace(user)
	var b strings.Builder
	for _, r := range user {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), ".-")
	if name == "" {
		name = "unknown"
	}
	return name + ".jsonl"
}

// Append writes one entry to the entry user's journal file.
func (s *Store) Append(ctx context.Context, entry ports.JournalEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(entry.Event) == "" {
		return domain.NewValidationError("journal.event", "is required")
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}
	line, err := encode(entry)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("journal: create directory: %w", err)
	}
	path := filepath.Join(s.dir, FileName(entry.User))
	release, err := journalLock(ctx, path, appendLockWait)
	if err != nil {
		return err
	}
	defer release()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("journal: open %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		return fmt.Errorf("journal: append %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("journal: sync %s: %w", filepath.Base(path), err)
	}
	return file.Close()
}

func encode(entry ports.JournalEntry) ([]byte, error) {
	object := map[string]string{
		"ts":    entry.Time.Format(time.RFC3339Nano),
		"event": clip(entry.Event),
	}
	set := func(key, value string) {
		if value = clip(value); value != "" {
			object[key] = value
		}
	}
	set("user", entry.User)
	set("project", entry.Project)
	set("id", entry.ID)
	set("title", entry.Title)
	for key, value := range entry.Fields {
		if _, taken := reserved[key]; taken || strings.TrimSpace(key) == "" {
			continue
		}
		set(key, value)
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("journal: encode: %w", err)
	}
	if len(raw) > maxLineBytes {
		// Drop the free-form fields first; the core keys always fit.
		for key := range entry.Fields {
			delete(object, key)
		}
		object["truncated"] = "true"
		if raw, err = json.Marshal(object); err != nil {
			return nil, fmt.Errorf("journal: encode: %w", err)
		}
	}
	return append(raw, '\n'), nil
}

func clip(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if runes := []rune(value); len(runes) > maxFieldRunes {
		value = string(runes[:maxFieldRunes])
	}
	return value
}

// Read scans every journal file and returns matching entries newest first.
// Malformed lines are skipped, and a UTF-8 BOM written by the legacy tool is
// tolerated.
func (s *Store) Read(ctx context.Context, query ports.JournalQuery) ([]ports.JournalEntry, error) {
	files, err := filepath.Glob(filepath.Join(s.dir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("journal: list: %w", err)
	}
	limit := query.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	var out []ports.JournalEntry
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		stem := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		entries, err := readFile(ctx, path, stem)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if matches(entry, query) {
				out = append(out, entry)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	out = dedupeEntries(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// journalKey is the identity of one journal line for read deduplication. A
// retried append or a replayed legacy line therefore counts once.
type journalKey struct {
	ts    int64
	event string
	id    string
}

// dedupeEntries drops repeated (ts, event, id) entries, keeping the first in
// the given order.
func dedupeEntries(entries []ports.JournalEntry) []ports.JournalEntry {
	seen := make(map[journalKey]struct{}, len(entries))
	out := entries[:0]
	for _, entry := range entries {
		key := journalKey{ts: entry.Time.UTC().UnixNano(), event: entry.Event, id: entry.ID}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, entry)
	}
	return out
}

func readFile(ctx context.Context, path, stem string) ([]ports.JournalEntry, error) {
	// A writer holds the lock for one small append, so a reader normally
	// waits a few milliseconds. On a timeout it reads anyway: every line is a
	// single O_APPEND write and dedupe absorbs a line read twice.
	if release, lockErr := journalLock(ctx, path, readLockWait); lockErr == nil {
		defer release()
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("journal: open %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var out []ports.JournalEntry
	for scanner.Scan() {
		line := bytes.TrimPrefix(scanner.Bytes(), []byte("\xef\xbb\xbf"))
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		entry, ok := decode(line, stem)
		if ok {
			out = append(out, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("journal: read %s: %w", filepath.Base(path), err)
	}
	return out, nil
}

func decode(line []byte, stem string) (ports.JournalEntry, bool) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return ports.JournalEntry{}, false
	}
	text := func(key string) string {
		switch value := raw[key].(type) {
		case string:
			return value
		case float64, bool:
			return fmt.Sprint(value)
		default:
			return ""
		}
	}
	at, err := time.Parse(time.RFC3339Nano, text("ts"))
	if err != nil || text("event") == "" {
		return ports.JournalEntry{}, false
	}
	entry := ports.JournalEntry{
		Time:    at,
		Event:   text("event"),
		User:    text("user"),
		Project: text("project"),
		ID:      text("id"),
		Title:   text("title"),
	}
	if entry.Project == "" {
		// Legacy connector-sync lines use project_id.
		entry.Project = text("project_id")
	}
	if entry.User == "" {
		entry.User = stem
	}
	for key := range raw {
		if _, taken := reserved[key]; taken {
			continue
		}
		if value := text(key); value != "" {
			if entry.Fields == nil {
				entry.Fields = map[string]string{}
			}
			entry.Fields[key] = value
		}
	}
	return entry, true
}

func matches(entry ports.JournalEntry, query ports.JournalQuery) bool {
	switch {
	case query.User != "" && !strings.EqualFold(entry.User, query.User):
		return false
	case query.Project != "" && entry.Project != query.Project:
		return false
	case query.Event != "" && entry.Event != query.Event:
		return false
	case !query.Since.IsZero() && entry.Time.Before(query.Since):
		return false
	}
	return true
}

// journalLock serializes appends (and best-effort reads) of one journal file
// across processes. The lock is "<file>.jsonl.lock", created with O_EXCL next
// to the journal; it holds no payload, so it is cleared only after
// lockStaleAfter, which is far longer than one append.
func journalLock(ctx context.Context, path string, wait time.Duration) (func(), error) {
	lockPath := path + lockSuffix
	deadline := time.Now().Add(wait)
	backoff := lockRetryMin
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			_ = file.Close()
			released := false
			return func() {
				if released {
					return
				}
				released = true
				_ = os.Remove(lockPath)
			}, nil
		}
		// Windows reports a lock that is still being deleted as access denied
		// rather than exists; both mean busy.
		busy := os.IsExist(err) || errors.Is(err, fs.ErrPermission)
		if !busy {
			return nil, fmt.Errorf("journal: create lock %s: %w", filepath.Base(lockPath), err)
		}
		if staleJournalLock(lockPath) {
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("journal: lock %s after %s", filepath.Base(lockPath), wait)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		if backoff *= 2; backoff > lockRetryMax {
			backoff = lockRetryMax
		}
	}
}

// staleJournalLock removes a lock left by a crashed writer. The mtime is read
// twice so a lock created between the checks is not deleted.
func staleJournalLock(lockPath string) bool {
	info, err := os.Stat(lockPath)
	if err != nil {
		return os.IsNotExist(err)
	}
	if time.Since(info.ModTime()) < lockStaleAfter {
		return false
	}
	current, err := os.Stat(lockPath)
	if err != nil || !current.ModTime().Equal(info.ModTime()) {
		return false
	}
	return os.Remove(lockPath) == nil
}
