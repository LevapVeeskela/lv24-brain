// Package folders implements a read-only native source that reads plain-text
// notes from local directories: general folders, Obsidian vaults and Logseq
// graphs. It needs no credentials.
//
// Behavior contract:
//   - Config settings: "paths" (required, comma/newline separated roots),
//     "extensions" (optional, default md,txt,rst,html), "ignore" (optional,
//     comma/newline globs or directory names, matched against each path
//     segment and the slash-separated relative path with path.Match).
//     The directories .git, .obsidian, .logseq and node_modules are always
//     pruned, at any depth. The configured root itself is never ignored.
//   - Symlinks are never followed: symlinked files are skipped and symlinked
//     directories are pruned, so a vault cannot escape its root.
//   - Only regular files with an allowed extension are candidates, visited in
//     ascending slash-separated relative-path order per root (nested files
//     included, deterministic across Windows/macOS via filepath.ToSlash).
//   - Files whose first bounded read contains NUL or is not valid UTF-8 are
//     skipped (binary/non-UTF-8). No file is ever read past
//     domain.MaxItemBodyBytes+1 bytes; longer text bodies are truncated to
//     domain.MaxItemBodyBytes on a UTF-8 boundary (the host would truncate
//     anyway and records it).
//   - Item kind is domain.ItemDocument, author/URL/labels are empty, times are
//     the file modification time in UTC (clock fallback when zero), title is
//     the first Markdown H1 ("# ...", single hash) after an optional YAML
//     frontmatter block, else the file base name. The body keeps the file
//     text verbatim: frontmatter is kept, Obsidian [[wikilinks]] and Logseq
//     ((block refs)) / property:: lines stay as text.
//   - External ID is the normalized slash-separated relative path. With a
//     single root it is exactly that; with several roots it is prefixed with
//     the first 8 hex chars of sha256(absRoot) ("<hash8>/<rel>") so identical
//     relative paths under different roots never collide. Adding a second
//     root re-keys single-root items (old keys stay, no tombstones).
//   - One checkpoint stream per root, "folders-<16 hex of sha256(absRoot)>",
//     declared dynamically (descriptor Streams is empty). Checkpoint JSON is
//     {"path","offset","lastScan","done"} (< 200 bytes, far below 64 KiB).
//     A finished scan resets offsets to 0 with lastScan = max(previous,
//     max emitted mtime of the finishing batch); the next scan replays from
//     lastScan-5m and relies on host hash dedup. req.Since (inclusive on
//     mtime) narrows via max(bound, Since). req.Limit caps items, last file
//     whole; More is true exactly while unvisited candidate files remain.
//   - Missing files (deleted between scans) only clamp offsets; no deletes
//     are ever inferred or emitted (Deletes capability is false).
//   - Overlapping roots are allowed but produce one item per root covering
//     the file; prefer disjoint roots.
package folders

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
)

const (
	// Name is the provider name registered for local folders.
	Name = "folders"

	checkpointVersion = 1

	// defaultReadLimit applies when ReadRequest.Limit <= 0.
	defaultReadLimit = 50
	// maxReadLimit bounds a single Read batch.
	maxReadLimit = 500

	// scanOverlap replays the tail of the previous scan so an update that
	// raced a checkpoint is still picked up; unchanged replays are no-ops
	// for the host (same Ref + same content = same Hash).
	scanOverlap = 5 * time.Minute
)

var (
	defaultExtensions = []string{"md", "txt", "rst", "html"}
	// alwaysIgnoredDirs is pruned at any depth, case-insensitively.
	alwaysIgnoredDirs = []string{".git", ".obsidian", ".logseq", "node_modules"}
)

// Descriptor returns the static provider description for local folders.
func Descriptor() domain.SourceDescriptor {
	return domain.SourceDescriptor{
		Provider:    Name,
		DisplayName: "Local folders",
		Capabilities: domain.Capabilities{
			Backfill:    true,
			Incremental: true,
			AuthModes:   []domain.AuthMode{domain.AuthNone},
		},
		DefaultPublish: domain.PublishLocalOnly,
		ConfigFields: []domain.ConfigField{
			{Key: "paths", Label: "Folders or vaults", Required: true, Help: "Comma or newline separated directories (plain folders, Obsidian vaults, Logseq graphs)."},
			{Key: "extensions", Label: "Extensions", Help: "Comma or newline separated file extensions without dots (default md,txt,rst,html)."},
			{Key: "ignore", Label: "Ignore", Help: "Comma or newline separated globs or directory names to skip (in addition to .git, .obsidian, .logseq, node_modules)."},
		},
		Backends: []domain.Backend{domain.BackendNative},
	}
}

// folderCheckpoint is the per-root cursor stored opaquely by the host.
type folderCheckpoint struct {
	Path     string     `json:"path"`
	Offset   int        `json:"offset"`
	LastScan *time.Time `json:"lastScan,omitempty"`
	Done     bool       `json:"done"`
}

// Source reads local text notes into raw items. It performs no network I/O
// and stores no credentials.
type Source struct {
	connection domain.Connection
	roots      []string // sorted absolute directories
	extensions map[string]struct{}
	ignores    []string
	now        func() time.Time
}

// NewSource validates the connection settings and binds the source to the
// configured roots. Paths must exist and be directories; entries with NUL
// or empty entries are rejected; every root is canonicalized with
// filepath.Abs, sorted and deduplicated.
func NewSource(conn domain.Connection, now func() time.Time) (*Source, error) {
	if err := Descriptor().ValidateConnection(conn); err != nil {
		return nil, err
	}
	roots, err := parseRoots(conn.Settings["paths"])
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Source{
		connection: conn,
		roots:      roots,
		extensions: parseExtensions(conn.Settings["extensions"]),
		ignores:    splitList(conn.Settings["ignore"]),
		now:        now,
	}, nil
}

// Descriptor returns the provider's static description.
func (s *Source) Descriptor() domain.SourceDescriptor { return Descriptor() }

// Check verifies every root is a readable directory. It reads directory
// entries only, never file content.
func (s *Source) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Stat(root)
		if err != nil {
			return fmt.Errorf("folders check: root %q is not accessible: %w", root, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("folders check: root %q is not a directory", root)
		}
		dir, err := os.Open(root)
		if err != nil {
			return fmt.Errorf("folders check: root %q is not readable: %w", root, err)
		}
		_, err = dir.Readdirnames(1)
		closeErr := dir.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("folders check: root %q is not readable: %w", root, err)
		}
		if closeErr != nil {
			return fmt.Errorf("folders check: root %q: %w", root, closeErr)
		}
	}
	return nil
}

// Read returns the next bounded batch across all roots in sorted-root,
// sorted-relative-path order. See the package doc for cursor, Since/Limit
// and skip semantics.
func (s *Source) Read(ctx context.Context, req ports.ReadRequest) (ports.ReadBatch, error) {
	if err := ctx.Err(); err != nil {
		return ports.ReadBatch{}, err
	}
	if req.Connection.ID != s.connection.ID || req.Connection.Provider != Name {
		return ports.ReadBatch{}, domain.NewValidationError("folders.connection", "does not match source connection")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}
	if limit > maxReadLimit {
		limit = maxReadLimit
	}

	multiRoot := len(s.roots) > 1
	type rootState struct {
		abs    string
		stream string
		prefix string // external-ID prefix, "" for a single root
		cursor folderCheckpoint
		bound  *time.Time
		files  []string
	}
	states := make([]*rootState, 0, len(s.roots))
	for _, root := range s.roots {
		stream := streamForRoot(root)
		cursor := folderCheckpoint{Path: root}
		if stored, ok := req.Checkpoints[stream]; ok {
			if stored.Stream != stream || stored.Version != checkpointVersion {
				return ports.ReadBatch{}, domain.NewValidationError("folders.checkpoint", "unsupported stream or version")
			}
			if err := json.Unmarshal(stored.Data, &cursor); err != nil {
				return ports.ReadBatch{}, domain.NewValidationError("folders.checkpoint", "is invalid")
			}
			if cursor.Offset < 0 {
				return ports.ReadBatch{}, domain.NewValidationError("folders.checkpoint", "position is invalid")
			}
			cursor.Path = root
			if cursor.LastScan != nil {
				utc := cursor.LastScan.UTC()
				cursor.LastScan = &utc
			}
		}
		files, err := s.listFiles(root)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("folders read: %w", err)
		}
		if cursor.Offset > len(files) {
			// Files vanished since the cursor was stored; clamp and
			// continue. No deletes are inferred.
			cursor.Offset = len(files)
		}
		prefix := ""
		if multiRoot {
			prefix = rootShortHash(root) + "/"
		}
		states = append(states, &rootState{
			abs:    root,
			stream: stream,
			prefix: prefix,
			cursor: cursor,
			bound:  overlapBound(cursor.LastScan, req.Since),
			files:  files,
		})
	}

	items := make([]domain.Item, 0, limit)
	maxima := make([]*time.Time, len(states))
outer:
	for i, st := range states {
		for st.cursor.Offset < len(st.files) {
			if len(items) >= limit {
				break outer
			}
			if err := ctx.Err(); err != nil {
				return ports.ReadBatch{}, err
			}
			rel := st.files[st.cursor.Offset]
			st.cursor.Offset++
			item, emitted := s.readFile(st.abs, st.prefix, rel, st.bound)
			if !emitted {
				continue
			}
			items = append(items, *item)
			maxima[i] = maxTime(maxima[i], &item.UpdatedAt)
		}
	}

	more := false
	for _, st := range states {
		if st.cursor.Offset < len(st.files) {
			more = true
			break
		}
	}

	checkpoints := make([]domain.Checkpoint, 0, len(states))
	for i, st := range states {
		next := folderCheckpoint{Path: st.abs}
		if more {
			next.Offset = st.cursor.Offset
			next.LastScan = st.cursor.LastScan
		} else {
			next.Done = true
			next.LastScan = maxTime(st.cursor.LastScan, maxima[i])
		}
		data, err := json.Marshal(next)
		if err != nil {
			return ports.ReadBatch{}, fmt.Errorf("encode folders checkpoint: %w", err)
		}
		checkpoints = append(checkpoints, domain.Checkpoint{Stream: st.stream, Version: checkpointVersion, Data: data})
	}
	sort.Slice(checkpoints, func(i, j int) bool { return checkpoints[i].Stream < checkpoints[j].Stream })
	return ports.ReadBatch{Items: items, Checkpoints: checkpoints, More: more}, nil
}

// readFile stats, filters and reads one candidate. ok=false means the file
// is skipped (Since, unreadable, vanished, non-regular, binary/non-UTF-8 or
// overlong external ID); only context-adjacent IO races skip silently.
func (s *Source) readFile(root, prefix, rel string, bound *time.Time) (item *domain.Item, ok bool) {
	externalID := prefix + rel
	if len(externalID) > 1024 || strings.ContainsRune(externalID, 0) {
		return nil, false
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if err != nil {
		return nil, false
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false
	}
	mod := info.ModTime().UTC()
	if mod.IsZero() {
		mod = s.now().UTC()
	}
	if bound != nil && mod.Before(*bound) {
		return nil, false
	}
	body, ok := readBoundedBody(full)
	if !ok {
		return nil, false
	}
	title := extractTitle(body, path.Base(externalID))
	if strings.TrimSpace(title) == "" {
		title = path.Base(externalID)
	}
	out := domain.Item{
		Ref:       domain.ItemRef{ConnectionID: s.connection.ID, ExternalID: externalID},
		Kind:      domain.ItemDocument,
		Title:     title,
		Body:      body,
		CreatedAt: mod,
		UpdatedAt: mod,
	}
	if len(s.connection.Projects) == 1 {
		out.ProjectID = s.connection.Projects[0]
	}
	return &out, true
}

// listFiles walks root and returns the sorted slash-separated relative paths
// of candidate files. Symlinks are never followed, ignored directories are
// pruned, non-regular files and disallowed extensions are skipped.
func (s *Source) listFiles(root string) ([]string, error) {
	var rels []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			// Never follow symlinks; WalkDir does not descend into them.
			return nil
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		slashRel := filepath.ToSlash(rel)
		if d.IsDir() {
			if isAlwaysIgnored(d.Name()) || matchesIgnore(d.Name(), slashRel, s.ignores) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if _, allowed := s.extensions[extensionKey(d.Name())]; !allowed {
			return nil
		}
		if matchesIgnore(d.Name(), slashRel, s.ignores) {
			return nil
		}
		rels = append(rels, slashRel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(rels)
	return rels, nil
}

// readBoundedBody reads at most MaxItemBodyBytes+1 bytes and reports ok=false
// for unreadable, NUL-containing or non-UTF-8 content.
func readBoundedBody(full string) (body string, ok bool) {
	file, err := os.Open(full)
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	buf, err := io.ReadAll(io.LimitReader(file, domain.MaxItemBodyBytes+1))
	if err != nil {
		return "", false
	}
	if bytes.IndexByte(buf, 0) >= 0 {
		return "", false
	}
	if !utf8.Valid(buf) {
		return "", false
	}
	if len(buf) > domain.MaxItemBodyBytes {
		buf = truncateUTF8(buf[:domain.MaxItemBodyBytes])
	}
	return string(buf), true
}

// truncateUTF8 cuts bytes to a valid UTF-8 prefix.
func truncateUTF8(buf []byte) []byte {
	for len(buf) > 0 && !utf8.Valid(buf) {
		buf = buf[:len(buf)-1]
	}
	return buf
}

// extractTitle returns the first "# ..." H1 after an optional YAML
// frontmatter block, or fallback when none has text.
func extractTitle(body, fallback string) string {
	content := strings.TrimPrefix(body, "\xef\xbb\xbf")
	lines := strings.Split(content, "\n")
	start := 0
	if len(lines) > 0 && strings.TrimSpace(strings.TrimSuffix(lines[0], "\r")) == "---" {
		for i := 1; i < len(lines) && i < 200; i++ {
			edge := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
			if edge == "---" || edge == "..." {
				start = i + 1
				break
			}
		}
	}
	for _, line := range lines[start:] {
		trimmed := strings.TrimLeft(strings.TrimSuffix(line, "\r"), " \t")
		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "#\t") {
			if title := strings.TrimSpace(trimmed[1:]); title != "" {
				return title
			}
		}
	}
	return fallback
}

func parseRoots(raw string) ([]string, error) {
	entries := splitList(raw)
	if len(entries) == 0 {
		return nil, domain.NewValidationError("folders.paths", "must contain at least one directory")
	}
	roots := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.ContainsRune(entry, 0) {
			return nil, domain.NewValidationError("folders.paths", "must not contain NUL")
		}
		abs, err := filepath.Abs(entry)
		if err != nil {
			return nil, domain.NewValidationError("folders.paths", fmt.Sprintf("entry %q is invalid: %v", entry, err))
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, domain.NewValidationError("folders.paths", fmt.Sprintf("entry %q is not accessible: %v", entry, err))
		}
		if !info.IsDir() {
			return nil, domain.NewValidationError("folders.paths", fmt.Sprintf("entry %q is not a directory", entry))
		}
		roots = append(roots, abs)
	}
	sort.Strings(roots)
	out := roots[:0]
	for _, root := range roots {
		if len(out) == 0 || out[len(out)-1] != root {
			out = append(out, root)
		}
	}
	return out, nil
}

func parseExtensions(raw string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, entry := range splitList(raw) {
		if key := extensionKey(entry); key != "" {
			set[key] = struct{}{}
		}
	}
	if len(set) == 0 {
		for _, ext := range defaultExtensions {
			set[ext] = struct{}{}
		}
	}
	return set
}

func extensionKey(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimPrefix(name, ".")
	if i := strings.LastIndex(name, "."); i >= 0 && !strings.ContainsAny(name, ",/\n\r") {
		// A full file name maps to its suffix ("note.MD" -> "md").
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	for _, r := range name {
		if r < 'a' || r > 'z' {
			if r < '0' || r > '9' {
				return ""
			}
		}
	}
	return name
}

// splitList splits comma/newline separated settings and drops empties.
func splitList(raw string) []string {
	var out []string
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func isAlwaysIgnored(name string) bool {
	for _, dir := range alwaysIgnoredDirs {
		if strings.EqualFold(name, dir) {
			return true
		}
	}
	return false
}

func matchesIgnore(name, slashRel string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if ok, _ := path.Match(pattern, name); ok {
			return true
		}
		if ok, _ := path.Match(pattern, slashRel); ok {
			return true
		}
		if pattern == name || pattern == slashRel {
			return true
		}
	}
	return false
}

// streamForRoot names the deterministic per-root stream.
func streamForRoot(absRoot string) string {
	sum := sha256.Sum256([]byte(absRoot))
	return "folders-" + hex.EncodeToString(sum[:])[:16]
}

// rootShortHash prefixes multi-root external IDs.
func rootShortHash(absRoot string) string {
	sum := sha256.Sum256([]byte(absRoot))
	return hex.EncodeToString(sum[:])[:8]
}

// overlapBound replays lastScan-5m, narrowed by an explicit Since.
func overlapBound(lastScan, since *time.Time) *time.Time {
	var bound *time.Time
	if lastScan != nil {
		overlap := lastScan.UTC().Add(-scanOverlap)
		bound = &overlap
	}
	if since != nil {
		floor := since.UTC()
		if bound == nil || floor.After(*bound) {
			bound = &floor
		}
	}
	return bound
}

func maxTime(left, right *time.Time) *time.Time {
	if left == nil {
		return right
	}
	if right != nil && right.After(*left) {
		return right
	}
	return left
}

var _ ports.Source = (*Source)(nil)
