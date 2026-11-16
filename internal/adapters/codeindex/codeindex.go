// Package codeindex builds and queries a native code graph for Go,
// TypeScript/JavaScript and Python checkouts.
//
// The store is derived data: one SQLite file per checkout under the brain data
// directory, never inside the checkout and never in git. It holds files,
// symbols, edges and meta rows only; source text is re-read from the checkout
// at query time. The Go extractor uses go/ast for declarations and go/types
// (via golang.org/x/tools/go/packages) for exact call, method, embedding and
// interface-satisfaction resolution. The script extractor runs a pinned
// tree-sitter WASI reactor with wazero and extracts syntax-level declarations,
// imports, calls, heritage and type references; it never invents a
// type-resolved edge.
//
// The adapter implements ports.GraphIndex for the "native" engine:
//
//	Index   full rebuild of every package in the module
//	Sync    re-extract only changed packages, rebuild on a symbol-set change
//	Search  exact name, then FTS5, then substring
//	Node    one symbol with source and direct neighbours
//	Explore symbols, source, flow and blast radius under MaxGraphExploreBytes
//	Impact  reverse breadth-first walk over dependency edges
package codeindex

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/levapveeskela/lv24-brain/internal/domain"
	"github.com/levapveeskela/lv24-brain/internal/ports"
	_ "modernc.org/sqlite"
)

const (
	// Engine is the engine name the adapter reports and config accepts.
	Engine = ports.GraphEngineNative

	// Language is the type-checked language of the native extractor. The
	// syntax-only tree-sitter languages are reported by LanguageForPath.
	Language = "go"

	// DefaultDBSubdir is the derived store directory under the brain data
	// directory. It is documented and covered by the repository .gitignore.
	DefaultDBSubdir = "index/graph"

	// DefaultIndexPattern is the package pattern of a full index.
	DefaultIndexPattern = "./..."

	busyTimeoutMS = 5000
	// maxSourceFileBytes refuses to read a checkout file larger than this.
	maxSourceFileBytes = 8 << 20
	// maxSymbolSourceLines bounds the declaration Node renders.
	maxSymbolSourceLines = 400
	// defaultNeighborLimit bounds the caller and callee lists of one node.
	defaultNeighborLimit = 20
	// maxDocRunes and maxSignatureRunes keep one declaration row small; a
	// giant generated doc must not dominate the store.
	maxDocRunes       = 2000
	maxSignatureRunes = 480
)

// Errors returned before a query can be answered.
var (
	// ErrIndexMissing reports a store that has not been created yet.
	ErrIndexMissing = errors.New("codeindex: graph index does not exist")
	// ErrIndexEmpty reports a store that exists but holds no files.
	ErrIndexEmpty = errors.New("codeindex: graph index is empty; run brain code index")
	// ErrSchemaMismatch wraps domain.ErrUnsupported for a store written by
	// another schema or extractor version. The writer rebuilds it; a reader
	// asks the caller to rebuild instead of misreading old columns.
	ErrSchemaMismatch = errors.New("codeindex: graph index schema mismatch; run brain code index --rebuild")
)

// Options configures one native graph index bound to one checkout.
type Options struct {
	// Root is the checkout the index describes. It is required.
	Root string
	// DBPath is the derived SQLite file. It must be outside the checkout and
	// is normally resolved by DefaultDBPath.
	DBPath string
	// Project labels the answers.
	Project string
	// Tags are extra Go build tags used when scanning and loading packages.
	Tags []string
	// Exclude lists repository-relative directory prefixes to skip.
	Exclude []string
	// Patterns overrides the package patterns of a full index. The default is
	// "./...".
	Patterns []string
	// Now overrides the clock in tests.
	Now func() time.Time
	// Create creates the database file when it is absent. A read command
	// leaves it false so a query does not materialize an empty index.
	Create bool
}

// Index is one native graph store bound to one checkout.
type Index struct {
	db       *sql.DB
	dbPath   string
	root     string
	project  string
	module   string
	tags     []string
	exclude  []string
	patterns []string
	now      func() time.Time

	needsRebuild bool
	notes        []string
	ts           *tsEngine
	mu           sync.Mutex
}

var _ ports.GraphIndex = (*Index)(nil)

// Open opens the store at opts.DBPath. It never writes rows; Create only
// materializes an empty schema so Index and Sync have a place to write.
func Open(ctx context.Context, opts Options) (*Index, error) {
	root, err := normalizeRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	dbPath := strings.TrimSpace(opts.DBPath)
	if dbPath == "" {
		return nil, domain.NewValidationError("graph.path", "is required for the native engine")
	}
	dbPath = filepath.Clean(dbPath)
	if !opts.Create {
		if _, statErr := os.Stat(dbPath); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s; run brain code index", ErrIndexMissing, dbPath)
			}
			return nil, fmt.Errorf("codeindex: inspect %s: %w", dbPath, statErr)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("codeindex: create index directory: %w", err)
	}
	db, err := openStore(dbPath)
	if err != nil {
		return nil, err
	}
	index := &Index{
		db:       db,
		dbPath:   dbPath,
		root:     root,
		project:  strings.TrimSpace(opts.Project),
		tags:     append([]string(nil), opts.Tags...),
		exclude:  normalizeExcludes(opts.Exclude),
		patterns: append([]string(nil), opts.Patterns...),
		now:      opts.Now,
	}
	if index.now == nil {
		index.now = time.Now
	}
	meta, err := index.readMeta(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if len(meta) == 0 {
		index.needsRebuild = true
		if opts.Create {
			if err := index.rebuildSchema(ctx); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
	} else {
		schema, extractor := metaInt(meta, metaSchema), metaInt(meta, metaExtractor)
		if schema != SchemaVersion || extractor != ExtractorVersion {
			index.needsRebuild = true
			index.notes = append(index.notes, fmt.Sprintf(
				"index was written with schema %d/extraction %d; this engine reads %d/%d and rebuilds on the next write",
				schema, extractor, SchemaVersion, ExtractorVersion))
		}
		index.module = strings.TrimSpace(meta[metaModule])
		if project := strings.TrimSpace(meta[metaProject]); project != "" && index.project == "" {
			index.project = project
		}
	}
	if index.module == "" {
		index.module = readModulePath(root)
	}
	return index, nil
}

// Close checkpoints the WAL and releases the database handle.
func (i *Index) Close() error {
	if i == nil {
		return nil
	}
	if i.ts != nil {
		_ = i.ts.close()
		i.ts = nil
	}
	if i.db == nil {
		return nil
	}
	_, _ = i.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return i.db.Close()
}

// DBPath is the derived SQLite file.
func (i *Index) DBPath() string { return i.dbPath }

// Root is the checkout the index describes.
func (i *Index) Root() string { return i.root }

// Module is the Go module path the qualnames are trimmed against.
func (i *Index) Module() string { return i.module }

// openStore opens the writable store with WAL and a bounded busy wait.
func openStore(path string) (*sql.DB, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMS) + ")" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("codeindex: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("codeindex: open %s: %w", path, err)
	}
	return db, nil
}

// Status reports the index identity, counts, age and git commit.
func (i *Index) Status(ctx context.Context, project string) (ports.GraphStatus, error) {
	status := ports.GraphStatus{
		Engine:             Engine,
		Project:            i.label(project),
		Root:               i.root,
		DBPath:             i.dbPath,
		SchemaVersion:      SchemaVersion,
		ExtractionVersion:  ExtractorVersion,
		IndexedWithVersion: fmt.Sprintf("native/%d", ExtractorVersion),
		Dirty:              false,
		Notes:              append([]string(nil), i.notes...),
	}
	if info, err := os.Stat(i.dbPath); err == nil {
		status.SizeBytes = info.Size()
	}
	meta, err := i.readMeta(ctx)
	if err != nil {
		return ports.GraphStatus{}, err
	}
	status.ExtractionVersion = metaInt(meta, metaExtractor)
	if status.ExtractionVersion == 0 {
		status.ExtractionVersion = ExtractorVersion
	}
	indexedAt := metaInt64(meta, metaIndexedAt)
	if indexedAt > 0 {
		status.IndexedAt = time.UnixMilli(indexedAt).UTC()
		if age := i.now().Sub(status.IndexedAt); age > 0 {
			status.AgeSeconds = int64(age.Seconds())
		}
	}
	status.Commit = strings.TrimSpace(meta[metaGitSHA])
	status.Dirty = meta[metaGitDirty] == "1"
	status.IndexState = strings.TrimSpace(meta[metaState])
	if status.IndexState == "" {
		status.IndexState = stateEmpty
	}
	if i.needsRebuild {
		status.IndexState = "stale-schema"
		status.Notes = append(status.Notes,
			"the store was written by another schema; run brain code index --rebuild before trusting counts")
	}
	if i.needsRebuild {
		return status, nil
	}
	if err := i.db.QueryRowContext(ctx, `SELECT count(*) FROM files`).Scan(&status.Files); err != nil {
		return ports.GraphStatus{}, fmt.Errorf("codeindex: count files: %w", err)
	}
	if err := i.db.QueryRowContext(ctx, `SELECT count(*) FROM symbols`).Scan(&status.Nodes); err != nil {
		return ports.GraphStatus{}, fmt.Errorf("codeindex: count symbols: %w", err)
	}
	if err := i.db.QueryRowContext(ctx, `SELECT count(*) FROM edges`).Scan(&status.Edges); err != nil {
		return ports.GraphStatus{}, fmt.Errorf("codeindex: count edges: %w", err)
	}
	var unresolved sql.NullInt64
	if err := i.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(unresolved), 0) FROM files`).Scan(&unresolved); err != nil {
		return ports.GraphStatus{}, fmt.Errorf("codeindex: count unresolved references: %w", err)
	}
	status.UnresolvedRefs = int(unresolved.Int64)
	status.NodesByKind, err = i.countBy(ctx, `SELECT kind, count(*) FROM symbols GROUP BY kind ORDER BY count(*) DESC, kind LIMIT 16`)
	if err != nil {
		return ports.GraphStatus{}, err
	}
	status.FilesByLanguage, err = i.countBy(ctx, `SELECT language, count(*) FROM files GROUP BY language ORDER BY count(*) DESC, language LIMIT 8`)
	if err != nil {
		return ports.GraphStatus{}, err
	}
	if status.Files == 0 {
		status.IndexState = stateEmpty
		status.Notes = append(status.Notes, "the store is empty; run brain code index")
	}
	if skipped := metaInt(meta, metaSkipped); skipped > 0 {
		status.Notes = append(status.Notes, fmt.Sprintf("%d file(s) were skipped (oversize, unreadable or excluded by build constraints)", skipped))
	}
	if pkgErrors := metaInt(meta, metaPkgErrors); pkgErrors > 0 {
		status.Notes = append(status.Notes, fmt.Sprintf("%d package(s) reported type or load errors; symbols were extracted syntactically where possible", pkgErrors))
	}
	if partial := metaInt(meta, metaPartial); partial > 0 {
		status.Notes = append(status.Notes, fmt.Sprintf("%d file(s) had syntax errors at the last full index; declarations intersecting ERROR spans were skipped", partial))
	}
	return status, nil
}

func (i *Index) countBy(ctx context.Context, query string) (map[string]int, error) {
	rows, err := i.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("codeindex: count by kind: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, fmt.Errorf("codeindex: count by kind: %w", err)
		}
		out[name] = count
	}
	return out, rows.Err()
}

// label resolves the project label of an answer.
func (i *Index) label(project string) string {
	if strings.TrimSpace(project) == "" {
		return i.project
	}
	return project
}

// ensureReadable refuses a query against a store the reader cannot trust.
func (i *Index) ensureReadable(ctx context.Context) error {
	if i.needsRebuild {
		return ErrSchemaMismatch
	}
	var files int
	if err := i.db.QueryRowContext(ctx, `SELECT count(*) FROM files`).Scan(&files); err != nil {
		return fmt.Errorf("codeindex: inspect store: %w", err)
	}
	if files == 0 {
		return ErrIndexEmpty
	}
	return nil
}

// DefaultDBPath resolves the derived store of one checkout. The project id and
// a short hash of the canonical checkout path keep two checkouts (a worktree
// and its main tree) from sharing a path-keyed index.
func DefaultDBPath(dataDir, project, root string) (string, error) {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return "", domain.NewValidationError("config.data_dir", "is required for the native graph engine")
	}
	name := sanitizeProjectID(project)
	if name == "" {
		name = "project"
	}
	canonical := canonicalRoot(root)
	digest := sha256.Sum256([]byte(canonical))
	return filepath.Join(dataDir, filepath.FromSlash(DefaultDBSubdir),
		name+"-"+hex.EncodeToString(digest[:])[:8]+".db"), nil
}

var invalidProjectChar = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// sanitizeProjectID makes a project id safe as one file name component.
func sanitizeProjectID(project string) string {
	trimmed := strings.TrimSpace(project)
	if trimmed == "" {
		return ""
	}
	trimmed = invalidProjectChar.ReplaceAllString(trimmed, "-")
	trimmed = strings.Trim(trimmed, "-.")
	if len(trimmed) > 64 {
		trimmed = trimmed[:64]
	}
	return trimmed
}

// canonicalRoot is stable across relative and absolute spellings of the same
// checkout on the same platform.
func canonicalRoot(root string) string {
	cleaned := filepath.Clean(strings.TrimSpace(root))
	if absolute, err := filepath.Abs(cleaned); err == nil {
		cleaned = absolute
	}
	cleaned = filepath.ToSlash(cleaned)
	if runtime.GOOS == "windows" {
		cleaned = strings.ToLower(cleaned)
	}
	return cleaned
}

// normalizeRoot validates and cleans the checkout root.
func normalizeRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", domain.NewValidationError("graph.root", "is required for the native engine")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("codeindex: resolve root %s: %w", root, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("codeindex: root %s: %w", root, err)
	}
	if !info.IsDir() {
		return "", domain.NewValidationError("graph.root", fmt.Sprintf("%q is not a directory", root))
	}
	return filepath.Clean(absolute), nil
}

// normalizeExcludes turns exclude prefixes into the slash form of the index.
func normalizeExcludes(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = normalizeRepoPath(value)
		value = strings.TrimSuffix(value, "/")
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

// relPath converts an absolute file path to the repository-relative slash
// form. It reports false for a path outside the checkout.
func (i *Index) relPath(absolute string) (string, bool) {
	if absolute == "" {
		return "", false
	}
	rel, err := filepath.Rel(i.root, filepath.Clean(absolute))
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// readModulePath finds the nearest go.mod above root and returns its module
// path. An absent module is not fatal: qualnames then keep the full package
// path.
func readModulePath(root string) string {
	dir := filepath.Clean(root)
	for {
		candidate := filepath.Join(dir, "go.mod")
		if raw, err := os.ReadFile(candidate); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "module") {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) >= 2 && fields[0] == "module" {
					return strings.Trim(fields[1], `"`)
				}
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// gitHead reads .git/HEAD without invoking git. A linked worktree's gitdir
// file and packed refs are handled; no other git state is interpreted.
func gitHead(root string) string {
	dir := filepath.Clean(root)
	for {
		candidate := filepath.Join(dir, ".git")
		if info, err := os.Stat(candidate); err == nil {
			gitDir := candidate
			if !info.IsDir() {
				if raw, readErr := os.ReadFile(candidate); readErr == nil {
					line := strings.TrimSpace(string(raw))
					if strings.HasPrefix(line, "gitdir:") {
						gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
						if !filepath.IsAbs(gitDir) {
							gitDir = filepath.Join(dir, gitDir)
						}
					}
				}
			}
			return readGitHEAD(gitDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// readGitHEAD resolves HEAD to a commit hash.
func readGitHEAD(gitDir string) string {
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(head, "ref: ") {
		if isHexHash(head) {
			return head
		}
		return ""
	}
	ref := strings.TrimSpace(strings.TrimPrefix(head, "ref: "))
	if value, readErr := os.ReadFile(filepath.Join(gitDir, filepath.FromSlash(ref))); readErr == nil {
		candidate := strings.TrimSpace(string(value))
		if isHexHash(candidate) {
			return candidate
		}
	}
	packed, readErr := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if readErr != nil {
		return ""
	}
	for _, line := range strings.Split(string(packed), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref && isHexHash(fields[0]) {
			return fields[0]
		}
	}
	return ""
}

// isHexHash reports whether value looks like a full git object hash.
func isHexHash(value string) bool {
	if len(value) < 40 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

// gitDirty reports whether the tracked worktree differs from HEAD. It runs for
// a few seconds at most and a missing or failed git command is reported as
// clean rather than blocking the index.
func gitDirty(root string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "--untracked-files=no").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) != ""
}

// normalizeRepoPath turns a Windows or "./"-prefixed path into the POSIX form
// the index uses.
func normalizeRepoPath(value string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	normalized = strings.TrimPrefix(normalized, "./")
	return strings.TrimPrefix(normalized, "/")
}

// placeholders renders n comma-separated SQL placeholders.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// stringsToAny converts a string list for variadic query arguments.
func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for index, value := range values {
		out[index] = value
	}
	return out
}

// truncateRunes shortens s to at most limit runes without splitting one.
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// clampLimit clamps a caller limit into [fallback, maximum].
func clampLimit(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
