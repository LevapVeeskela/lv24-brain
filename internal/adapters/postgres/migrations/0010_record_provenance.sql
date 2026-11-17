-- 0010_record_provenance.sql - provenance (origin, reviewed, trust) on records.
ALTER TABLE notes
    ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reviewed boolean NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS trust text NOT NULL DEFAULT '';

ALTER TABLE decisions
    ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reviewed boolean NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS trust text NOT NULL DEFAULT '';

ALTER TABLE attempts
    ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reviewed boolean NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS trust text NOT NULL DEFAULT '';

ALTER TABLE handoffs
    ADD COLUMN IF NOT EXISTS origin text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS reviewed boolean NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS trust text NOT NULL DEFAULT '';
