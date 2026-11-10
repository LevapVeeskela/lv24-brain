# PyPI launcher

`python/` contains the thin Python launcher published to PyPI as
[`lv24-brain`](https://pypi.org/project/lv24-brain/). It is not a second
implementation of the CLI and it does not contain a binary.

**Canonical distribution:** the npm package `lv24-brain`, the signed
PowerShell/bash installers, and GitHub Releases stay authoritative. Every
launcher release delegates to the npm package at the same version, so the Go
binary is still built and signed once, by `release.yml`.

**Status:** the launcher and workflow live in this repository, but no PyPI
project has been created yet. The owner must add the trusted publisher below
before the first upload. Until then, `pip install lv24-brain` will not find the
package.

## What the package is

- `python/pyproject.toml` — metadata and the single `lv24-brain` console
  script.
- `python/src/lv24_brain/cli.py` — finds `npx` and runs
  `npx --yes lv24-brain@<version>` with the caller's arguments, stdio,
  environment, and exit code. `BRAIN_NPM_VERSION` overrides the version or tag.
- `python/tests/test_cli.py` — mocked/fake-npx tests for argument forwarding,
  exit-code propagation, the override, missing `npx` (`127`), `OSError`
  (`127`), and interrupt (`130`).
- `.github/workflows/pypi-publish.yml` — build, validate, and publish with
  Trusted Publishing (OIDC). No API token and no PyPI secret.

The launcher never uses a shell, so arguments and the version string cannot be
interpreted as shell syntax.

## `brain` name conflict

Only `lv24-brain` is installed. The `brain` command belongs to the native Go
binary and to the npm package; a pip-installed `brain` script would shadow it
depending on `PATH` order, possibly breaking `brain update` and the signed
installer layout. Keep using `brain` for a native install and `lv24-brain` for
the Python launcher.

## Version source of truth

There is no root `package.json` in this repository: `npm/build.mjs` generates
the npm manifests from the release tag at release time. The release tag
(`vX.Y.Z`) is therefore the version source of truth. `python/pyproject.toml`
and `python/src/lv24_brain/__init__.py` must be bumped to the same version
before tagging. The workflow also compares a root `package.json` when one
exists (for example in a future layout), but it is not required today.

## Local checks

From the repository root:

```bash
python -m compileall -q python/src python/tests
python -m unittest discover -s python/tests -v

cd python
python -m build
python -m twine check dist/*
```

`python -m build` and `twine` are development tools only; they are not
dependencies of the installed launcher.

## Releasing

1. Set the same version in `python/pyproject.toml` and
   `python/src/lv24_brain/__init__.py`. The npm and Go versions come from the
   tag, so no additional manifest needs editing.
2. Merge that change, then push the `vX.Y.Z` tag. `release.yml` builds the
   binaries and creates the GitHub release.
3. Run **pypi-publish** manually with the version. `release.yml` creates the
   release with the repository `GITHUB_TOKEN`, and GitHub does not start
   workflows from events caused by that token, so the `release: published`
   trigger does not fire for these releases. Publishing the release from the
   GitHub UI also triggers the workflow.
4. The workflow refuses a version that does not match the tag, the pyproject
   file, and `__init__.py`. If that version already exists on PyPI, the upload
   is skipped: PyPI releases are immutable.

Example:

```bash
gh workflow run pypi-publish.yml --ref main -f version=0.12.0
```

## One-time PyPI setup (owner)

No PyPI project or token is created by this repository. To enable publishing,
the owner adds a Trusted Publisher on PyPI (or a pending publisher for the
first release):

- PyPI project name: `lv24-brain`
- Owner: `LevapVeeskela`
- Repository: `lv24-brain`
- Workflow filename: `pypi-publish.yml`
- Environment: empty

The workflow needs `id-token: write` (already set). It uses
`pypa/gh-action-pypi-publish` with a pinned commit and passes no credentials.
PyPI issues a short-lived upload token for the job.

Until that setup exists, the workflow fails at the publish step with an OIDC
error; build and tests still run. Do not add a PyPI API token as a fallback
unless the owner explicitly asks for one.

## Cost

Public repository, standard GitHub-hosted runner, OIDC, and free PyPI
publishing: no paid services or extra storage are involved. Build artifacts are
not uploaded to GitHub.
