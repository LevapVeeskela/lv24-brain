# Team runbook

This runbook covers daily use, git-mode sync, hooks, server operations,
upgrades, database migrations, legacy import and rollback, and incident
checklists. It describes the behaviour of this repository. Flag-level
reference is in [quickstart.md](quickstart.md#command-flags). Installation and
deployment details are in [install.md](install.md), [deploy.md](deploy.md),
and [update.md](update.md).

Placeholders: `~/brain-data` is an example data directory (the private data
repository in git mode), `<data-dir>` is the brain data directory of any
instance, and `https://brain.example.test` is an example server URL. Replace
them with your own values. Never commit the data directory or put secrets in
examples.

## 1. Daily use (agents and humans)

### Set the default instance

Once per machine, point the CLI at the data directory's `brain.yaml`:

```sh
brain config use ~/brain-data/brain.yaml
brain config show
brain config clear          # remove the pointer
```

The config is resolved in this order: `--config`, `$BRAIN_CONFIG`,
`./brain.yaml`, then the pointer set by `brain config use`.

### Open a project

```sh
cd <path-to-project-checkout>
brain open .
brain open --brief .
brain bootstrap demo
```

`brain open` prints the bootstrap pack, a `## Git` section, and a `## Graphs`
section. `--brief` prints only the project and state fields. The selector is an
id, an alias, or a path. Without one, the project is resolved from the working
directory: the longest registered path containing it, else the single
registered project under it. Use `brain project list` to see the registry.
A write to an unknown project fails unless `--create` is passed (for example
`brain note --create --project demo "..."`), which registers it; without the
flag a typo or unregistered alias never starts a new project. `brain project set`
makes the metadata of a registered project explicit.

### Write records

```sh
brain note "Fixed the cache warmup"
brain decision "Adopt the Go engine" --status accepted \
  --context "Single binary for all platforms" \
  --alternatives "keep the PowerShell wrapper,rewrite in Rust" \
  --consequences "Legacy scripts are retired"
brain attempt "Pin the old driver" --hypothesis "Old driver is compatible" \
  --result "Startup fails" --reason "Driver removed the flag"
brain handoff "Session summary" --next "Run the migration dry run"
```

- Positional text is the body for `note`, and the title for `decision` and
  `attempt`. A `note` title is the first line of its text, at most 80
  characters. Do not combine positional text with `--title` or `--body`.
- `--alternatives`, `--consequences`, and `--evidence` are comma-separated
  lists. Text with commas must go into `--context` or the body.
- `brain handoff` records git context read from the project checkout. Use
  `--no-git` to skip it or `--git-context` to supply it.
- Records are Markdown files. In git mode they are shared with everyone who
  can read the data repository. Do not write secrets, tokens, or personal data
  into records.

Decisions about specific code should name it with `--paths` (comma-separated,
repo-relative files or directories with forward slashes, for example
`--paths internal/wire/,cmd/brain/main.go`). Absolute paths and `..` segments
are rejected. The same field is the `paths` array of `brain_decision`. When a
project has a local checkout on the machine that runs `brain review`, the review
reports each accepted or proposed decision whose paths changed in git after the
decision was last updated (`decision-code-changed`). It is report-only: confirm
the decision still holds or supersede it. Machines without the checkout, and
projects that are not git repositories, are skipped silently.

### Record lifecycle (local and git mode)

```sh
brain record list [project] --kind decision --status proposed --since 7d
brain record show <id>
brain record status <id> accepted          # proposed|accepted|rejected|superseded
brain record supersede <old-id> --by <new-id>
brain record trash <id> --yes              # moves it to projects/<p>/trash/
brain record trash-list [project]
brain record restore <id>
```

- Changes are journaled (`status`, `trash`, `restore`) and update the search
  index at once. Trash is reversible and lives in the data repository, so it
  syncs like any record. Notes written by hand without an id marker cannot be
  trashed; edit the daily file instead.
- Agents get `brain_records`, `brain_record` and `brain_decision_status` over
  MCP; trash and restore stay CLI-only.

### Links between records (since v0.8.0)

Reference another record by writing `[[<id>]]`, `[[<id>|text]]` or, across
projects, `[[<project>/<id>]]` in a note body, decision context, alternatives,
consequences or evidence, attempt hypothesis, result or reason, or handoff
summary or next action. Links inside inline code and code fences are ignored.

```sh
brain note "Rolled back, see [[decision-lv24-20261108-101500|the cache decision]]"
brain record show <id>                 # adds Links and Backlinks sections
brain links <id> [--project p] [--json]
brain links --broken [--project p]     # references to missing records or projects
```

Backlinks are computed on read from the same project's records; nothing is
stored and no migration is needed. MCP `brain_record` returns `links` (state
found, missing or unavailable, with kind, title and status) and `backlinks`.

### Review hygiene: broken links and near-duplicates (since v0.8.0)

- `broken-link`: a `[[...]]` reference to a record or project that does not
  exist. Fix the text by hand; there is no automatic fix.
- `near-duplicate`: two records of the same kind in one project with nearly the
  same text (3-word shingle similarity of at least 80%, or the same title and a
  body of at least 60%; `[auto]` handoffs compare only the summary, at 90%).
  The older id comes first. Keep one: for decisions
  `brain record supersede <old> --by <new>`, otherwise
  `brain record trash <id> --yes` (undo with `brain record restore <id>`). Only
  the 500 most recent records per kind are compared; trashed, rejected and
  superseded records are ignored. `brain doctor` shows a `duplicates` check
  with counts per project.

Both are report-only: `brain review --fix` never touches them.

### Journal and search

```sh
brain journal --since 24h --limit 50
brain journal --project demo --event decision
brain search "cache warmup" --project demo --limit 10
brain index                 # refresh the local index after Markdown changed outside the CLI
brain journal --repair --dry-run   # records of the last 30 days without a journal line
```

- `brain journal` shows the per-user activity file `journal/<user>.jsonl`,
  newest first. It works in local and git mode. A retried write is journaled
  once. When a journal append failed after a record was stored, `brain
  doctor` (check `journal`) reports it and `brain journal --repair` appends
  the missing lines, timed at the record's creation and marked `repaired`.
- `brain index` also drops index entries whose records are gone (deleted,
  renamed or quarantined on another machine) and reports them as `pruned`.
- `brain search` queries the local FTS5 index in local and git mode and calls
  `GET /v1/search` in server mode. It searches curated records by default;
  `--dumps` adds legacy connector dumps (mail, GitHub/GitLab issues and PRs),
  `--kind item` adds raw-zone items (excluded by default), and
  `--kind note,decision` (or `--kind dump`) filters by kind. GitHub/GitLab
  dumps are indexed under the registered project whose `repo_url` they
  mirror, so `brain search --project ims --kind dump "flaky"` finds that
  repository's issues. Connector runs index their dumps; item runs update the
  per-item index (`brain index` prints `items`). Server mode applies
  `--kind` too (Postgres full-text search, ranked); raw items join only when
  asked for and only when mapped to an existing brain project.
- Local search (FTS5) matches each query term of 4 or more characters by
  prefix, so `warm` finds `warmup`; shorter terms match exactly. It
  first requires all terms; when that finds nothing, it falls back to records
  that match any term.
- Server mode also supports `brain record list|show|status|supersede`,
  `catchup`, `review` and `journal --repair` reads; trash and restore are
  local/git only.
- Writes made through the CLI update the local index, and `brain sync`
  reindexes after pulling records from another machine. Markdown changed by
  hand needs `brain index` (or `brain index --rebuild` to rebuild it from
  Markdown).

### Session routine for agents

1. `brain open .` at the start, and `brain sync` first in git mode.
2. Record decisions and failed attempts when they happen, not at the end.
3. `brain handoff "<summary>" --next "<next action>"` at the end.
4. In git mode, run `brain sync --push` after the handoff (see section 2).

## 2. Git-mode sync routine

The data repository is private and holds the shared brain. The sync commands
are available in git mode only; server mode uses the API instead.

```sh
brain sync --status --config ~/brain-data/brain.yaml   # read-only: ahead, behind, changed, conflicts
brain sync --config ~/brain-data/brain.yaml            # commit local changes, then pull (rebase)
brain sync --config ~/brain-data/brain.yaml --push     # the same, then push
brain index --config ~/brain-data/brain.yaml           # after pulled Markdown changed
```

What `brain sync` does, in order:

1. Stops with an error and changes nothing if conflicts are already reported.
2. Commits local changes when the working tree is dirty. The message is
   `brain sync: <timestamp>`.
3. Fetches and rebases onto the upstream branch.
4. Pushes only with `--push`.

It never force-pushes and never resolves conflicts automatically.
`--status` never changes anything.

### Conflicts

`brain sync --status` lists conflicting paths as `conflict: <path>` lines.
`brain sync` refuses to run while they exist, and reports
`unresolved conflicts in <paths>`.

To resolve:

1. Inspect the repository: `git -C ~/brain-data status`.
2. Edit the conflicting Markdown files by hand. Keep the content that is still
   correct. When both sides hold different knowledge, keep both as separate
   records (for example with `brain note`).
3. Stage the files with `git add`. Then finish the rebase with
   `git rebase --continue`, or with `git commit` if no rebase is in progress.
4. Run `brain sync --status` until it reports no conflicts, then `brain sync --push`.

Do not use `git reset --hard`, do not take one side of a whole directory with
`--ours` or `--theirs`, and do not force-push. If a conflict cannot be resolved
safely, stop and ask the maintainer. While a rebase is still in progress,
`git rebase --abort` returns the branch to its state before the rebase.

## 3. Hooks

The git hook records each commit in the journal. It is installed per clone, in
the repository's hooks directory (`.git/hooks` in a normal clone), and is not
committed.

```sh
brain hooks install --repo <path-to-project-checkout> --config ~/brain-data/brain.yaml
brain hooks status --repo <path-to-project-checkout>
brain hooks uninstall --repo <path-to-project-checkout>
```

- `install` flags: `--repo <path>` (default `.`), `--force`, and
  `--config <brain.yaml>`. The config path is recorded in the hook. Without
  `--config`, it falls back to `$BRAIN_CONFIG`, and if that is unset the path
  is omitted.
- `status` and `uninstall` accept `--repo <path>`.
- The hook runs `brain event commit --quiet --repo <top-level>`.
- A post-commit hook that was not written by lv24-brain is not overwritten
  unless `--force` is given. With `--force` it is moved to a numbered
  `post-commit.lv24-brain-backup` file. Existing backups are never overwritten.
- Hook activity is read with `brain journal --event commit`.

## 3a. Connectors, code graphs, and project discovery

Local and git modes take connector targets from the brain data, so a scheduled
refresh needs no environment variables:

```sh
brain connector run --all --days 7          # github, gitlab, every mail account
brain connector run --name imap --account <id> --days 2
brain graphs --changed-only                 # graphify update / codegraph sync
brain graphs --dry-run                      # show what would run
brain discover --root <workspace>           # dry run: list new checkouts
brain discover --root <workspace> --apply   # register the new ones
```

- GitHub repositories come from registered projects' `repo_url`; GitLab
  projects from `repo_url` and the `gitlab`/`origin` remotes of registered
  checkouts; mail accounts from `<data-dir>/connectors/mail.json` (account id,
  host, port, username, folders; folder names may be stored in IMAP modified
  UTF-7). `BRAIN_GITHUB_REPOS`, `BRAIN_GITLAB_PROJECTS` and `BRAIN_IMAP_*`
  still override these defaults.
- Secrets: `gitlab/token`, `imap/<account>/password` in the OS store
  (`brain secret set <name>` reads stdin). Without `github/token`, the token
  is read from `gh auth token` at run time when the GitHub CLI is signed in;
  it is never stored.
- Each run is bounded to five minutes and a failing account does not stop the
  others; the command exits 1 if any run failed.
- Named connections (v0.9.0, local/git only):
  `brain connection add github --id <id> --setting repos=o/r`,
  `brain connection add gitlab --id <id> --setting projects=g/p`,
  `brain connection add imap --id <id> --account <mail-id> --base-url imaps://host[:port] --setting username=... [--setting folders=...]`;
  secrets via stdin (`brain connection secret set <id> token|password`,
  stored as `conn/<id>/<field>`); `brain connection run <id> [--since RFC3339]
  [--days N] [--limit N] [--json]`; `brain connector run --all` prefers the catalogue
  when non-empty and falls back to the legacy targets above when empty.
  `--name` stays legacy snapshot (Markdown dumps); it is not native
  item-level. Deletes are never inferred.
- OAuth (v0.12.0, in development): `brain connection auth <id>` runs GitHub's
  device flow or GitLab's PKCE loopback flow (port 47615) using a public client
  ID from `--client-id` or `BRAIN_OAUTH_CLIENT_ID`. The bundle is stored only
  in the OS secret store (`conn/<id>/oauth` plus token-free metadata), never in
  `connections.json`. An OAuth session takes precedence over a PAT for `token`
  lookups; `--remove` deletes it and falls back to the PAT/token path. Refresh
  rotation is persisted before the new access token is used.
- Phase 1 native providers (v0.10.0, local/git only) are registered next to
  GitHub/GitLab/IMAP: `folders` (`paths`, optional `extensions`/`ignore`; no
  credentials; Obsidian/Logseq text is preserved), `jira` (`token` secret,
  optional `email`/`deployment`/`projects`/`jql`), `confluence` (`token`
  secret, optional `email`/`deployment`/`spaces`/`cql`), `linear` (`token`
  secret, optional `teams`/`authscheme`), `slack` (`token` secret,
  `channels` required, optional `threads`/`limit`; DMs and private channels
  are never discovered), `notion` (`token` secret, optional
  `databases`/`pages`/`version`), `sentry` (`token` secret, `org` required,
  optional `projects`/`environment`; at most three newest events per issue),
  `postgres-schema` (`host`/`database`/`user` + `password` secret, optional
  `port`/`sslmode`/`includesystem`), `sqlite-schema` (absolute `path`, optional
  `includesystem`), `rss` (`urls`, optional `limit`) and `web` (`sitemaps`,
  optional `maxpages`/`allowhosts`/`respectrobots`). Add/run them with
  `brain connection add|run`; `brain connector run --name` still accepts only
  `github`, `gitlab`, `imap`. The database sources read catalogs only and the
  web source never follows redirects or leaves the sitemap's host/port unless
  `allowhosts` permits it.
- The Phase 3 `mcp` source (v0.12.0, local/git only) connects to MCP servers:
  `transport=stdio` with `command` and newline-separated `args` stored as local
  credentials (never in the synced catalogue), or `transport=http` with an
  https `url` (loopback http for tests) and an optional `token` secret.
  Optional `tools` is an exact read-tool allow-list; without it the source
  selects names containing search/list/recent/query/find and always denies the
  destructive verbs or a destructive annotation. `maxitems` (default 100, max
  500), `query` (default `*`) and `timeout` (default 30s, max 5m) are bounded.
  Results are marked recent-only, and stdio commands inherit the agent
  environment: run only trusted servers.
- `brain graphs` runs `graphify` and `codegraph` only where their indexes
  already exist (`--init` creates missing ones), skips missing tools, and logs
  tool output to `<user cache dir>/lv24-brain/graphs.log`, outside the data
  repository.
- `brain discover` scans only the immediate children of `--root` (a `.git`
  directory or file, or `main/.git`), never modifies existing projects, and
  reports id conflicts instead of registering them.

## 3a2. MCP server for coding agents

`brain mcp` serves the brain over stdio (MCP 2025-06-18) with these tools:

- `brain_open` - load a project's state, recent decisions, attempts and last handoff.
- `brain_context` - context for one task: state, review findings, latest handoff, best-matching decisions/attempts/notes and failed attempts to avoid (`query`, optional `project`, `budget`, `kinds`, `since`).
- `brain_search` - full-text search over notes, decisions, attempts and handoffs (English and Russian word forms match; optional `since`); connector dumps only with `include_dumps` or `kinds` containing `dump`.
- `brain_note` - append a dated note.
- `brain_decision` - record a decision with its context, alternatives and consequences.
- `brain_attempt` - record an attempt with its result and reason.
- `brain_handoff` - record a session handoff with a summary and next action.
- `brain_journal` - list recent activity, newest first.
- `brain_projects` - list registered projects with kind, paths and repository URL.
- `brain_state_get` - read a project's goal, next action, active work, blockers and open questions.
- `brain_state_set` - update the state; only the given fields change.
- `brain_catchup` - what changed since the latest handoff (or the last `since_hours`).
- `brain_review` - find stale or contradictory knowledge; read-only, `brain review --fix` applies fixes in a shell.
- `brain_records` - list a project's records with ids and status, newest first.
- `brain_record` - read one record in full by id.
- `brain_decision_status` - accept, reject or supersede a decision.

Register it once per machine with an explicit config, because
agents may start servers without the variables that locate the default config:

```sh
claude mcp add --scope user lv24-brain -- <brain> mcp --config <data-dir>/brain.yaml
codex mcp add lv24-brain -- <brain> mcp --config <data-dir>/brain.yaml
```

For OpenCode add the same command to the `mcp` section of
`~/.config/opencode/opencode.json`. Projects resolve from the server's working
directory (the agent's project root) or the `project` argument; writes never
create projects.

## 3a3. Background agent and local control API

`brain agent` runs in the foreground and is the single background engine for a
data directory; a second instance exits because the OS lock is held.

```sh
brain agent --config <data-dir>/brain.yaml
brain agent status --config <data-dir>/brain.yaml
brain agent stop --config <data-dir>/brain.yaml
```

- Private state lives in `<data-dir>/agent/` (0600, self-ignored):
  `runtime.json`, `schedule.json`, `stop.json`, `settings.json`, `agent.log`
  (rotated) and the process lock. Nothing in that directory is published.
- `runtime.json` holds `{port, token, pid, version, started_at}`. The token is
  a per-process 256-bit bearer secret; never log it, paste it into tickets or
  commit it. The file is removed on a clean shutdown.
- Schedules come from `Connection.Schedule` (`manual`, `@hourly`, `@daily`,
  `@every <duration>`, minimum 1m). The scheduler catches up once after sleep
  and never replays a backlog; a run that is already queued or running for the
  connection is skipped.
- The local API is loopback-only and ephemeral: use the port and token from
  `runtime.json`. It requires exactly one `Authorization: Bearer` header, the
  exact loopback `Host`, rejects `Origin`/`Referer`, has no CORS and bounds
  request bodies. It exposes:
  `GET /v1/health`; connections CRUD, `POST /v1/connections/{id}/test|run`,
  `POST /v1/connections/{id}/credentials/{field}`; `GET /v1/runs`,
  `GET /v1/runs/{id}`, `GET /v1/runs/{id}/log` (SSE); schedules, projects,
  non-secret settings; `POST /v1/engine/update` with `{"confirm":true}`;
  `POST /v1/shutdown`. Consumers should not cache the token across restarts.
- `POST /v1/engine/update` downloads, verifies and stages a signed release and
  swaps the binary; the running process keeps the old image until restart, and
  `brain update --rollback` remains the recovery path.
- If the agent is unhealthy, check `agent status` first, then the tail of
  `agent/agent.log`; a stale runtime/token after a crash is cleaned on the next
  start while the lock is held. `brain agent stop` validates the PID is a brain
  executable before requesting shutdown.

## 3b. Shared agent instructions

One file in the private data repository, `<data-dir>/global/AGENTS.md`, holds
the instructions every coding agent must follow on every machine. `brain agents
sync` writes it into a managed block (between `LV24_SHARED_START` and
`LV24_SHARED_END` markers) in:

- `<workspace>/AGENTS.md` — the folder that contains the data directory;
- `~/.config/opencode/AGENTS.md` and `~/.codex/AGENTS.md`, when those tools are installed;
- `~/.claude/CLAUDE.md`, as a single `@<data-dir>/global/AGENTS.md` import line.

Text outside the markers is never touched. Edit the canonical file, run
`brain sync --push`, then `brain agents sync` on each machine (`--check` reports
drift without writing and exits 1).

`brain agents sync --hooks` (opt-in; the nightly job does not pass it) also installs
brain session hooks: SessionStart and SessionEnd in `~/.claude/settings.json`,
SessionStart, Stop and SessionEnd in `~/.codex/hooks.json`, and an OpenCode plugin in
`~/.config/opencode/plugins/` (`brain-session-end.js` if present, else `brain-handoff.js`).
Existing settings and hooks are kept, the commands are refreshed in place when the
binary or config path changes, and the first change to a file saves
`<file>.pre-brain-hooks`. Codex keeps new hooks disabled until you approve them in
Codex yourself (the owner approves hooks; brain does not). Brain never edits
`~/.codex/config.toml`. `--check --hooks` reports what would change and exits 1.

Scheduled refresh: on Windows the nightly "Company Brain Graph Refresh" task
runs graphs, connectors, `brain sync --push` and `brain agents sync`. On macOS
the LaunchAgent `by.lv24.brain-daily` (`~/Library/LaunchAgents/`, script
`~/.local/bin/brain-daily.sh`, log `~/Library/Logs/lv24-brain-daily.log`) runs
`brain sync --push` and `brain agents sync` daily at 09:30 or on wake after a
missed run; it runs in the login session, so git can use the keychain.
Run it now with `launchctl kickstart gui/$(id -u)/by.lv24.brain-daily`. The instructions refer to `brain` on PATH,
so they are identical on Windows and macOS.

### Daily digest

`brain digest` is the morning summary across all registered projects: what changed
in the window (default `24h`; Go durations such as `90m` or `24h`, or `7d`), per
project with activity, then a global "Needs you" section with proposed decisions
(listed whatever their age), open questions, blocked items and review finding counts.
Projects with nothing to report are omitted. Flags: `--project <id>` for one project,
`--json` for the envelope, `--write` to save the human digest to
`<data_dir>/digests/YYYY-MM-DD.md` (local/git mode; one file per day, overwritten on
rerun). The digests directory syncs with the data repo by design. Optional: add
`brain digest --write` to the nightly job (Windows `C:\0project\.brain\bin\brain-graphs.bat`,
macOS `~/.local/bin/brain-daily.sh`); the scripts are not changed by brain.

## 4. Server-mode operations

Server mode runs `brain-server` with PostgreSQL (`BRAIN_DATABASE_URL`). The
in-memory store used when that variable is unset loses data and tokens on
restart. It is for development only. Environment variables are listed in
[deploy.md](deploy.md#environment).

### Roles

| Role | Can do |
|---|---|
| `viewer` | read projects, state, notes, decisions, attempts, handoffs, search, revisions, own tokens, connector run records |
| `member` | everything a viewer can, plus create and update projects and project state, create notes, decisions, attempts and handoffs, and update or delete records they own |
| `admin` | everything a member can, plus manage users, delete projects, read the audit log, run connectors, and revoke other users' tokens |

Ownership rules:

- A member may update or delete only the notes, decisions, and attempts they
  created, and delete only the handoffs they created. The owner is the creating
  user id, shown as `createdBy`.
- Projects and project state are member-writable while a project is open
  (has no members). A restricted project follows its membership (below).
- Records with an unknown owner (created before ownership tracking) are
  admin-only.
- Connector runs are created by admins only (`POST /v1/connectors/run`).

Users are managed through the API, not the CLI, and only admins may call these
endpoints: `GET /v1/users`, `POST /v1/users`, `GET /v1/users/{id}`,
`PATCH /v1/users/{id}` (display name, role, auth source, with a `revision`),
and `DELETE /v1/users/{id}?revision=<n>`. Request fields are in
`GET /openapi.json`.

The first administrator can be created at startup with
`BRAIN_BOOTSTRAP_ADMIN_LOGIN` and `BRAIN_BOOTSTRAP_ADMIN_PASSWORD`. Set both or
neither; the password must pass the server's password policy. The account is
created once. On later starts an existing login is ignored, so the password
variable is not needed after the first successful start. Remove it from the
environment after that start.

### Project members (restricted projects)

A project without members is open to every user per their global role. Add
members to restrict it; then only members and admins can see it at all
(others get `404`, and it disappears from lists, search, revisions and SSE).
The project role is capped by the global role: a global viewer stays
read-only.

| Project role | Can do in that project |
|---|---|
| `viewer` | read |
| `member` | read, create records, put state, update/delete own records |
| `maintainer` | member, plus edit project metadata and update/delete any record |

Admin CLI (server mode, signed in as an admin):

```sh
brain project members acme                         # restriction state and members
brain project members acme add alice --role maintainer
brain project members acme add bob --role member   # same command changes a role
brain project members acme remove bob
brain project members acme restrict                # admin-only, no members yet
brain project members acme open                    # reopen; refused while members exist
```

API: `GET /v1/projects/{id}/members`, `PUT /v1/projects/{id}/members/{user}`
(`{"role": ...}`, user id or login), `DELETE /v1/projects/{id}/members/{user}`,
`PUT /v1/projects/{id}/access` (`{"restricted": true|false}`). All are
admin-only and audited (`GET /v1/audit?entityKind=project`).

Removing the last member keeps the project restricted (admin-only); reopen it
explicitly. Before restricting a busy project, add every member who still
needs it, then check `brain project members <id>`.

### Login and logout (clients)

```sh
brain init --mode server --server-url https://brain.example.test --config ~/brain-server.yaml
printf '%s\n' '<password>' | brain login --config ~/brain-server.yaml \
  --login <login> --password-stdin
brain status --config ~/brain-server.yaml
brain logout --config ~/brain-server.yaml
```

- `--password-stdin` reads one line from stdin. `BRAIN_PASSWORD` can be used
  instead of the flag, and `BRAIN_LOGIN` supplies the login when `--login` is
  omitted.
- `brain login` stores one credential bundle in the OS secret store, bound to
  the normalized server URL. It is never sent to another server.
- `brain login` warns when the URL is plain `http` and the host is not
  loopback. Use HTTPS for any shared server.
- `brain logout` revokes the session and its refresh-token family, then clears
  the stored bundle. An access token already issued stays valid until it
  expires (`BRAIN_ACCESS_TTL`, 15 minutes by default).

### Static tokens and BRAIN_TOKEN

- `BRAIN_TOKEN` (or `BRAIN_SECRET_SERVER_TOKEN`) is a static API token from the
  environment. It is intended for automation, not for a personal machine.
- While it is set, it takes precedence over stored credentials. `brain login`
  is refused, and `brain logout` leaves a stored session untouched and exits
  non-zero.
- A static token can be stored with `brain secret set server-token` (the value
  is read from stdin) and removed with `brain secret remove server-token --yes`.
- A static API token has no refresh token and cannot be revoked by
  `brain logout`. Revoke it on the server (see the incident checklist in
  section 8).

### API tokens

API tokens (prefix `brain_`) are created and revoked through the API:

- `POST /v1/tokens` with `name`, `role`, and `userId` (admins may mint for
  other users). The token role cannot exceed the role of the owner or of the
  caller. The plaintext is returned once; only a hash is stored.
- Set an expiry with `expiresInDays` (1 to 3650) or `expiresAt` (RFC 3339, in
  the future, at most 10 years away); not both. Without either, the token never
  expires until revoked. For automation, recommend `expiresInDays` (for example
  `90`) so a leaked or abandoned token stops working on its own. An expired token
  is rejected like a revoked one, and the response `apiToken.expiresAt` shows the
  expiry instant.
- `GET /v1/tokens` lists the caller's tokens. Admins may pass `userId`, or
  `all=true` to list every token.
- `DELETE /v1/tokens/{id}` revokes a token. Only the owner or an admin may do it.
- Scope a token with `scopes` (`read`, `write`, `admin`) and limit it to
  projects with `projectIds`. Omitted means unscoped and all projects, which is
  how every older token keeps working. `read` allows only GET requests, `write`
  adds knowledge/state/project writes, `admin` (admin-role tokens only) adds
  admin endpoints. Prefer the narrowest token for automation, for example a
  read-only token for dashboards:

  ```sh
  brain token create --name dashboard --scope read --project acme --expires 90d
  brain token create --name ci-writer --scope write --project acme --project infra --expires 720h
  brain token list            # admins: every user's tokens; others: their own
  brain token revoke <token-id>
  ```

  `create` prints the raw token once; it cannot be shown again. `list` never
  returns secrets or hashes, only id, name, owner, role, scopes, projects,
  created/expires/last-used time and status (active/expired/revoked).
  `--expires` takes days (`30d`) or a Go duration (`720h`), at most 3650 days;
  omit it for a token that never expires (`--expires-in-days N` still works).
  `--json` uses the standard `{schema, data}` envelope (`data.token`,
  `data.api_token`); since v0.6.3, v0.6.x printed `{token, apiToken}`. In local
  or git mode these commands exit 2.

  A token minted by another token can only narrow it (scopes, projects and
  expiry within the caller's). Scopes and projects are recorded in the
  `auth.token.created` audit event.

### Database pool, timeouts and request logs

- `BRAIN_DATABASE_MAX_CONNS` (default 10, range 1-200) sizes the pgx pool of
  each replica. Keep the sum across replicas below Postgres `max_connections`.
- `BRAIN_DATABASE_STATEMENT_TIMEOUT` (default `30s`, `0` disables, range
  1s-1h) is sent as `statement_timeout` on every connection. Startup migrations
  use the same limit. Before a large migration on a big table, raise it in
  `deploy/.env`, restart, and restore the value afterwards.
- Invalid values stop the server at startup with an error that names the
  variable.
- Request logs are JSON records on stderr: `msg="http request"`, `request_id`,
  `method`, `route` (the router pattern, never the raw path), `status`,
  `duration_ms`, and `user_id` for authenticated requests. Tokens, headers,
  query strings, and bodies are never logged. Server errors log at ERROR and
  health probes at DEBUG. Other logs keep the text format.

### Metrics

Metrics are off by default. With `BRAIN_METRICS_ENABLED=1` a second listener
serves Prometheus text format on `BRAIN_METRICS_ADDR` (default
`127.0.0.1:9090`). The public API port never serves `/metrics`.

- Series: `http_requests_total{method,route,code}`,
  `http_request_duration_seconds{method,route}` (histogram with fixed buckets
  from 5 ms to 10 s), `http_requests_in_flight`, `sse_clients`,
  `db_pool_acquired_connections`, `db_pool_idle_connections`,
  `db_pool_total_connections`, `db_pool_max_connections` (PostgreSQL only), and
  `brain_build_info{version,commit,go_version}`.
- Labels use the route pattern, such as `/v1/projects/{id}`. Unmatched paths
  share the label `unmatched`, and non-standard methods share `OTHER`, so
  cardinality stays bounded.
- The endpoint has no authentication. Keep it on loopback or on a private
  network. An address on a public interface exposes operational data.
- In the Compose stack the listener lives inside the container. To scrape it
  from another service on the same Compose network, set
  `BRAIN_METRICS_ADDR=0.0.0.0:9090` and scrape `brain-server:9090`. Do not
  publish the port to the host unless you need to.

### Retention

The retention job runs in every replica. The first run starts about one minute
after boot, then every `BRAIN_RETENTION_INTERVAL` (default `6h`). A Postgres
advisory lock lets only one replica sweep at a time. Set
`BRAIN_RETENTION_DISABLED=1` to turn it off. It is skipped in memory mode.

Each run deletes:

| Data | Rule |
|---|---|
| `audit_events` | `created_at` older than `BRAIN_AUDIT_RETENTION` (default `180d`). |
| `revisions` | `created_at` older than `BRAIN_REVISION_RETENTION` (default `90d`), and not referenced by any remaining audit event or as an entity's current revision. Audit rows are swept first, so revisions they held are freed in the same run. |
| `refresh_tokens` | `expires_at` in the past. Revoked rows are removed when they expire. |
| `auth_revocations` | `revoked_at` older than `BRAIN_REFRESH_TTL` (default `720h`). Tokens guarded by a tombstone have expired by then. |
| `api_tokens` | `expires_at` more than 30 days in the past. Tokens without an expiry are never removed. |

Effects to expect: `/v1/revisions` and SSE replay cover a shorter history. A
client that reconnects with an older `Last-Event-ID` receives only the events
that remain. Deletes run in batches of 1000 rows per statement, at most 500
batches per table per run, so locks stay short. Leftover rows go to the next
run. Each run logs `retention completed` with per-table counts, or
`retention failed` with the error.

### Audit log

Security events are kept in an audit log that only admins can read:

```sh
curl -sS -H "Authorization: Bearer $BRAIN_TOKEN" \
  "https://brain.example.test/v1/audit?action=auth.refresh.reused&limit=100"
```

`GET /v1/audit` (admin only) filters by `actor`, `action`, `entityKind`,
`entityId`, `since` and `until` (RFC 3339), with `limit` (default 50, maximum
500) and `offset`. The response is `{"items": [...]}`.

Recorded actions:

| Action | Meaning |
|---|---|
| `auth.login.succeeded`, `auth.login.failed`, `auth.login.rate_limited` | local password logins |
| `auth.logout` | session logout |
| `auth.refresh.reused` | a rotated refresh token was presented again |
| `auth.oidc.login`, `auth.oidc.linked`, `auth.oidc.signup`, `auth.oidc.refused` | OIDC sign-in outcomes |
| `auth.token.created`, `auth.token.revoked` | API token lifecycle |

Audit records carry the actor, the entity, and the client address in the
`client` detail field. They never contain passwords or tokens. Writes are
best-effort: a failed audit write is logged and does not fail the request.
Viewers and members never see these events.

### Proxies and rate limits

- Behind a reverse proxy, set `BRAIN_TRUSTED_PROXIES` to the proxy addresses,
  as comma-separated IPs or CIDRs. Only from those peers is `X-Forwarded-For`
  honoured, and it is walked from the right so spoofed left-hand entries are
  ignored. Empty means the header is ignored. In that case every client shares
  the proxy's address for rate limiting.
- Under Compose the peer is the Docker network gateway, for example
  `172.16.0.0/12` (see [deploy.md](deploy.md)).
- `BRAIN_LOGIN_RATE_LIMIT` sets the attempts per minute allowed per client
  address and, separately, per account.

### OIDC

OIDC uses a browser flow (`GET /v1/auth/oidc/login` and the callback). There is
no CLI OIDC flow. Settings: `BRAIN_OIDC_ISSUER`, `BRAIN_OIDC_CLIENT_ID`,
`BRAIN_OIDC_CLIENT_SECRET`, `BRAIN_OIDC_REDIRECT_URL`, `BRAIN_OIDC_SCOPES`,
`BRAIN_OIDC_GROUPS_CLAIM`, `BRAIN_OIDC_GROUP_ROLES` (`group=role` pairs),
`BRAIN_OIDC_DEFAULT_ROLE`, and `BRAIN_OIDC_ALLOW_SIGNUP`.

Account linking rules, stated plainly:

- A first OIDC login is linked to an existing local account only when the
  provider asserts `email_verified` and the verified email equals the account
  login.
- Admin accounts are never linked implicitly.
- An unverified email is never used as the login.
- Every other collision is refused with `403` and audited as
  `auth.oidc.refused`. Linking is audited as `auth.oidc.linked`.

## 5. Upgrades

### Client binary

```sh
brain update --check                    # report only
brain update --check --channel beta     # check a named channel
brain update --yes                      # verify and apply the newest release
brain update --rollback                 # restore brain(.exe).previous
```

- `--channel` takes `stable` or `beta`; the default comes from `brain.yaml`.
  It cannot be combined with `--rollback`. `--rollback` cannot be combined
  with `--check` or `--yes`.
- `--check` reports only. `--yes` downloads, verifies the signature and the
  SHA-256 checksum, and replaces the `brain` executable. On Windows the
  replacement happens after the process exits, so the new version takes effect
  on the next start.
- The updater never changes the data repository.
- Before ordinary commands the CLI performs a notify-only daily check. Set
  `BRAIN_NO_UPDATE=1` to disable it.

### Signed manifests

Each release publishes `release.json` and `release.json.sig`. The updater
accepts a release only when the signature verifies against a key embedded in
the binary (`trustedReleaseKeys` in `internal/update/signature.go`). An unsigned
release, a release signed by another key, or a tampered manifest is skipped. A
build with no embedded key refuses to update. The installers verify
`checksums.txt` only. See [update.md](update.md#signatures).

### Release signing setup (maintainer, once)

```sh
go run ./internal/tools/releasesign keygen -out <path-outside-the-repo>/release-signing.key
```

- `keygen` creates the file (it must not already exist) with the base64 private
  seed, and prints the public key.
- Add the public key to `trustedReleaseKeys` and commit it. A public key is not
  a secret.
- Store the seed as the `BRAIN_RELEASE_SIGNING_KEY` Actions secret, for example
  `gh secret set BRAIN_RELEASE_SIGNING_KEY < <path-outside-the-repo>/release-signing.key`.
  Then keep an offline backup and delete the file.
- The release workflow fails when the secret is missing. The signing step
  (`releasesign sign -in release.json -out release.json.sig`) reads the seed from
  `BRAIN_RELEASE_SIGNING_KEY` and refuses to sign unless the matching public key
  is embedded in the build. `--allow-unembedded` exists for testing only and must
  not be used for a release.
- Verify a manifest locally with
  `go run ./internal/tools/releasesign verify -in release.json -sig release.json.sig`
  (add `-pub <base64>` to check against a specific key).

### Key rotation

The updater accepts every key in `trustedReleaseKeys`, so rotation does not
break installed binaries:

1. Run `keygen` for the new key pair.
2. Add the new public key next to the old one and release a binary. Sign that
   release with the old key, which is still the active secret.
3. Once users are on that release, replace the `BRAIN_RELEASE_SIGNING_KEY`
   secret with the new seed.
4. Sign later releases with the new key.
5. In a later release, remove the old public key. Releases signed only by the
   old key are then refused by binaries built from that release.

## 6. Database migrations and the upgrade gate

- Server schema version: **9** (`SchemaVersion` in
  `internal/adapters/postgres/postgres.go`). Migrations are embedded from
  `internal/adapters/postgres/migrations/`, from `0001_init.sql` to
  `0009_connector_items.sql`.
- On startup the server applies embedded migrations to an empty database.
- For an existing schema with pending migrations, the server refuses to start
  unless `BRAIN_DATABASE_BACKUP_CONFIRMED=1` is set. The message names the
  pending versions.
- Migrations are forward-only. There are no down-migrations. Rollback means
  restoring a backup and running the previous image.

Gate procedure for an upgrade:

1. Confirm that no migration or long-running job is in progress.
2. Take a backup with `pg_dump -Fc` as described in
   [deploy.md](deploy.md#backup). Store it outside the Docker volumes.
   Check that it is readable with `pg_restore --list <dump-file>`.
3. Start the new server with `BRAIN_DATABASE_BACKUP_CONFIRMED=1` set in its
   environment.
4. Check `GET /ready`, and run `brain doctor --config <path>` and check the
   `server` line.
5. Remove `BRAIN_DATABASE_BACKUP_CONFIRMED` from the environment. Leaving it set
   would let a later migration run without the gate.

Rollback after a failed upgrade: start the previous image (tag or digest) and
restore the backup taken in step 2, following [deploy.md](deploy.md#restore).

## 7. Legacy migration and rollback

Use this only for a legacy `.brain` directory. For the overall plan see
[migration.md](migration.md).

```sh
brain migrate --from <legacy-dir> --data-dir <data-dir> --dry-run
brain migrate --from <legacy-dir> --data-dir <data-dir>
brain migrate --rollback latest --data-dir <data-dir>
brain migrate --rollback <manifest.json> --data-dir <data-dir> [--dry-run]
```

- `--from` is required for an import. `--data-dir` names the target, or
  `--config` can name a `brain.yaml` that holds it. `--rollback` cannot be
  combined with `--from`.
- Always run `--dry-run` first. It reports created, unchanged, conflicting and
  failed files without writing.
- The import is idempotent. It never overwrites a target file whose content
  differs. Conflicts and failures make the command exit non-zero.
- A successful import writes `schema_version: 1` to the target configuration.
- Each import writes a backup manifest under `<data-dir>/backups/`
  (`migrate-*`). `--rollback latest` picks the newest one. A manifest is
  accepted only for the target directory it was written for.
- Mail, connectors, the journal, legacy registry metadata, and global
  conventions are not imported.
- Keep the legacy directory read-only until the new engine has passed the
  parity checks.

## 8. Incident checklists

### Lost or leaked API token

Applies to `brain_` API tokens, the static `server-token` secret, and
`BRAIN_TOKEN` values. The server stores only hashes, so the listing never shows
a token value.

1. Find the token id with `GET /v1/tokens` (as the owner) or
   `GET /v1/tokens?all=true` (as an admin).
2. Revoke it: `DELETE /v1/tokens/{id}` (owner or admin).
3. Confirm the revocation: `GET /v1/audit?action=auth.token.revoked` (admin).
4. Check for unexpected mints during the exposure window:
   `GET /v1/audit?action=auth.token.created&since=<RFC 3339 time>`.
5. Issue a replacement with `POST /v1/tokens` and give it to the owner through
   a channel that is not this repository or a chat log.
6. On each client, run `brain secret remove server-token --yes` and
   `brain secret set server-token`. For automation, update the `BRAIN_TOKEN`
   value in the secret store of the job.
7. If the leaked value cannot be matched to a token id, revoke every token of
   that owner from the list and reissue them.

A leaked JWT access token is cut off as soon as its session is revoked: run
`brain logout` on the affected client (or delete/demote the user), and the
server rejects that session's access tokens on the next request.

### Refresh-token reuse alert

The event `auth.refresh.reused` means a refresh token that had already been
rotated was presented again. The server treats this as a leak and revokes the
whole token family (`ErrRefreshReused`). The user must sign in again.

1. Pull the events around the alert:
   `GET /v1/audit?action=auth.refresh.reused&since=<RFC 3339 time>`, and the
   `auth.login.*` and `auth.oidc.*` events in the same window.
2. Compare the `client` detail with the usual addresses for the account. The
   reuse event itself has no actor, so correlate by time and by the login events.
3. Ask the account owner whether they signed in from that client. If not,
   treat the account as compromised.
4. Review what the account wrote during the window with `GET /v1/audit?actor=<user-id>`.
5. The v1 API has no password-change and no revoke-all-sessions endpoint. For a
   compromised account, remove it with `DELETE /v1/users/{id}?revision=<n>`
   and create a new one with `POST /v1/users`, using a new password.

### Suspected release signing-key leak

Treat any exposure of `BRAIN_RELEASE_SIGNING_KEY`, its offline backup, or its
file as a compromise.

1. Stop signing. Remove or replace the `BRAIN_RELEASE_SIGNING_KEY` secret.
2. Find releases published after the suspected exposure. Remove any that are
   not authentic, with the GitHub UI or `gh release delete`. The release
   workflow never overwrites an existing release, so this step is manual.
3. Generate a new key pair (section 5). Build a release whose
   `trustedReleaseKeys` contains only the new key, and sign it with the new seed.
4. Limit of this procedure: installed binaries still trust the leaked key until
   they are replaced. The updater cannot revoke a key remotely, so an attacker
   with the seed can publish a manifest that those binaries accept. The new
   binary reaches users only by a manual download and install. Publish the
   checksums (`checksums.txt`) with it, and tell users to reinstall.
5. Server images are built from source and are not affected by the updater.
   Rebuild them with the new code if the leak touched the build pipeline.
6. Record the incident and the cut-over date in the private data repository
   with `brain decision` or `brain note`. Do not record the seed or any value
   from it.

### Data repository conflict

Follow section 2, Conflicts. Summary:

1. `brain sync --status` lists the conflicting paths.
2. Resolve them by hand in the data repository, then finish the rebase or commit.
3. `brain sync --status` must report no conflicts.
4. `brain sync --push`, then `brain index` on each machine that pulled changes.
5. Never force-push and never discard the other side's changes without reviewing them.
