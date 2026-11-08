# Publishing plan

Where and how to publish `brain` beyond GitHub Releases. Constraint: zero
extra spend — free registries, free CI on the public repository, no paid
features. Every channel ships the same signed GoReleaser binaries; none
builds its own.

## Status

| Channel | State | Install command for users |
|---|---|---|
| GitHub Releases + installers | live | `install.ps1` / `install.sh` (docs/install.md) |
| npm | ready, publishes on the next release | `npm i -g lv24-brain` / `npx lv24-brain open` |
| Homebrew tap | planned | `brew install levapveeskela/tap/brain` |
| Scoop bucket | planned | `scoop bucket add lv24 …; scoop install brain` |
| winget | later | `winget install LV24.Brain` |
| `go install` | works today | `go install github.com/levapveeskela/lv24-brain/cmd/brain@latest` |
| Docker image (server) | planned | `docker pull ghcr.io/levapveeskela/brain-server` |
| Showcase site | built locally | GitHub Pages (free for public repos) |

Name check (2026-11-08): `lv24-brain` and `@lv24/brain` are free on npm;
`brain-cli` is taken.

## 1. npm (first)

**Package layout (esbuild-style, no install scripts):**
- `lv24-brain` — main package: a tiny `bin/brain.js` launcher that resolves
  the platform package and `spawnSync`s its binary with the same args,
  stdio inherited and exit code passed through. `optionalDependencies`
  list every platform package at the exact same version.
- `@lv24-brain/win32-x64`, `win32-arm64`, `darwin-x64`, `darwin-arm64`,
  `linux-x64`, `linux-arm64` in the free npm organization `lv24-brain`
  (unscoped names were rejected by npm spam detection) — each contains only the binary, with `os` and
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
- Authentication — preferred: **npm Trusted Publishing** (OIDC from GitHub
  Actions; no long-lived token at all). Configure once on npmjs.com per
  package: repository `LevapVeeskela/lv24-brain`, workflow `release.yml`.
  Fallback: a **granular access token** limited to "publish" on the
  `lv24-brain` packages with an expiry (e.g. 90 days), stored only as the
  repository secret `NPM_TOKEN`.
- The workflow refuses to publish when the npm version already exists
  (immutable, like GitHub releases).
- Dry run first: `npm pack` in CI on every PR (release-check job) to catch
  broken package files before a tag.

**Owner steps (the token never goes into chat or files):**
1. `npm login` locally once to confirm the account, then on npmjs.com
   either set up Trusted Publishing for `lv24-brain` (preferred), or create
   a granular publish token.
2. If a token: `gh secret set NPM_TOKEN --repo LevapVeeskela/lv24-brain` and
   paste it at the prompt in your own terminal. Done 2026-11-08 (granular
   publish token).
3. Create the free npm organization `lv24-brain` (platform packages live
   there) and make sure the token can publish to it.

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

`lv24-brain-site` → GitHub Pages from a public repository (free). Needs a
remote, a `pages` workflow (build + deploy, one Linux job), and sanitized
`_source/` history first (owner name, private repository names and local
paths currently appear in the copied docs).

## Order

1. npm: engine install-source detection → package generator + PR dry run →
   Trusted Publishing setup → first publish with a minor release.
2. Homebrew + Scoop (one GoReleaser change, one token).
3. Server image on GHCR.
4. Site on Pages after the content sanitization.
5. winget.

Each step goes into its own release with docs (install.md, README, site)
updated in the same cycle.
