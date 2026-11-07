-- 0007_search_fulltext.sql - PostgreSQL full-text search for server mode.
-- Forward-only migration: there is intentionally no down migration.
--
-- Each searchable table gets a STORED generated tsvector column. Title terms
-- get weight A and body terms weight B, so ts_rank puts title matches first.
-- The 'simple' configuration is used because content mixes Russian and
-- English and has no stemming dictionary for either. Every statement is
-- idempotent (IF NOT EXISTS / OR REPLACE), so re-running the file is safe.

-- array_to_string is only STABLE, which generated columns reject. The text[]
-- columns joined here are never NULL and their output is deterministic, so
-- this wrapper is declared IMMUTABLE.
CREATE OR REPLACE FUNCTION brain_search_join(items text[])
RETURNS text
LANGUAGE sql
IMMUTABLE
AS $$ SELECT coalesce(array_to_string(items, ' '), '') $$;

ALTER TABLE notes ADD COLUMN IF NOT EXISTS search_tsv tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('simple', coalesce(body, '')), 'B')
    ) STORED;

ALTER TABLE decisions ADD COLUMN IF NOT EXISTS search_tsv tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('simple', coalesce(decision_context, '') || ' ' ||
            brain_search_join(alternatives) || ' ' ||
            brain_search_join(consequences) || ' ' ||
            brain_search_join(evidence)), 'B')
    ) STORED;

ALTER TABLE attempts ADD COLUMN IF NOT EXISTS search_tsv tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', coalesce(title, '')), 'A') ||
        setweight(to_tsvector('simple', coalesce(hypothesis, '') || ' ' ||
            coalesce(result, '') || ' ' || coalesce(reason, '')), 'B')
    ) STORED;

ALTER TABLE handoffs ADD COLUMN IF NOT EXISTS search_tsv tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', coalesce(summary, '')), 'A') ||
        setweight(to_tsvector('simple', coalesce(git_context, '') || ' ' ||
            coalesce(next_action, '')), 'B')
    ) STORED;

CREATE INDEX IF NOT EXISTS notes_search_tsv_idx ON notes USING GIN (search_tsv);
CREATE INDEX IF NOT EXISTS decisions_search_tsv_idx ON decisions USING GIN (search_tsv);
CREATE INDEX IF NOT EXISTS attempts_search_tsv_idx ON attempts USING GIN (search_tsv);
CREATE INDEX IF NOT EXISTS handoffs_search_tsv_idx ON handoffs USING GIN (search_tsv);
