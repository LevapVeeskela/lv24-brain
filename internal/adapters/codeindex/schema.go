package codeindex

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// SchemaVersion is the graph schema this package reads and writes. A database
// written by another schema is rebuilt from the checkout instead of migrated:
// the index is derived data, so a rebuild is cheaper and safer than a
// migration that can leave half-mapped rows behind.
//
// Schema 2 adds the communities and community_members tables.
const SchemaVersion = 2

// ExtractorVersion is the version of the extraction rules. Bumping it forces a
// rebuild because symbol identity or edge kinds may have changed.
//
// Version 2 adds the tree-sitter TypeScript/TSX/JavaScript/JSX extractor and
// the module symbol kind. Version 3 adds the syntax-only Python extractor.
const ExtractorVersion = 3

// metaKeys are the keys stored in the meta table.
const (
	metaSchema        = "schema_version"
	metaExtractor     = "extractor_version"
	metaEngine        = "engine"
	metaProject       = "project"
	metaModule        = "module"
	metaGitSHA        = "git_sha"
	metaGitDirty      = "git_dirty"
	metaIndexedAt     = "indexed_at"
	metaState         = "index_state"
	metaUnresolved    = "unresolved_refs"
	metaSkipped       = "skipped_files"
	metaPkgErrors     = "package_errors"
	metaCommunities   = "communities"
	metaCommunityAlgo = "community_algorithm"
	metaGraphDigest   = "graph_digest"
	metaPartial       = "partial_files"
)

// schemaStatements creates the graph store. There is deliberately no source
// text here: files record identity, symbols record ranges and declaration
// text, and a query re-reads the body from the checkout.
var schemaStatements = []string{
	`CREATE TABLE meta (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	) WITHOUT ROWID`,
	`CREATE TABLE files (
		path TEXT PRIMARY KEY,
		sha256 TEXT NOT NULL,
		size INTEGER NOT NULL,
		mtime_ns INTEGER NOT NULL,
		package TEXT NOT NULL,
		language TEXT NOT NULL,
		indexed_at INTEGER NOT NULL,
		unresolved INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID`,
	`CREATE TABLE symbols (
		id TEXT PRIMARY KEY,
		path TEXT NOT NULL,
		package TEXT NOT NULL,
		kind TEXT NOT NULL,
		name TEXT NOT NULL,
		qualname TEXT NOT NULL,
		start_line INTEGER NOT NULL,
		end_line INTEGER NOT NULL,
		signature TEXT NOT NULL DEFAULT '',
		doc TEXT NOT NULL DEFAULT '',
		exported INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX symbols_name ON symbols(name)`,
	`CREATE INDEX symbols_qualname ON symbols(qualname)`,
	`CREATE INDEX symbols_path ON symbols(path, start_line)`,
	`CREATE INDEX symbols_package ON symbols(package)`,

	`CREATE TABLE edges (
		source TEXT NOT NULL,
		target TEXT NOT NULL,
		kind TEXT NOT NULL,
		line INTEGER NOT NULL DEFAULT 0,
		confidence REAL NOT NULL DEFAULT 1.0,
		provenance TEXT NOT NULL DEFAULT 'types',
		PRIMARY KEY (source, target, kind, line)
	) WITHOUT ROWID`,
	`CREATE INDEX edges_target ON edges(target, kind)`,
	`CREATE INDEX edges_source ON edges(source, kind)`,

	`CREATE TABLE communities (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		top_symbol TEXT NOT NULL,
		size INTEGER NOT NULL,
		cohesion REAL NOT NULL,
		signature TEXT NOT NULL
	)`,
	`CREATE UNIQUE INDEX communities_signature ON communities(signature)`,
	`CREATE TABLE community_members (
		community_id INTEGER NOT NULL,
		symbol_id TEXT NOT NULL,
		PRIMARY KEY (community_id, symbol_id)
	) WITHOUT ROWID`,
	`CREATE INDEX community_members_symbol ON community_members(symbol_id)`,

	`CREATE VIRTUAL TABLE symbols_fts USING fts5(
		name, qualname, signature, doc,
		content='symbols', content_rowid='rowid',
		tokenize='unicode61 remove_diacritics 2')`,
}

// dropStatements removes the store in dependency order. Shadow FTS tables are
// removed with the virtual table.
var dropStatements = []string{
	`DROP TABLE IF EXISTS symbols_fts`,
	`DROP TABLE IF EXISTS community_members`,
	`DROP TABLE IF EXISTS communities`,
	`DROP TABLE IF EXISTS edges`,
	`DROP TABLE IF EXISTS symbols`,
	`DROP TABLE IF EXISTS files`,
	`DROP TABLE IF EXISTS meta`,
}

// schemaTables are the tables a complete store must contain.
var schemaTables = []string{"meta", "files", "symbols", "edges", "communities", "community_members", "symbols_fts"}

// ensureSchema creates the store when it is missing and rebuilds it when the
// schema or extractor version moved. It is a no-op for an up-to-date store.
func (i *Index) ensureSchema(ctx context.Context) error {
	if !i.needsRebuild {
		ok, err := i.hasSchema(ctx)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return i.rebuildSchema(ctx)
}

// ensureSchemaIfNeeded rebuilds only when Open detected a mismatch. A writer
// that has not opened the meta table yet still creates the store.
func (i *Index) ensureSchemaIfNeeded(ctx context.Context) error {
	if i.needsRebuild {
		return i.rebuildSchema(ctx)
	}
	ok, err := i.hasSchema(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return i.rebuildSchema(ctx)
	}
	return nil
}

// rebuildSchema drops every table and recreates the current schema.
func (i *Index) rebuildSchema(ctx context.Context) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("codeindex: begin schema rebuild: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range dropStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("codeindex: drop graph tables: %w", err)
		}
	}
	for _, statement := range schemaStatements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("codeindex: create graph tables: %w", err)
		}
	}
	now := i.now().UnixMilli()
	values := map[string]string{
		metaSchema:     strconv.Itoa(SchemaVersion),
		metaExtractor:  strconv.Itoa(ExtractorVersion),
		metaEngine:     Engine,
		metaIndexedAt:  strconv.FormatInt(now, 10),
		metaState:      stateEmpty,
		metaUnresolved: "0",
	}
	if i.project != "" {
		values[metaProject] = i.project
	}
	if i.module != "" {
		values[metaModule] = i.module
	}
	if err := setMeta(ctx, tx, values); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("codeindex: commit schema rebuild: %w", err)
	}
	i.needsRebuild = false
	return nil
}

// hasSchema reports whether every table of the current schema exists.
func (i *Index) hasSchema(ctx context.Context) (bool, error) {
	var count int
	query := `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN (` +
		placeholders(len(schemaTables)) + `)`
	if err := i.db.QueryRowContext(ctx, query, stringsToAny(schemaTables)...).Scan(&count); err != nil {
		return false, fmt.Errorf("codeindex: inspect schema: %w", err)
	}
	return count == len(schemaTables), nil
}

// readMeta reads every meta key. A missing meta table yields an empty map.
func (i *Index) readMeta(ctx context.Context) (map[string]string, error) {
	var exists int
	if err := i.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'`).Scan(&exists); err != nil {
		return nil, fmt.Errorf("codeindex: inspect meta table: %w", err)
	}
	if exists == 0 {
		return map[string]string{}, nil
	}
	rows, err := i.db.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return nil, fmt.Errorf("codeindex: read meta: %w", err)
	}
	defer func() { _ = rows.Close() }()
	meta := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("codeindex: read meta: %w", err)
		}
		meta[key] = value
	}
	return meta, rows.Err()
}

// setMeta writes keys inside an existing transaction.
func setMeta(ctx context.Context, tx *sql.Tx, values map[string]string) error {
	for key, value := range values {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO meta(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return fmt.Errorf("codeindex: write meta %s: %w", key, err)
		}
	}
	return nil
}

// metaInt parses a meta integer, returning zero for anything else.
func metaInt(meta map[string]string, key string) int {
	value, err := strconv.Atoi(strings.TrimSpace(meta[key]))
	if err != nil {
		return 0
	}
	return value
}

// metaInt64 parses a meta 64-bit integer, returning zero for anything else.
func metaInt64(meta map[string]string, key string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(meta[key]), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// stateEmpty and stateComplete are the index_state values of the native store.
const (
	stateEmpty    = "empty"
	stateComplete = "complete"
)
