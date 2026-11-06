-- 0005_knowledge_owner.sql - record the owner of knowledge records.
-- Forward-only migration: there is intentionally no down migration.
--
-- Members may update or delete only the notes, decisions, attempts and
-- handoffs they created; admins may change any. created_by holds the creating
-- user's id (stable across login renames). It is set on insert and never
-- updated.
--
-- Backfill: the creating actor's login is taken from the earliest
-- "<kind>.create" audit event and mapped to a user id. Rows with no matching
-- event or user keep '' (unknown owner), which only admins may change. Before
-- this release the server did not propagate the actor into Postgres, so many
-- existing rows legitimately stay ''.

ALTER TABLE notes ADD COLUMN IF NOT EXISTS created_by text NOT NULL DEFAULT '';
ALTER TABLE decisions ADD COLUMN IF NOT EXISTS created_by text NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN IF NOT EXISTS created_by text NOT NULL DEFAULT '';
ALTER TABLE handoffs ADD COLUMN IF NOT EXISTS created_by text NOT NULL DEFAULT '';

WITH creators AS (
    SELECT DISTINCT ON (a.entity_kind, a.entity_id) a.entity_kind, a.entity_id, u.id AS user_id
    FROM audit_events a
    JOIN users u ON u.login = a.actor
    WHERE a.action IN ('note.create', 'decision.create', 'attempt.create', 'handoff.create')
      AND a.actor <> ''
    ORDER BY a.entity_kind, a.entity_id, a.id
)
UPDATE notes t SET created_by = c.user_id
FROM creators c
WHERE c.entity_kind = 'note' AND c.entity_id = t.id AND t.created_by = '';

WITH creators AS (
    SELECT DISTINCT ON (a.entity_kind, a.entity_id) a.entity_kind, a.entity_id, u.id AS user_id
    FROM audit_events a
    JOIN users u ON u.login = a.actor
    WHERE a.action = 'decision.create' AND a.actor <> ''
    ORDER BY a.entity_kind, a.entity_id, a.id
)
UPDATE decisions t SET created_by = c.user_id
FROM creators c
WHERE c.entity_id = t.id AND t.created_by = '';

WITH creators AS (
    SELECT DISTINCT ON (a.entity_kind, a.entity_id) a.entity_kind, a.entity_id, u.id AS user_id
    FROM audit_events a
    JOIN users u ON u.login = a.actor
    WHERE a.action = 'attempt.create' AND a.actor <> ''
    ORDER BY a.entity_kind, a.entity_id, a.id
)
UPDATE attempts t SET created_by = c.user_id
FROM creators c
WHERE c.entity_id = t.id AND t.created_by = '';

WITH creators AS (
    SELECT DISTINCT ON (a.entity_kind, a.entity_id) a.entity_kind, a.entity_id, u.id AS user_id
    FROM audit_events a
    JOIN users u ON u.login = a.actor
    WHERE a.action = 'handoff.create' AND a.actor <> ''
    ORDER BY a.entity_kind, a.entity_id, a.id
)
UPDATE handoffs t SET created_by = c.user_id
FROM creators c
WHERE c.entity_id = t.id AND t.created_by = '';
