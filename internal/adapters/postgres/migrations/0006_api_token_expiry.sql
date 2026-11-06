-- 0006_api_token_expiry.sql - optional expiry for API tokens.
-- Forward-only migration: there is intentionally no down migration.
--
-- expires_at is NULL for tokens that never expire, which is every token minted
-- before this release and every token minted without an expiry. A non-NULL
-- value makes the token behave exactly like a revoked token once it is reached:
-- FindAPITokenByHash reports it as not found. Expired rows are kept so the
-- owner and admins can still list them with their expiry.

ALTER TABLE api_tokens ADD COLUMN IF NOT EXISTS expires_at timestamptz;
