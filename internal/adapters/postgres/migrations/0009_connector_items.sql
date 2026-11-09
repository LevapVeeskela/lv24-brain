-- 0009_connector_items.sql - raw-zone items and checkpoints (contracts-v1).
-- Forward-only migration: there is intentionally no down migration. Every
-- statement is idempotent so a partially applied run can be repeated.
--
-- connector_items holds normalized external items keyed by Item.Key()
-- ("<connection_id>/<40 hex>"). A tombstone keeps its row with deleted = true
-- and an empty body. project_id is '' for unmapped items; it has no foreign
-- key because items may map to projects created later, and search only shows
-- items whose project exists (and the caller may read). item_key and stream
-- use the "C" collation so ORDER BY matches the byte order of the keys.

CREATE TABLE IF NOT EXISTS connector_items (
    connection_id text NOT NULL,
    item_key text COLLATE "C" NOT NULL,
    external_id text NOT NULL,
    kind text NOT NULL DEFAULT '',
    title text NOT NULL DEFAULT '',
    body text NOT NULL DEFAULT '',
    url text NOT NULL DEFAULT '',
    author text NOT NULL DEFAULT '',
    labels text[] NOT NULL DEFAULT '{}',
    acl text[] NOT NULL DEFAULT '{}',
    project_id text NOT NULL DEFAULT '',
    created_at timestamptz,
    updated_at timestamptz,
    observed_at timestamptz,
    hash text NOT NULL,
    deleted boolean NOT NULL DEFAULT false,
    stored_at timestamptz NOT NULL DEFAULT now(),
    search_tsv tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('simple', coalesce(body, '') || ' ' || coalesce(url, '') || ' ' ||
            coalesce(author, '') || ' ' || brain_search_join(labels)), 'B')
    ) STORED,
    PRIMARY KEY (connection_id, item_key),
    CONSTRAINT connector_items_key_check CHECK (left(item_key, length(connection_id) + 1) = connection_id || '/'),
    CONSTRAINT connector_items_hash_check CHECK (hash LIKE 'sha256:%'),
    CONSTRAINT connector_items_tombstone_check CHECK (NOT deleted OR body = '')
);
CREATE INDEX IF NOT EXISTS connector_items_project_idx ON connector_items (project_id) WHERE NOT deleted;
CREATE INDEX IF NOT EXISTS connector_items_search_tsv_idx ON connector_items USING GIN (search_tsv);

CREATE TABLE IF NOT EXISTS connector_checkpoints (
    connection_id text NOT NULL,
    stream text COLLATE "C" NOT NULL,
    version integer NOT NULL,
    data bytea NOT NULL DEFAULT '\x'::bytea,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (connection_id, stream),
    CONSTRAINT connector_checkpoints_version_check CHECK (version >= 1),
    CONSTRAINT connector_checkpoints_data_check CHECK (octet_length(data) <= 65536)
);

-- Item-level runs (ports.Source) record their connection and RunStats.
-- Legacy runs keep both NULL.
ALTER TABLE connector_runs ADD COLUMN IF NOT EXISTS connection_id text;
ALTER TABLE connector_runs ADD COLUMN IF NOT EXISTS stats jsonb;
CREATE INDEX IF NOT EXISTS connector_runs_connection_started_idx
    ON connector_runs (connection_id, started_at DESC, id) WHERE connection_id IS NOT NULL;
