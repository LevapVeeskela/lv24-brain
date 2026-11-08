#!/usr/bin/env bash
# Publishes the npm packages that npm/build.mjs generated into OUT_DIR:
# platform packages first, then the lv24-brain launcher. A version already on
# npm is skipped, so re-running is safe. Prereleases use the beta dist-tag.
#
# usage: npm/publish.sh VERSION OUT_DIR
#
# Authentication, in order:
#   1. npm Trusted Publishing (OIDC): in a GitHub Actions job with
#      `id-token: write`, npm exchanges the job's OIDC token for a short-lived
#      publish credential. Each package lists release.yml and npm-publish.yml
#      as trusted publishers on npmjs.com. Needs npm >= 11.5.1.
#   2. NODE_AUTH_TOKEN: a classic token, kept only as a manual fallback.
# Without either, publishing is skipped with a notice.
set -euo pipefail

VERSION="${1:?usage: npm/publish.sh VERSION OUT_DIR}"
OUT_DIR="${2:?usage: npm/publish.sh VERSION OUT_DIR}"

# actions/setup-node exports this placeholder when registry-url is set and
# no token was given; it is not a credential.
SETUP_NODE_PLACEHOLDER="XXXXX-XXXXX-XXXXX-XXXXX"
token="${NODE_AUTH_TOKEN:-}"
if [ "$token" = "$SETUP_NODE_PLACEHOLDER" ]; then
  token=""
fi

version_at_least() {
  # version_at_least HAVE WANT: true when HAVE >= WANT (numeric x.y.z).
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -t. -k1,1n -k2,2n -k3,3n | head -n1)" = "$2" ]
}

if [ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ] && [ -z "$token" ]; then
  npm_version="$(npm --version)"
  if ! version_at_least "$npm_version" "11.5.1"; then
    echo "npm $npm_version is too old for Trusted Publishing (needs >= 11.5.1)" >&2
    exit 1
  fi
  # A configured token (even a placeholder) would be sent instead of OIDC.
  unset NODE_AUTH_TOKEN
  echo "npm: publishing with Trusted Publishing (OIDC), npm $npm_version"
elif [ -n "$token" ]; then
  echo "npm: publishing with NODE_AUTH_TOKEN"
else
  echo "::notice title=npm::no OIDC token (id-token: write) and no NODE_AUTH_TOKEN; npm publish skipped for $VERSION."
  exit 0
fi

tag_args=()
case "$VERSION" in
  *-*) tag_args=(--tag beta) ;;
esac

publish() {
  # A "./" prefix keeps npm from reading "dir/name" as a GitHub shorthand.
  local dir="./${1#./}"
  local name
  name="$(node -p "JSON.parse(require('fs').readFileSync(process.argv[1], 'utf8')).name" "$dir/package.json")"
  if npm view "$name@$VERSION" version >/dev/null 2>&1; then
    echo "$name@$VERSION already on npm; skipping"
    return 0
  fi
  npm publish "$dir" --provenance --access public "${tag_args[@]}"
}

for dir in "$OUT_DIR"/@lv24/*/; do
  publish "${dir%/}"
done
publish "$OUT_DIR/lv24-brain"
