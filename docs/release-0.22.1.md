# Release 0.22.1 — `sync --watch` and the three-machine proof

Status: release candidate on `main`; tag `v0.22.1`.

0.22.1 adds the loop that lets every machine publish without waiting for a
manual `brain sync`, and a test that proves the intended topology: three clones,
two concurrent sessions each.

## `brain sync --watch`

```sh
brain sync --watch --push --interval=5m --config ~/brain-data/brain.yaml
```

- Runs one sync immediately, then every interval (default `5m`, minimum `30s`).
- Stops cleanly on SIGINT/SIGTERM.
- A conflict or transient failure is logged and retried on the next tick; the
  data repository stays canonical and a human can resolve the conflict while
  the service keeps running.
- Rejects `--status` and `--json` combinations; JSON is not a stream.
- Works under a macOS LaunchAgent, a systemd user timer, or the Windows
  "Company Brain Update" task. The Windows task now runs `sync --push` every
  30 minutes; the other machines can use `--watch`.

## Three-machine concurrency test

`TestThreeMachinesWriteConcurrently` creates three clones, two concurrent
sessions per machine, and writes unique notes and three different state fields.
After the sync chain it verifies on every clone:

- all 13 note markers are present (seed plus 12 concurrent notes);
- the goal, next action and active list all survive;
- the registry keeps one project entry.

The test registers the same deterministic merge drivers the CLI registers
through `brain doctor --fix` / the sync preflight.

## Semantics

Independent records, notes, different state fields, project paths and aliases
merge without manual work. A same-field state edit or an alias claimed by two
machines is kept deterministically and reported as a conflict artifact for
review. True same-field, same-instant semantics still require server-canonical
mode; git remains canonical through 0.22–0.23.
