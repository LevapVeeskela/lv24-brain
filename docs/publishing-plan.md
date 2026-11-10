# Publishing plan

Where and how to publish `brain` beyond GitHub Releases. Constraint: zero
extra spend — free registries, free CI on the public repository, no paid
features. Every channel ships the same signed GoReleaser binaries; none
builds its own.

## Status

| Channel | State | Install command for users |
|---|---|---|
| GitHub Releases + installers | live | `install.ps1` / `install.sh` (docs/install.md) |
| npm | live since v0.6.0 | `npm i -g lv24-brain` / `npx lv24-brain open` |
| PyPI (`lv24-brain` launcher) | live since v0.12.0 | `pip install lv24-brain` / `lv24-brain open .` |
| Homebrew tap | planned | `brew install levapveeskela/tap/brain` |
| Scoop bucket | planned | `scoop bucket add lv24 …; scoop install brain` |
| winget | later | `winget install LV24.Brain` |
| `go install` | works today | `go install github.com/levapveeskela/lv24-brain/cmd/brain@latest` |
| Docker image (server) | live from v0.6.2 (release.yml `image` job) | `docker pull ghcr.io/levapveeskela/lv24-brain-server` |
| Docs site | live | https://brain.lv24.by |

PyPI is a thin launcher, not a second engine: it requires Node.js 18+ and
delegates to the npm package at the same version. npm and the signed installers
remain canonical; see `docs/pypi.md`. `0.13.0` is the live PyPI release; the
launcher is prepared at `0.13.1` for the next engine tag.

Name check (2026-11-08): `lv24-brain` and `@lv24/brain` are free on npm;
`brain-cli` is taken.

## 1. npm (first)

**Package layout (esbuild-style, no install scripts):**
- `lv24-brain` — main package: a tiny `bin/brain.js` launcher that resolves
  the platform package and `spawnSync`s its binary with the same args,
  stdio inherited and exit code passed through. `optionalDependencies`
  list every platform package at the exact same version.
- `@lv24/brain-win32-x64`, `brain-win32-arm64`, `brain-darwin-x64`,
  `brain-darwin-arm64`, `brain-linux-x64`, `brain-linux-arm64` in the free
  npm organization `lv24` (unscoped names were rejected by npm spam
  detection) — each contains only the binary, with `os` and
  `cpu` fields so npm installs just the matching one.
- No `postinstall` download: works offline after install, survives
  `--ignore-scripts`, and nothing is fetched from outside npm.

Alternative: a single package with a postinstall that downloads the GitHub
release asset. Rejected: blocked by `--ignore-scripts`/corporate proxies,
and it duplicates the installer's signature logic.

**Engine changes needed:**
- `brain update` must detect an npm-managed binary (path inside
  `node_modules`) and print `npm i -g lv24-brain@latest` instead of
  replacing files under npm's control. Same later for Homebrew/Scoop.
- `brain version` shows the install source.

**Release automation (GitHub Actions, public repo = free minutes):**
- In the existing release workflow, after GoReleaser: generate the seven
  `package.json` files from the tag version, copy each binary from `dist/`,
  verify its SHA-256 against the signed manifest, then `npm publish
  --provenance --access public` (platform packages first, main last).
- Authentication: **npm Trusted Publishing** (OIDC from GitHub Actions; no
  long-lived token). Each of the seven packages trusts two workflows of
  `LevapVeeskela/lv24-brain`: `release.yml` (every tag) and `npm-publish.yml`
  (manual re-publish of an existing release). The jobs have
  `id-token: write`; npm >= 11.5.1 is required (CI's Node 24 ships 11.19)
  and provenance is attached automatically. `npm/publish.sh` uses OIDC when
  the job has it and falls back to `NODE_AUTH_TOKEN` only when one is
  passed explicitly; the workflows no longer pass `NPM_TOKEN`.
- The workflow refuses to publish when the npm version already exists
  (immutable, like GitHub releases).
- Dry run first: `npm pack` in CI on every PR (release-check job) to catch
  broken package files before a tag.

**Owner steps:**
1. Done 2026-11-08: npm account, free organization `lv24`, first publish
   (v0.6.0) with a granular token in the `NPM_TOKEN` secret.
2. **Trusted Publishing (before January 2027, when npm stops tokens that
   bypass 2FA for direct publishing).** For each package — `lv24-brain`,
   `@lv24/brain-win32-x64`, `@lv24/brain-win32-arm64`,
   `@lv24/brain-darwin-x64`, `@lv24/brain-darwin-arm64`,
   `@lv24/brain-linux-x64`, `@lv24/brain-linux-arm64` — open
   npmjs.com → package → Settings → Trusted publishing → Add trusted
   publisher → GitHub Actions, and enter (case-sensitive):
   - Organization or user: `LevapVeeskela`
   - Repository: `lv24-brain`
   - Workflow filename: `release.yml`
   - Environment: empty

   - Allowed actions: check **Allow npm publish** (without it only
     `npm stage publish` is allowed)

   Then add a second trusted publisher with the same values and workflow
   filename `npm-publish.yml`. Done 2026-11-08 for all seven packages;
   v0.7.1 was published through `npm-publish.yml` with OIDC. A new configuration must complete its first
   publish within 2 days, so do this shortly before the next release.
3. Release the next version. The log line
   `npm: publishing with Trusted Publishing (OIDC)` and the seven
   `+ <package>@<version>` lines confirm it worked. If it failed, fix the
   trusted publisher fields and re-run `npm-publish.yml` for that tag.
4. After a successful release: on each package, Settings → Publishing access
   → "Require two-factor authentication and disallow tokens"; then delete the
   secret (`gh secret delete NPM_TOKEN --repo LevapVeeskela/lv24-brain`) and
   revoke the granular token on npmjs.com (Access Tokens).

## 2. Homebrew tap and Scoop bucket

GoReleaser generates both from the same release (`brews:` and `scoops:`
sections). They need two public repositories, `homebrew-tap` and
`scoop-bucket`, and a fine-grained token with contents:write on just those
two repositories (`TAP_GITHUB_TOKEN` secret), because the default workflow
token cannot push to other repositories. Free.

## 3. winget

Submit a manifest to `microsoft/winget-pkgs` with `wingetcreate` after a
release (manual at first, automated later with the same tap token pattern
if it proves useful). Free; review takes a few days.

## 4. Server image

Publish `brain-server` to GitHub Container Registry from the release
workflow (`docker/build-push-action`, `GITHUB_TOKEN` with packages:write).
Free for public images. Multi-arch (amd64/arm64) via buildx.

## 5. Showcase site

Live at https://brain.lv24.by since 2026-11-08: static files from
`lv24-brain-site` served by the LV24 edge nginx (no cloud CI, no cost).
Deploy with `scripts/deploy-lv24.sh` in the site repository. GitHub Pages is
no longer planned.

## Order

1. npm: engine install-source detection → package generator + PR dry run →
   Trusted Publishing setup → first publish with a minor release.
2. Homebrew + Scoop (one GoReleaser change, one token).
3. winget.

Each step goes into its own release with docs (install.md, README, site)
updated in the same cycle.
