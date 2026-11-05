-- 0001_init.sql - LV24 Brain server-mode schema.
-- Forward-only migration: there is intentionally no down migration.

CREATE TABLE IF NOT EXISTS roles (
    name text PRIMARY KEY,
    description text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT roles_name_check CHECK (name IN ('admin', 'member', 'viewer'))
);

INSERT INTO roles (name, description) VALUES
    ('admin', 'server administration, users, roles, connectors, backups'),
    ('member', 'write shared knowledge and run own sync'),
    ('viewer', 'read, search and export')
ON CONFLICT (name) DO NOTHING;

CREATE TABLE IF NOT EXISTS revisions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind text NOT NULL DEFAULT 'server',
    actor text NOT NULL DEFAULT '',
    entity_kind text NOT NULL DEFAULT '',
    entity_id text NOT NULL DEFAULT '',
    detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT revisions_kind_check CHECK (kind IN ('git', 'server'))
);
CREATE INDEX IF NOT EXISTS revisions_created_at_idx ON revisions (created_at DESC, id);
CREATE INDEX IF NOT EXISTS revisions_entity_idx ON revisions (entity_kind, entity_id);

CREATE TABLE IF NOT EXISTS users (
    id text PRIMARY KEY,
    login text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    role text NOT NULL DEFAULT 'member',
    auth_source text NOT NULL DEFAULT 'local',
    password_hash text NOT NULL DEFAULT '',
    oidc_issuer text NOT NULL DEFAULT '',
    oidc_subject text NOT NULL DEFAULT '',
    revision bigint NOT NULL REFERENCES revisions (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_login_check CHECK (btrim(login) <> ''),
    CONSTRAINT users_role_check CHECK (role IN ('admin', 'member', 'viewer')),
    CONSTRAINT users_role_fk FOREIGN KEY (role) REFERENCES roles (name) ON UPDATE CASCADE,
    CONSTRAINT users_oidc_pair_check CHECK (
        (oidc_issuer = '' AND oidc_subject = '')
        OR (oidc_issuer <> '' AND oidc_subject <> '')
    )
);
CREATE UNIQUE INDEX IF NOT EXISTS users_login_lower_uidx ON users (lower(login));
CREATE UNIQUE INDEX IF NOT EXISTS users_oidc_uidx ON users (oidc_issuer, oidc_subject) WHERE oidc_subject <> '';

CREATE TABLE IF NOT EXISTS projects (
    id text PRIMARY KEY,
    name text NOT NULL,
    paths text[] NOT NULL DEFAULT '{}',
    aliases text[] NOT NULL DEFAULT '{}',
    kind text NOT NULL DEFAULT 'other',
    repo_url text NOT NULL DEFAULT '',
    default_branch text NOT NULL DEFAULT '',
    revision bigint NOT NULL REFERENCES revisions (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT projects_name_check CHECK (btrim(name) <> ''),
    CONSTRAINT projects_kind_check CHECK (kind IN ('app', 'service', 'repository', 'infra', 'other'))
);
CREATE INDEX IF NOT EXISTS projects_name_lower_idx ON projects (lower(name));

CREATE TABLE IF NOT EXISTS notes (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    title text NOT NULL DEFAULT '',
    body text NOT NULL,
    tags text[] NOT NULL DEFAULT '{}',
    permalink text NOT NULL DEFAULT '',
    revision bigint NOT NULL REFERENCES revisions (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notes_body_check CHECK (btrim(body) <> '')
);
CREATE INDEX IF NOT EXISTS notes_project_updated_idx ON notes (project_id, updated_at DESC, id);
CREATE INDEX IF NOT EXISTS notes_updated_idx ON notes (updated_at DESC, id);

CREATE TABLE IF NOT EXISTS decisions (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    title text NOT NULL,
    status text NOT NULL DEFAULT 'proposed',
    decision_context text NOT NULL DEFAULT '',
    alternatives text[] NOT NULL DEFAULT '{}',
    consequences text[] NOT NULL DEFAULT '{}',
    evidence text[] NOT NULL DEFAULT '{}',
    permalink text NOT NULL DEFAULT '',
    revision bigint NOT NULL REFERENCES revisions (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT decisions_title_check CHECK (btrim(title) <> ''),
    CONSTRAINT decisions_status_check CHECK (status IN ('proposed', 'accepted', 'rejected', 'superseded'))
);
CREATE INDEX IF NOT EXISTS decisions_project_updated_idx ON decisions (project_id, updated_at DESC, id);
CREATE INDEX IF NOT EXISTS decisions_updated_idx ON decisions (updated_at DESC, id);

CREATE TABLE IF NOT EXISTS attempts (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    title text NOT NULL,
    hypothesis text NOT NULL DEFAULT '',
    result text NOT NULL DEFAULT '',
    reason text NOT NULL DEFAULT '',
    permalink text NOT NULL DEFAULT '',
    revision bigint NOT NULL REFERENCES revisions (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT attempts_title_check CHECK (btrim(title) <> '')
);
CREATE INDEX IF NOT EXISTS attempts_project_updated_idx ON attempts (project_id, updated_at DESC, id);
CREATE INDEX IF NOT EXISTS attempts_updated_idx ON attempts (updated_at DESC, id);

CREATE TABLE IF NOT EXISTS handoffs (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    summary text NOT NULL,
    git_context text NOT NULL DEFAULT '',
    next_action text NOT NULL DEFAULT '',
    permalink text NOT NULL DEFAULT '',
    revision bigint NOT NULL REFERENCES revisions (id),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT handoffs_summary_check CHECK (btrim(summary) <> '')
);
CREATE INDEX IF NOT EXISTS handoffs_project_created_idx ON handoffs (project_id, created_at DESC, id);
CREATE INDEX IF NOT EXISTS handoffs_created_idx ON handoffs (created_at DESC, id);

CREATE TABLE IF NOT EXISTS project_state (
    project_id text PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    goal text NOT NULL DEFAULT '',
    active text[] NOT NULL DEFAULT '{}',
    blocked text[] NOT NULL DEFAULT '{}',
    next_action text NOT NULL DEFAULT '',
    open_questions text[] NOT NULL DEFAULT '{}',
    fresh_until timestamptz,
    revision bigint NOT NULL REFERENCES revisions (id),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS connector_runs (
    id text PRIMARY KEY,
    source text NOT NULL,
    started_at timestamptz NOT NULL,
    finished_at timestamptz,
    status text NOT NULL DEFAULT 'running',
    counts jsonb NOT NULL DEFAULT '{}'::jsonb,
    error_message text NOT NULL DEFAULT '',
    revision bigint NOT NULL REFERENCES revisions (id),
    CONSTRAINT connector_runs_source_check CHECK (btrim(source) <> ''),
    CONSTRAINT connector_runs_status_check CHECK (status IN ('running', 'success', 'partial', 'failed'))
);
CREATE INDEX IF NOT EXISTS connector_runs_source_started_idx ON connector_runs (source, started_at DESC, id);
CREATE INDEX IF NOT EXISTS connector_runs_started_idx ON connector_runs (started_at DESC, id);

CREATE TABLE IF NOT EXISTS audit_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    revision bigint NOT NULL REFERENCES revisions (id),
    actor text NOT NULL DEFAULT '',
    action text NOT NULL,
    entity_kind text NOT NULL DEFAULT '',
    entity_id text NOT NULL DEFAULT '',
    detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_events_action_check CHECK (btrim(action) <> '')
);
CREATE INDEX IF NOT EXISTS audit_events_created_idx ON audit_events (created_at DESC, id);
CREATE INDEX IF NOT EXISTS audit_events_entity_idx ON audit_events (entity_kind, entity_id);
CREATE INDEX IF NOT EXISTS audit_events_actor_idx ON audit_events (actor, created_at DESC);
