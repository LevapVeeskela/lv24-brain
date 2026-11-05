# Migration from the legacy Company Brain

This document describes importing knowledge from a legacy `.brain` directory
into an LV24 Brain filesystem-store data directory.

The importer lives in `internal/migrate` and is exposed as `brain migrate`. It
is idempotent, never overwrites an existing target file, and only touches
knowledge records. It does not copy mail, connector dumps, or journals. Project
registry metadata from the legacy `registry.json` is merged by the CLI after a
successful import.

## What is imported

For every project directory under `<legacy>/projects/<project-id>/`:

| Legacy path | Target path |
|---|---|
| `state.md` | `projects/<project-id>/state.md` |
| `notes/*.md` | `projects/<project-id>/notes/*.md` |
| `decisions/*.md` | `projects/<project-id>/decisions/*.md` |
| `attempts/*.md` | `projects/<project-id>/attempts/*.md` |
| `handoffs/*.md` | `projects/<project-id>/handoffs/*.md` |

Only regular `.md` files are imported. Hidden temp files (`.tmp-*`) and other
extensions are skipped silently. Symlinked project directories and source files
are reported as `failed`, because following a symlink could read or write
outside the selected data directory.

Everything else is intentionally ignored:

- `mail/`
- `connectors/`
- `journal/`
- `global/`, `docs/`, `templates/`, `bin/`
- `secrets/`, `index/`, `backups/`

`registry.json` is not copied verbatim, but its project entries are read to
enrich the target registry (see below).

Copy the remaining non-knowledge parts separately, as described in the product
design, or leave them in the legacy directory.

Every imported file gets data schema version `1` frontmatter with
`project_id`, `kind`, `updated_at`, and `schema_version`. Existing optional
`permalink` values are preserved.

## Safety model

- The source is opened read-only.
- Source and target must be different directories and must not be nested.
- Existing target files are never overwritten:
  - identical bytes are reported as `unchanged`;
  - different bytes are reported as `conflict` and left untouched.
- Re-running the importer is safe: the second pass reports only `unchanged`
  files and creates nothing.
- Project ids and file names are validated; traversal and symlinked ancestors
  are rejected.
- Output is deterministic (sorted projects and files, UTC timestamps), so the
  same source always produces the same target bytes.
- A malformed or unreadable file is reported as `failed`; the remaining files
  are still imported.

## Normalization

| Record | Conversion |
|---|---|
| State | `kind: state`; `updated_at` is preserved; unknown body sections (`Recent decisions`, `Evidence`, ...) remain in the file. |
| Notes | `kind: note`; file `date` is preserved or taken from the file name; each `## HH:MM` section becomes a marked note with a stable id and a timestamp built from the file date and the section time. |
| Decisions | `kind: decision`; `id` is preserved; `created_at` comes from `created_at`, then `valid_from`; status is normalized to `proposed`, `accepted`, `rejected`, or `superseded`; `Context` + `Decision` are kept in the context field; rejected approaches and revisit triggers are folded into alternatives and consequences; `source_refs` and `evidence` become evidence. |
| Attempts | `kind: attempt`; `id` is preserved; `created_at` comes from `created_at`, then `observed_at`; `Symptom` and `What we tried` are kept in the result field; `Outcome / why it failed` becomes `Why it failed`; `What to try next` and legacy `files`/`evidence` metadata are kept in the result. |
| Handoffs | `kind: handoff`; `id` is preserved; `created_at` is preserved; `Objective`, `Completed`, `Unfinished`, `Blockers`, and evidence are kept in the summary; `branch`, `head`, `dirty_files`, `status`, and the `Git log` section become the git context; `Next action` is preserved. |

Legacy frontmatter keys that have no schema-v1 field (for example decision
`confidence`, `valid_to`, `supersedes`, or state `branch`/`head`) are not copied
into frontmatter. Their body content is preserved.

## Before you start

Set two shell variables for the examples below:

```powershell
$legacy = '<workspace>\.brain'
$brainData = '<brain-data>'
```

1. Stop every writer: the legacy `brain.ps1` CLI, agent hooks, scheduled tasks,
   and any editor session that may save files in the legacy directory.
2. Back up the legacy data directory:

   ```powershell
   $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
   Copy-Item -Recurse -LiteralPath $legacy `
     -Destination "<workspace>\brain-backup-$stamp"
   ```

   Or create an archive:

   ```powershell
   Compress-Archive -LiteralPath $legacy `
     -DestinationPath "<workspace>\brain-backup-$stamp.zip"
   ```

3. Prepare the target data directory and put it under version control (or back
   it up) so rollback is easy:

   ```powershell
   Test-Path -LiteralPath $brainData
   git -C $brainData status --short
   ```

4. Make sure the target is a separate directory, for example `<brain-data>`.
   Do not point the importer at the legacy `.brain` directory itself and do not
   nest one inside the other.

## Dry run

Dry run parses and converts every source file, reports what would happen, and
creates nothing (not even the target directory):

```powershell
brain migrate --from $legacy --data-dir $brainData --dry-run
```

Expected report shape:

```text
created   <N>
unchanged 0
conflicts 0
failed    0
```

`conflicts` or `failed` above zero means do not proceed until they are
understood. Use the per-file lines to see the exact source and target paths.

## Import

```powershell
brain migrate --from $legacy --data-dir $brainData
```

The command exits with status `1` when any file conflicted or failed, and with
status `0` only when every file was created or was already unchanged. On a
successful non-dry run it also registers imported projects in `registry.json`
and sets `schema_version: 1` in the target `brain.yaml`.

## Verification

1. Re-run the dry run. It must report zero `created`, zero `conflicts`, zero
   `failed`, and all files as `unchanged`:

   ```powershell
   brain migrate --from $legacy --data-dir $brainData --dry-run
   ```

2. Confirm that only knowledge directories and engine metadata were added:

   ```powershell
   Get-ChildItem -LiteralPath $brainData | Select-Object Name
   git -C $brainData status --short
   ```

   `mail`, `connectors`, `journal`, and legacy registry content must not appear
   as new target paths from this command.

3. Confirm schema-v1 frontmatter:

   ```powershell
   Get-ChildItem -Path (Join-Path $brainData 'projects') `
     -Recurse -Filter '*.md' |
     Select-String -Pattern '^schema_version: 1$' |
     Measure-Object
   ```

   The count must equal the number of imported Markdown files.

4. Compare file counts per record type:

   ```powershell
   foreach ($kind in 'notes', 'decisions', 'attempts', 'handoffs') {
     $legacyCount = @(
       Get-ChildItem -Path "$legacy\projects\*\${kind}" `
         -File -Filter '*.md' -ErrorAction SilentlyContinue
     ).Count
     $targetCount = @(
       Get-ChildItem -Path "$brainData\projects\*\${kind}" `
         -File -Filter '*.md' -ErrorAction SilentlyContinue
     ).Count
     "{0}: legacy={1} target={2}" -f $kind, $legacyCount, $targetCount
   }
   ```

   Counts should match minus any files the report marked as failed.

5. Spot-check a project through the engine:

   ```powershell
   $config = Join-Path $brainData 'brain.yaml'
   brain open --project <project-id> --config $config
   brain bootstrap --project <project-id> --config $config
   brain doctor --config $config
   ```

6. Rebuild the local search index so imported Markdown is searchable:

   ```powershell
   brain index --rebuild --config $config
   ```

7. Scan the imported data for credentials before committing to a private
   remote:

   ```powershell
   git -C $brainData grep -nEi `
     '(api[_-]?key|token|password|passwd|secret|BEGIN (RSA|OPENSSH|EC) PRIVATE KEY)' `
     -- projects
   ```

   Review every match. The importer does not redact secrets because it must not
   guess what is knowledge and what is a credential.

## Rollback

The source directory is never modified, so the legacy `.brain` remains the
fallback until the imported copy is verified.

- If the target is a Git repository and nothing was committed:

  ```powershell
  git -C $brainData status --short
  git -C $brainData clean -fd -- projects
  ```

  Review the dry-run output first: `clean -fd` removes untracked files. If the
  target already had tracked files, conflicts prevented overwriting them, so
  they do not need to be restored.

- If the target is not under Git, restore the pre-import backup of the target,
  or remove only the project directories listed as created in the report. Do
  not delete the whole target directory if it contains other data.

- If the import was committed, revert the import commit:

  ```powershell
  git -C $brainData revert <import-commit>
  ```

- Do not delete the legacy `.brain` directory until the new engine has passed
  the verification above and any required soak period.

## Command behavior

`brain migrate` takes:

| Flag | Meaning |
|---|---|
| `--from <legacy-dir>` | Required legacy `.brain` data directory. |
| `--data-dir <target-dir>` | Target data directory. When omitted, `data_dir` comes from `brain.yaml`. |
| `--config <path>` | Optional `brain.yaml` path, used to resolve the target and updated after a successful run. |
| `--dry-run` | Parse and report without writing anything. |

Exit codes:

- `0`: every file was created or already unchanged.
- `1`: at least one conflict or failed file, or a fatal validation/I/O error.
- `2`: usage error, such as a missing `--from` or an unknown flag.

After a successful non-dry run, the CLI:

- writes `schema_version: 1` into the target `brain.yaml` (creating a minimal
  config when none exists) and preserves a relative `data_dir` such as `.`;
- merges each imported project into `registry.json`: the legacy `path`, `repo`,
  and `branch` fill empty `paths`, `repo_url`, and `default_branch` fields, and
  legacy `kind` values map to `repository`, `app`, `service`, `infra`, or
  `other`. Curated values already present in the target registry are never
  overwritten; a project is rewritten only when a field was filled.

The importer itself does not touch configuration or the registry; that work is
done by the CLI after `migrate.Run` succeeds. If you call the package directly,
set `schema_version` and registry metadata yourself.

## Known limitations

- `brain.yaml` is created or updated by the CLI, not by the importer package.
- `registry.json` is not converted. The CLI registers imported project ids with
  minimal metadata; legacy repository metadata is not preserved.
- Mail, connectors, journal, global conventions, and legacy registry paths are
  not copied by this importer; handle them with the private-data repository
  packaging step.
- The SQLite index must be rebuilt after import: `brain index --rebuild`.
- A file that cannot be represented portably (for example a name containing
  `:`, `<`, `>`, `"`, `|`, `?`, or `*`) is reported as failed; rename it in the
  legacy source and rerun.
- A conflict is never merged. Resolve it by comparing the legacy source and the
  target file, then either keep the target edit or move it aside and rerun.
- Server mode is not a direct target. Import into a Markdown data directory
  first. Server-side Markdown import/export is not wired yet; create records
  through the HTTP API after migration if needed.
