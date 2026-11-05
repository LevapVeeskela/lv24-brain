# LV24 Brain installer for macOS and Linux

`install.sh` installs the `brain` CLI from a GitHub release. It requires bash
3.2+ (the macOS default), `curl`, and `sha256sum`, `shasum`, or `openssl`.

```sh
bash ./install.sh --help
bash ./install.sh --mode local --yes
bash ./install.sh --mode git --data-dir ~/brain-data --yes
bash ./install.sh --mode server --server-url https://brain.example.com --yes
```

By default it installs to `~/.local/bin`; use `--dir` to change that. It
verifies the release SHA-256 before replacing anything, stages the binary on
the destination filesystem, and keeps the previous copy as `brain.previous`
for manual rollback. It never uses `sudo`.

`GITHUB_TOKEN` is optional and read-only; it only raises GitHub API rate
limits and is never printed or stored. The installer never asks for, writes, or
logs secrets.

Git mode expects an existing clone of the private data repository. See
[`docs/install.md`](../../docs/install.md) for all options, verification, and
rollback/uninstall commands.
