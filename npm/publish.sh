#!/usr/bin/env bash
# Publishes the npm packages that npm/build.mjs generated into OUT_DIR:
# platform packages first, then the lv24-brain launcher. A version already on
# npm is skipped, so re-running is safe. Prereleases use the beta dist-tag.
#
# usage: npm/publish.sh VERSION OUT_DIR
# env:   NODE_AUTH_TOKEN (npm token); empty skips publishing with a notice.
set -euo pipefail

VERSION="${1:?usage: npm/publish.sh VERSION OUT_DIR}"
OUT_DIR="${2:?usage: npm/publish.sh VERSION OUT_DIR}"

if [ -z "${NODE_AUTH_TOKEN:-}" ]; then
  echo "::notice title=npm::NPM_TOKEN is not set; npm publish skipped for $VERSION."
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

for dir in "$OUT_DIR"/@lv24-brain/*/; do
  publish "${dir%/}"
done
publish "$OUT_DIR/lv24-brain"
