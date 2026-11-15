package sqliteindex

// schemaStatements create the document table, its FTS5 mirror and the triggers
// that keep both in sync. They are idempotent, so opening an existing index is
// safe.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS documents (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		title TEXT NOT NULL,
		body TEXT NOT NULL,
		updated_at INTEGER NOT NULL,
		valid_from INTEGER NOT NULL DEFAULT 0,
		valid_to INTEGER,
		superseded_by TEXT,
		supersedes TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS documents_project_idx ON documents(project_id)`,
	`CREATE INDEX IF NOT EXISTS documents_valid_idx ON documents(kind, valid_to)`,
	`CREATE INDEX IF NOT EXISTS documents_superseded_idx ON documents(superseded_by)`,
	`CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(
		title,
		body,
		content='documents',
		content_rowid='rowid',
		tokenize='unicode61 remove_diacritics 2'
	)`,
	`CREATE TRIGGER IF NOT EXISTS documents_after_insert AFTER INSERT ON documents BEGIN
		INSERT INTO documents_fts(rowid, title, body) VALUES (new.rowid, new.title, new.body);
	END`,
	`CREATE TRIGGER IF NOT EXISTS documents_after_delete AFTER DELETE ON documents BEGIN
		INSERT INTO documents_fts(documents_fts, rowid, title, body)
		VALUES ('delete', old.rowid, old.title, old.body);
	END`,
	`CREATE TRIGGER IF NOT EXISTS documents_after_update AFTER UPDATE ON documents BEGIN
		INSERT INTO documents_fts(documents_fts, rowid, title, body)
		VALUES ('delete', old.rowid, old.title, old.body);
		INSERT INTO documents_fts(rowid, title, body) VALUES (new.rowid, new.title, new.body);
	END`,
}

const upsertSQL = `INSERT INTO documents (id, project_id, kind, title, body, updated_at, valid_from, valid_to, superseded_by, supersedes)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	project_id = excluded.project_id,
	kind = excluded.kind,
	title = excluded.title,
	body = excluded.body,
	updated_at = excluded.updated_at,
	valid_from = excluded.valid_from,
	valid_to = excluded.valid_to,
	superseded_by = excluded.superseded_by,
	supersedes = excluded.supersedes`

const deleteSQL = `DELETE FROM documents WHERE id = ?`

// searchSelectSQL is the FTS5 search prefix. Its four snippet placeholders
// precede the MATCH placeholder; optional filters are appended by
// buildSearchStatement. Values are only ever bound, never interpolated.
const searchSelectSQL = `SELECT d.id, d.project_id, d.kind, d.title,
	snippet(documents_fts, -1, ?, ?, ?, ?), bm25(documents_fts),
	d.valid_from, d.valid_to, d.superseded_by
FROM documents_fts
JOIN documents d ON d.rowid = documents_fts.rowid
WHERE documents_fts MATCH ?`

const searchOrderSQL = `
ORDER BY bm25(documents_fts), d.updated_at DESC, d.id ASC
LIMIT ?`

// schemaVersion is stored in SQLite's user_version. Bump it when the document
// id scheme or the table layout changes: the index is derived data, so a
// mismatch drops it and the caller reindexes from the Markdown sources.
// Version 2 scopes record ids by project ("<projectID>/<recordID>").
// Version 3 adds temporal validity (valid_from, valid_to, superseded_by,
// supersedes) and its indexes.
const schemaVersion = 3

// dropStatements remove everything schemaStatements creates. Triggers go
// first because they reference the tables.
var dropStatements = []string{
	`DROP TRIGGER IF EXISTS documents_after_insert`,
	`DROP TRIGGER IF EXISTS documents_after_delete`,
	`DROP TRIGGER IF EXISTS documents_after_update`,
	`DROP TABLE IF EXISTS documents_fts`,
	`DROP TABLE IF EXISTS documents`,
}
