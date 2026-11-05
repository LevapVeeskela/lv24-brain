-- 0002_auth_tokens.sql - durable API tokens and refresh-token rotations.
-- Forward-only migration: there is intentionally no down migration.

CREATE TABLE IF NOT EXISTS api_tokens (
    id text PRIMARY KEY,
    user_id text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name text NOT NULL,
    prefix text NOT NULL DEFAULT '',
    token_hash text NOT NULL,
    role text NOT NULL DEFAULT 'member',
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz,
    CONSTRAINT api_tokens_name_check CHECK (btrim(name) <> ''),
    CONSTRAINT api_tokens_hash_check CHECK (btrim(token_hash) <> ''),
    CONSTRAINT api_tokens_role_check CHECK (role IN ('admin', 'member', 'viewer')),
    CONSTRAINT api_tokens_role_fk FOREIGN KEY (role) REFERENCES roles (name) ON UPDATE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS api_tokens_hash_uidx ON api_tokens (token_hash);
CREATE INDEX IF NOT EXISTS api_tokens_user_created_idx ON api_tokens (user_id, created_at DESC, id);

CREATE TABLE IF NOT EXISTS refresh_tokens (
    token_hash text PRIMARY KEY,
    session_id text NOT NULL,
    family_id text NOT NULL,
    user_id text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    revoked_at timestamptz,
    CONSTRAINT refresh_tokens_session_check CHECK (btrim(session_id) <> ''),
    CONSTRAINT refresh_tokens_family_check CHECK (btrim(family_id) <> ''),
    CONSTRAINT refresh_tokens_hash_check CHECK (btrim(token_hash) <> ''),
    CONSTRAINT refresh_tokens_expiry_check CHECK (expires_at > issued_at)
);
CREATE INDEX IF NOT EXISTS refresh_tokens_session_idx ON refresh_tokens (session_id);
CREATE INDEX IF NOT EXISTS refresh_tokens_family_idx ON refresh_tokens (family_id);
CREATE INDEX IF NOT EXISTS refresh_tokens_expires_idx ON refresh_tokens (expires_at);
