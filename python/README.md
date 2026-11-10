# lv24-brain (Python launcher)

A thin Python launcher for **LV24 Brain**, the curated project memory CLI for
coding agents and people. LV24 Brain itself is a Go binary published on npm as
[`lv24-brain`](https://www.npmjs.com/package/lv24-brain) and through signed
GitHub release installers. This package does not reimplement the CLI, bundle
Node.js, or download the binary: the `lv24-brain` command delegates to the npm
package through `npx`.

The npm package, the signed installers, and GitHub Releases remain the
canonical distribution. The PyPI package only contains this launcher.

## The `brain` name

This package intentionally installs **only** the `lv24-brain` console script.
It does not install `brain`, because the native Go binary, the signed
installers, and the npm package already own that name. A pip-installed `brain`
script would silently shadow the real binary depending on `PATH` order. Use
`lv24-brain` for the launcher, or install the native binary for `brain`:

```bash
npm i -g lv24-brain
# or the installers in docs/install.md
brain version
```

## Prerequisites

- **Node.js 18 or newer**, including npm and npx
  ([nodejs.org](https://nodejs.org/)).
- Network access on first run, or an npm cache that already contains the
  requested package, because `npx` may download it.
- Python 3.9 or newer.

The CLI itself does not need a Go toolchain or a checkout of the repository.

## Installation

The launcher is available on PyPI as
[`lv24-brain`](https://pypi.org/project/lv24-brain/); the current version is
`0.14.0`:

```bash
pip install lv24-brain
```

This installs one console script, `lv24-brain`. It does not install `brain`:
the native Go binary and the npm package keep that command. The launcher also
runs as a module:

```bash
python -m lv24_brain --help
```

## Usage

```bash
lv24-brain open .
lv24-brain search "auth"
lv24-brain handoff "done: login page" --next "wire the logout button"
```

All arguments are forwarded unchanged to the npm CLI. The child process
inherits the terminal, environment, and exit code.

If `npx` cannot be found, the launcher exits with code `127` and prints an
installation hint. An interrupted run exits with code `130`.

## Version pinning

By default the launcher runs the npm package version matching its own version
(currently `0.14.0`). Override it with `BRAIN_NPM_VERSION` to use any npm
version or tag:

```bash
BRAIN_NPM_VERSION=latest lv24-brain --help
BRAIN_NPM_VERSION=0.14.0 lv24-brain --help
```

PowerShell:

```powershell
$env:BRAIN_NPM_VERSION = "latest"; lv24-brain --help
```

Keep the launcher and the npm CLI on the same version unless you have a reason
not to; the Python package is released for the same tag.

## Links

- Documentation: <https://brain.lv24.by>
- Repository and issues:
  <https://github.com/LevapVeeskela/lv24-brain>
- Publishing runbook:
  [docs/pypi.md](https://github.com/LevapVeeskela/lv24-brain/blob/main/docs/pypi.md)
- npm package: <https://www.npmjs.com/package/lv24-brain>

## License

Apache-2.0. See [LICENSE](LICENSE).
