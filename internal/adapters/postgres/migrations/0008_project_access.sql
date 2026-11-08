-- 0008_project_access.sql - per-project access control and API token scopes.
-- Forward-only migration: there is intentionally no down migration. Every
-- statement is idempotent so a partially applied run can be repeated.
--
-- Compatibility: a project without a project_restrictions row stays open to
-- every authenticated user per their global role, exactly as before. Adding
-- the first member restricts the project (one row here); from then on only
-- its members and global admins can access it. Removing the last member keeps
-- the project restricted (admin-only) until an admin reopens it explicitly, so
-- a membership cleanup or a user deletion never silently opens a project.
--
-- API tokens minted before this release have empty scopes and project_ids,
-- which means "unscoped" (the token carries its role, as before).

CREATE TABLE IF NOT EXISTS project_restrictions (
    project_id text PRIMARY KEY REFERENCES projects (id) ON DELETE CASCADE,
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS project_members (
    project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    user_id text NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id),
    CONSTRAINT project_members_role_check CHECK (role IN ('viewer', 'member', 'maintainer'))
);
CREATE INDEX IF NOT EXISTS project_members_user_idx ON project_members (user_id);

-- Invariant: a project with members is restricted.
INSERT INTO project_restrictions (project_id)
SELECT DISTINCT project_id FROM project_members
ON CONFLICT (project_id) DO NOTHING;

ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS scopes text[] NOT NULL DEFAULT '{}';
ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS project_ids text[] NOT NULL DEFAULT '{}';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'api_tokens_scopes_check'
    ) THEN
        ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_scopes_check
            CHECK (scopes <@ ARRAY['read', 'write', 'admin']::text[]);
    END IF;
END
$$;
