-- 0004_security_audit.sql - allow audit events without a revision.
-- Forward-only migration: there is intentionally no down migration.
--
-- Entity mutations keep writing a revision plus an audit event in one
-- transaction. Security events (logins, logouts, refresh-token reuse, API token
-- lifecycle, OIDC linking) are not entity changes and must not appear in the
-- revision stream that every viewer can read through /v1/revisions and SSE, so
-- they are stored with a NULL revision and are visible to admins only through
-- GET /v1/audit. The foreign key to revisions is unchanged for non-NULL values.

ALTER TABLE audit_events ALTER COLUMN revision DROP NOT NULL;

CREATE INDEX IF NOT EXISTS audit_events_action_idx ON audit_events (action, created_at DESC);
