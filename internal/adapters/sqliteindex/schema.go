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
		content_updated_at INTEGER NOT NULL DEFAULT 0,
		valid_from INTEGER NOT NULL DEFAULT 0,
		valid_to INTEGER,
		superseded_by TEXT,
		supersedes TEXT,
		status TEXT NOT NULL DEFAULT '',
		confidence REAL NOT NULL DEFAULT 0,
		subtype TEXT NOT NULL DEFAULT '',
		scope TEXT NOT NULL DEFAULT 'project',
		topic_key TEXT NOT NULL DEFAULT '',
		occurrences INTEGER NOT NULL DEFAULT 1,
		pinned INTEGER NOT NULL DEFAULT 0,
		origin TEXT NOT NULL DEFAULT '',
		trust TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS documents_project_idx ON documents(project_id)`,
	`CREATE INDEX IF NOT EXISTS documents_valid_idx ON documents(kind, valid_to)`,
	`CREATE INDEX IF NOT EXISTS documents_superseded_idx ON documents(superseded_by)`,
	`CREATE INDEX IF NOT EXISTS documents_subtype_idx ON documents(project_id, subtype)`,
	`CREATE INDEX IF NOT EXISTS documents_scope_idx ON documents(project_id, scope)`,
	`CREATE INDEX IF NOT EXISTS documents_topic_idx ON documents(project_id, kind, topic_key)`,
	// relations is the derived projection of the typed relation frontmatter.
	// source_id is the project-scoped document key ("<project>/<record>") so a
	// source can be joined to its document; the relation target stays a record
	// id, because relations never cross projects.
	`CREATE TABLE IF NOT EXISTS relations (
		source_id TEXT NOT NULL,
		project_id TEXT NOT NULL,
		target_id TEXT NOT NULL,
		relation TEXT NOT NULL,
		status TEXT NOT NULL,
		confidence REAL NOT NULL DEFAULT 0,
		reason TEXT NOT NULL DEFAULT '',
		judged_at INTEGER,
		verdict_by TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (source_id, target_id, relation)
	)`,
	`CREATE INDEX IF NOT EXISTS relations_project_idx ON relations(project_id)`,
	`CREATE INDEX IF NOT EXISTS relations_target_idx ON relations(project_id, target_id)`,
	`CREATE INDEX IF NOT EXISTS relations_status_idx ON relations(project_id, status)`,
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

const upsertSQL = `INSERT INTO documents (id, project_id, kind, title, body, updated_at, content_updated_at, valid_from, valid_to, superseded_by, supersedes, status, confidence, subtype, scope, topic_key, occurrences, pinned, origin, trust)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	project_id = excluded.project_id,
	kind = excluded.kind,
	title = excluded.title,
	body = excluded.body,
	updated_at = excluded.updated_at,
	content_updated_at = excluded.content_updated_at,
	valid_from = excluded.valid_from,
	valid_to = excluded.valid_to,
	superseded_by = excluded.superseded_by,
	supersedes = excluded.supersedes,
	status = excluded.status,
	confidence = excluded.confidence,
	subtype = excluded.subtype,
	scope = excluded.scope,
	topic_key = excluded.topic_key,
	occurrences = excluded.occurrences,
	pinned = excluded.pinned,
	origin = excluded.origin,
	trust = excluded.trust`

const deleteSQL = `DELETE FROM documents WHERE id = ?`

const deleteRelationsSQL = `DELETE FROM relations WHERE source_id = ?`

const upsertRelationSQL = `INSERT INTO relations (source_id, project_id, target_id, relation, status, confidence, reason, judged_at, verdict_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source_id, target_id, relation) DO UPDATE SET
	project_id = excluded.project_id,
	status = excluded.status,
	confidence = excluded.confidence,
	reason = excluded.reason,
	judged_at = excluded.judged_at,
	verdict_by = excluded.verdict_by`

// schemaVersion is stored in SQLite's user_version. Bump it when the document
// id scheme or the table layout changes: the index is derived data, so a
// mismatch drops it and the caller reindexes from the Markdown sources.
// Version 2 scopes record ids by project ("<projectID>/<recordID>").
// Version 3 adds temporal validity (valid_from, valid_to, superseded_by,
// supersedes) and its indexes.
// Version 4 adds the ranking inputs (decision status, author confidence).
// Version 5 adds the derived relations table for listings and backlinks.
// Version 6 adds the engram-compatible classification (subtype, scope,
// topic_key, occurrences) and its filters.
// Version 7 adds the pinned flag used by the pin ranking signal.
// Version 8 adds content_updated_at, the last text change: --since compares it
// so a review, snooze or validity write does not re-report the whole corpus.
// Version 9 projects the record provenance (origin, trust) so the ranking trust
// term scores the record instead of staying neutral; rows written before it
// keep empty values and rank as unknown trust.
const schemaVersion = 9

// dropStatements remove everything schemaStatements creates. Triggers go
// first because they reference the tables.
var dropStatements = []string{
	`DROP TRIGGER IF EXISTS documents_after_insert`,
	`DROP TRIGGER IF EXISTS documents_after_delete`,
	`DROP TRIGGER IF EXISTS documents_after_update`,
	`DROP TABLE IF EXISTS relations`,
	`DROP TABLE IF EXISTS documents_fts`,
	`DROP TABLE IF EXISTS documents`,
}
