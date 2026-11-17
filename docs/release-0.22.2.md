# Release 0.22.2 вЂ” Postgres migration and provenance fix

Status: released; tag `v0.22.2`.

0.22.2 fixes two server-mode defects that the Postgres integration job caught
after 0.22.1.

## Migration BOM

`0010_record_provenance.sql` started with a UTF-8 BOM. PostgreSQL treats it as
a syntax error before the first SQL token, so a fresh server database failed to
migrate at version 10. The BOM is removed and a unit test (`TestMigrationFilesHaveNoBOM`)
now fails if any migration file starts with one.

## Decision provenance columns

`decisionColumns` still selected the pre-0.23 13-column list, while
`scanDecision` expected the provenance columns added by migration 0010. Reads
and updates of a decision failed with `number of field descriptions must equal
number of destinations, got 13 and 16`. The column list now includes
`origin, reviewed, trust`, matching notes, attempts and handoffs.

## Verification

- `go test -count=1 -p 1 ./internal/adapters/postgres ./internal/adapters/serverstore ./internal/server`
  against PostgreSQL 16 вЂ” green.
- `go vet` and the full unit suite вЂ” green.
- No data-schema change and no migration history rewrite; the checksum of
  migration 0010 changes with the BOM, so an existing server that applied the
  BOM version must be recreated or have its migration checksum refreshed
  deliberately. Fresh CI databases migrate cleanly.

`sync --watch` and the three-machine concurrency proof from 0.22.1 are included
in this build.
