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
		updated_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS documents_project_idx ON documents(project_id)`,
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

const upsertSQL = `INSERT INTO documents (id, project_id, kind, title, body, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	project_id = excluded.project_id,
	kind = excluded.kind,
	title = excluded.title,
	body = excluded.body,
	updated_at = excluded.updated_at`

const deleteSQL = `DELETE FROM documents WHERE id = ?`

const searchSQL = `SELECT d.id, d.project_id, d.kind, d.title,
	snippet(documents_fts, -1, ?, ?, ?, ?), bm25(documents_fts)
FROM documents_fts
JOIN documents d ON d.rowid = documents_fts.rowid
WHERE documents_fts MATCH ?
ORDER BY bm25(documents_fts), d.updated_at DESC, d.id ASC
LIMIT ?`

const searchProjectSQL = `SELECT d.id, d.project_id, d.kind, d.title,
	snippet(documents_fts, -1, ?, ?, ?, ?), bm25(documents_fts)
FROM documents_fts
JOIN documents d ON d.rowid = documents_fts.rowid
WHERE documents_fts MATCH ? AND d.project_id = ?
ORDER BY bm25(documents_fts), d.updated_at DESC, d.id ASC
LIMIT ?`
