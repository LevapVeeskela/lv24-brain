#!/usr/bin/env bash
#
# LV24 Brain installer for macOS and Linux.
#
# Resolves a published GitHub release (latest = highest semantic version on the
# channel), reads release.json (or checksums.txt), verifies the SHA-256 checksum,
# installs the brain binary atomically, keeps the previous binary as
# brain.previous, and optionally initializes a brain instance. A re-run for the
# version already installed skips the download unless --force is given.
# --uninstall removes the binary and its leftovers; it never touches brain.yaml,
# the data directory, secrets, or ~/.claude / ~/.codex settings.
#
# The installer never asks for, writes, or logs secrets, never uses sudo, and
# never pipes a download into a shell. Compatible with bash 3.2 (macOS).

set -euo pipefail

REPO="LevapVeeskela/lv24-brain"
API_ROOT="https://api.github.com"
RELEASE_ROOT="https://github.com"

CHANNEL="stable"
REQUESTED_VERSION=""
INSTALL_DIR=""
MODE=""
DATA_DIR=""
SERVER_URL=""
CONFIG_PATH=""
ASSUME_YES=0
NO_PATH_HINT=0
FORCE=0
UNINSTALL=0

OS=""
ARCH=""
TAG=""
VERSION_NUMBER=""
ASSET_NAME=""
ASSET_URL=""
ASSET_SHA=""
EXPECTED_SHA=""
MANIFEST_VERSION=""
TARGET_PATH=""
CONFIG_RESULT=""
INSTALLED_ALREADY=0
TMP_DIR=""
STAGED_BIN=""
SHA_TOOL=""

print_usage() {
    cat <<'USAGE'
Install the LV24 Brain engine on macOS or Linux.

Usage:
  install.sh [options]

Options:
  --version vX.Y.Z       Install an exact release instead of the latest one.
  --channel stable|beta  Release channel: stable (default) or beta.
  --dir PATH             Binary directory (default: ~/.local/bin).
  --mode local|git|server
                         Install mode. Runs "brain init" only with --yes.
  --data-dir PATH        Brain data directory for local and git mode.
  --server-url URL       Brain server base URL for server mode (https only).
  --config PATH          brain.yaml path passed to "brain init".
  --yes                  Non-interactive: run "brain init" when --mode is set;
                         with --uninstall, skip the confirmation prompt.
  --force                Reinstall even when the binary already reports the version.
  --uninstall            Remove brain, brain.previous, and install leftovers from
                         --dir. Keeps brain.yaml, data, and secrets. Refuses
                         without --yes when stdin is not a terminal (exit 2).
  --no-path-hint         Do not print a PATH hint.
  -h, --help             Show this help.

Environment:
  GITHUB_TOKEN           Optional read-only token; raises GitHub API limits.
  TMPDIR                 Optional parent directory for the temporary workspace.

Examples:
  install.sh
  install.sh --mode local --yes
  install.sh --version v1.2.3 --dir "$HOME/bin"
  install.sh --mode server --server-url https://brain.example.com --yes

Every download is HTTPS-only and is verified against the release SHA-256
checksum before the binary is installed. No secrets are stored.
USAGE
}

die() {
    printf 'install.sh: %s\n' "$1" >&2
    exit 1
}

usage_error() {
    printf 'install.sh: %s\n\n' "$1" >&2
    print_usage >&2
    exit 2
}

step() {
    printf '==> %s\n' "$1"
}

note() {
    printf '    %s\n' "$1"
}

warn() {
    printf 'install.sh: warning: %s\n' "$1" >&2
}

flag_value() {
    # Prints the value for a long option or returns 1 when it is missing.
    if [ -z "${2:-}" ]; then
        return 1
    fi
    case "$2" in
        --*) return 1 ;;
    esac
    printf '%s' "$2"
}

expand_path() {
    case "${1:-}" in
        '~') printf '%s' "$HOME" ;;
        '~/'*) printf '%s/%s' "$HOME" "${1#\~/}" ;;
        *) printf '%s' "$1" ;;
    esac
}

absolute_path() {
    case "${1:-}" in
        /*) printf '%s' "$1" ;;
        *) printf '%s/%s' "$(pwd)" "$1" ;;
    esac
}

parse_args() {
    local arg
    local normalized
    normalized=()
    # Accept --flag=value by splitting it into two arguments first.
    for arg in "$@"; do
        case "$arg" in
            --*=*) normalized+=("${arg%%=*}" "${arg#*=}") ;;
            *) normalized+=("$arg") ;;
        esac
    done
    if [ "${#normalized[@]}" -gt 0 ]; then
        set -- "${normalized[@]}"
    else
        set --
    fi

    while [ "$#" -gt 0 ]; do
        case "$1" in
            -h|--help)
                print_usage
                exit 0
                ;;
            --version)
                REQUESTED_VERSION="$(flag_value "$1" "${2:-}")" || usage_error "$1 requires a value"
                shift 2
                ;;
            --channel)
                CHANNEL="$(flag_value "$1" "${2:-}")" || usage_error "$1 requires a value"
                shift 2
                ;;
            --dir)
                INSTALL_DIR="$(flag_value "$1" "${2:-}")" || usage_error "$1 requires a value"
                shift 2
                ;;
            --mode)
                MODE="$(flag_value "$1" "${2:-}")" || usage_error "$1 requires a value"
                shift 2
                ;;
            --data-dir)
                DATA_DIR="$(flag_value "$1" "${2:-}")" || usage_error "$1 requires a value"
                shift 2
                ;;
            --server-url)
                SERVER_URL="$(flag_value "$1" "${2:-}")" || usage_error "$1 requires a value"
                shift 2
                ;;
            --config)
                CONFIG_PATH="$(flag_value "$1" "${2:-}")" || usage_error "$1 requires a value"
                shift 2
                ;;
            --yes)
                ASSUME_YES=1
                shift
                ;;
            --no-path-hint)
                NO_PATH_HINT=1
                shift
                ;;
            --force)
                FORCE=1
                shift
                ;;
            --uninstall)
                UNINSTALL=1
                shift
                ;;
            -*)
                usage_error "unknown option: $1"
                ;;
            *)
                usage_error "unexpected argument: $1"
                ;;
        esac
    done
}

validate_args() {
    local authority
    case "$CHANNEL" in
        stable|beta) ;;
        *) usage_error "--channel must be stable or beta" ;;
    esac

    if [ -n "$REQUESTED_VERSION" ]; then
        case "$REQUESTED_VERSION" in
            v*) ;;
            *) REQUESTED_VERSION="v$REQUESTED_VERSION" ;;
        esac
        if ! printf '%s' "$REQUESTED_VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$'; then
            usage_error "--version must look like v1.2.3"
        fi
    fi

    if [ -n "$MODE" ]; then
        case "$MODE" in
            local|git|server) ;;
            *) usage_error "--mode must be local, git, or server" ;;
        esac
    fi

    if [ -n "$SERVER_URL" ]; then
        case "$SERVER_URL" in
            https://?*) ;;
            *) usage_error "--server-url must start with https:// and include a host" ;;
        esac
        case "$SERVER_URL" in
            *[[:space:]]*) usage_error "--server-url must not contain whitespace" ;;
        esac
        authority="${SERVER_URL#https://}"
        authority="${authority%%/*}"
        case "$authority" in
            *@*) usage_error "--server-url must not embed credentials" ;;
        esac
        if [ "$MODE" != server ]; then
            usage_error "--server-url requires --mode server"
        fi
    fi

    if [ "$MODE" = server ] && [ -z "$SERVER_URL" ]; then
        usage_error "--mode server requires --server-url"
    fi

    if [ -z "$MODE" ] && [ "$UNINSTALL" -eq 0 ] && { [ -n "$DATA_DIR" ] || [ -n "$CONFIG_PATH" ]; }; then
        warn "--data-dir and --config only apply with --mode; ignoring them"
        DATA_DIR=""
        CONFIG_PATH=""
    fi
}

check_prereqs() {
    command -v curl >/dev/null 2>&1 || die "curl is required"
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        case "$GITHUB_TOKEN" in
            *[[:space:]]*) die "GITHUB_TOKEN must not contain whitespace" ;;
        esac
    fi

    if command -v sha256sum >/dev/null 2>&1; then
        SHA_TOOL="sha256sum"
    elif command -v shasum >/dev/null 2>&1; then
        SHA_TOOL="shasum"
    elif command -v openssl >/dev/null 2>&1; then
        SHA_TOOL="openssl"
    else
        die "sha256sum, shasum, or openssl is required to verify the download"
    fi
}

http_get() {
    # http_get URL DESTINATION [api|artifact]
    # The optional token is sent only as an API request header. It is never
    # written to disk or printed.
    local url destination kind effective
    local -a curl_args
    url="$1"
    destination="$2"
    kind="${3:-artifact}"

    case "$url" in
        https://*) ;;
        *) die "refusing non-HTTPS URL: $url" ;;
    esac

    curl_args=(-q -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2
        --connect-timeout 15 --retry 2 --retry-delay 1 --max-redirs 5
        --max-time 300 --max-filesize 268435456)

    if [ "$kind" = api ]; then
        case "$url" in
            https://api.github.com/*) ;;
            *) die "refusing a non-GitHub API URL: $url" ;;
        esac
        curl_args+=(-H 'Accept: application/vnd.github+json'
            -H 'User-Agent: lv24-brain-installer')
        if [ -n "${GITHUB_TOKEN:-}" ]; then
            curl_args+=(-H "Authorization: Bearer $GITHUB_TOKEN")
        fi
    else
        assert_artifact_host "$url"
    fi

    effective="$(curl "${curl_args[@]}" -o "$destination" -w '%{url_effective}' "$url")" || return 1

    if [ "$kind" != api ]; then
        # Redirects may only land on the approved GitHub artifact hosts.
        assert_artifact_host "$effective"
    fi
}

http_try() {
    # Like http_get, but quiet: used for optional metadata probes. It runs in
    # a subshell so a refusal inside http_get (die) fails only this probe
    # instead of silently ending the installer with stderr discarded.
    ( http_get "$1" "$2" "${3:-artifact}" ) 2>/dev/null
}

json_flat() {
    tr -d '\r\n' <"$1"
}

json_objects() {
    # One brace-delimited record per line for documents nested one level deep
    # (release.json assets). Flat asset fields never leak into a later
    # "signature" object or the top-level trailing fields.
    tr -d '\r' <"$1" | awk 'BEGIN { RS = "}" } { gsub(/\n+/, " ", $0); print }'
}

json_string() {
    # json_string TEXT KEY: prints the last string value for KEY.
    printf '%s' "$1" | sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p"
}

json_true() {
    # json_true TEXT KEY: succeeds when KEY is the JSON literal true.
    local pattern
    pattern="\"$2\"[[:space:]]*:[[:space:]]*true"
    [[ "$1" =~ $pattern ]]
}

api_get() {
    # api_get PATH DESTINATION
    http_get "$API_ROOT$1" "$2" api
}

semver_valid() {
    # semver_valid TAG: succeeds for vMAJOR.MINOR.PATCH with optional -pre and +build.
    printf '%s' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
}

compare_num() {
    # compare_num A B: prints -1, 0, or 1 for two non-negative integers.
    if [ $((10#$1)) -lt $((10#$2)) ]; then
        printf -- '-1'
    elif [ $((10#$1)) -gt $((10#$2)) ]; then
        printf '1'
    else
        printf '0'
    fi
}

compare_pre_identifier() {
    # SemVer 2.0 rule for one prerelease identifier: numeric < alphanumeric,
    # numeric identifiers compare as numbers.
    local left="$1" right="$2" left_num=0 right_num=0
    case "$left" in ''|*[!0-9]*) ;; *) left_num=1 ;; esac
    case "$right" in ''|*[!0-9]*) ;; *) right_num=1 ;; esac
    if [ "$left_num" -eq 1 ] && [ "$right_num" -eq 1 ]; then
        compare_num "$left" "$right"
    elif [ "$left_num" -eq 1 ]; then
        printf -- '-1'
    elif [ "$right_num" -eq 1 ]; then
        printf '1'
    elif [ "$left" = "$right" ]; then
        printf '0'
    elif [[ "$left" < "$right" ]]; then
        printf -- '-1'
    else
        printf '1'
    fi
}

semver_compare() {
    # semver_compare A B: prints -1, 0, or 1 by SemVer 2.0 precedence.
    # A leading v and +build metadata are ignored. A release outranks its prereleases.
    local a b a_pre="" b_pre="" a1 a2 a3 b1 b2 b3 r i
    local -a ap bp
    a="${1#v}"
    a="${a%%+*}"
    b="${2#v}"
    b="${b%%+*}"
    case "$a" in *-*) a_pre="${a#*-}" ;; esac
    case "$b" in *-*) b_pre="${b#*-}" ;; esac
    IFS=. read -r a1 a2 a3 <<<"${a%%-*}"
    IFS=. read -r b1 b2 b3 <<<"${b%%-*}"
    r="$(compare_num "${a1:-0}" "${b1:-0}")"
    if [ "$r" != 0 ]; then printf '%s' "$r"; return 0; fi
    r="$(compare_num "${a2:-0}" "${b2:-0}")"
    if [ "$r" != 0 ]; then printf '%s' "$r"; return 0; fi
    r="$(compare_num "${a3:-0}" "${b3:-0}")"
    if [ "$r" != 0 ]; then printf '%s' "$r"; return 0; fi

    if [ -z "$a_pre" ] && [ -z "$b_pre" ]; then printf '0'; return 0; fi
    if [ -z "$a_pre" ]; then printf '1'; return 0; fi
    if [ -z "$b_pre" ]; then printf -- '-1'; return 0; fi

    IFS=. read -r -a ap <<<"$a_pre"
    IFS=. read -r -a bp <<<"$b_pre"
    i=0
    while [ "$i" -lt "${#ap[@]}" ] && [ "$i" -lt "${#bp[@]}" ]; do
        r="$(compare_pre_identifier "${ap[$i]}" "${bp[$i]}")"
        if [ "$r" != 0 ]; then printf '%s' "$r"; return 0; fi
        i=$((i + 1))
    done
    if [ "${#ap[@]}" -lt "${#bp[@]}" ]; then
        printf -- '-1'
    elif [ "${#ap[@]}" -gt "${#bp[@]}" ]; then
        printf '1'
    else
        printf '0'
    fi
}

release_records() {
    # release_records FILE: prints one line per release of a GitHub release list,
    # keeping only each release's own top-level fields. Nested author and asset
    # objects are dropped, so draft and prerelease belong to their own release.
    tr -d '\r' <"$1" | awk '
        BEGIN { depth = 0; instr = 0; esc = 0; rec = "" }
        {
            n = length($0)
            for (i = 1; i <= n; i++) {
                c = substr($0, i, 1)
                if (instr) {
                    if (esc) { esc = 0 } else if (c == "\\") { esc = 1 } else if (c == "\"") { instr = 0 }
                    if (depth == 2) rec = rec c
                    continue
                }
                if (c == "\"") {
                    instr = 1
                    if (depth == 2) rec = rec c
                } else if (c == "{" || c == "[") {
                    depth++
                } else if (c == "}" || c == "]") {
                    depth--
                    if (depth == 1 && c == "}") { print rec; rec = "" }
                } else if (depth == 2) {
                    rec = rec c
                }
            }
        }'
}

select_latest_release() {
    # select_latest_release FILE CHANNEL: prints the top-level record of the
    # highest-version non-draft release on CHANNEL. Stable skips prereleases;
    # beta accepts both. Non-semver tags are ignored.
    local file="$1" channel="$2" record tag best="" best_tag=""
    release_records "$file" >"$TMP_DIR/release-records.txt"
    while IFS= read -r record; do
        [ -n "$record" ] || continue
        tag="$(json_string "$record" tag_name)"
        semver_valid "$tag" || continue
        if json_true "$record" draft; then continue; fi
        if [ "$channel" = stable ] && json_true "$record" prerelease; then continue; fi
        if [ -z "$best_tag" ] || [ "$(semver_compare "$tag" "$best_tag")" = 1 ]; then
            best="$record"
            best_tag="$tag"
        fi
    done <"$TMP_DIR/release-records.txt"
    [ -n "$best" ] || return 1
    printf '%s\n' "$best"
}

binary_reports_version() {
    # binary_reports_version VERSION: succeeds when TARGET_PATH runs and its
    # "brain version" first line names VERSION (for example "brain 0.2.10 (commit ...)").
    local want line
    want="${1#v}"
    [ -x "$TARGET_PATH" ] || return 1
    line="$("$TARGET_PATH" version 2>/dev/null | sed -n '1p')" || return 1
    printf '%s\n' "$line" | awk -v want="$want" '
        { for (i = 1; i <= NF; i++) { f = $i; sub(/^v/, "", f); if (f == want) found = 1 } }
        END { exit (found ? 0 : 1) }'
}

resolve_release() {
    local release_file release_json
    release_file="$TMP_DIR/release.json"

    if [ -n "$REQUESTED_VERSION" ]; then
        TAG="$REQUESTED_VERSION"
        step "Looking up release $TAG"
        if ! api_get "/repos/$REPO/releases/tags/$TAG" "$release_file"; then
            die "release $TAG was not found in $REPO (or GitHub was unreachable)"
        fi
        release_json="$(json_flat "$release_file")"
    else
        step "Looking up the latest $CHANNEL release (highest semantic version)"
        if ! api_get "/repos/$REPO/releases?per_page=100" "$release_file"; then
            die "could not read the release list for $REPO; set GITHUB_TOKEN for rate limits or pin --version"
        fi
        release_json="$(select_latest_release "$release_file" "$CHANNEL")" ||
            die "no $CHANNEL release with a semantic version was found in $REPO"
        TAG="$(json_string "$release_json" tag_name)"
    fi
    [ -n "$TAG" ] || die "the GitHub response did not contain a release tag"
    case "$TAG" in
        v[0-9]*) ;;
        *) die "unexpected release tag from GitHub: $TAG" ;;
    esac
    case "$TAG" in
        *[!0-9A-Za-z.+-]*) die "release tag contains unexpected characters: $TAG" ;;
    esac
    VERSION_NUMBER="${TAG#v}"
    [ -n "$VERSION_NUMBER" ] || die "release tag has no version: $TAG"

    if json_true "$release_json" draft; then
        die "release $TAG is still a draft and cannot be installed; pin another --version"
    fi
    if json_true "$release_json" prerelease; then
        if [ "$CHANNEL" = stable ]; then
            die "release $TAG is a prerelease; use --channel beta"
        fi
    fi
    note "release: $TAG"
}

manifest_lookup() {
    # manifest_lookup FILE: selects the CLI asset for $OS/$ARCH.
    # Sets ASSET_NAME, ASSET_URL, ASSET_SHA, and MANIFEST_VERSION.
    local object name
    MANIFEST_VERSION="$(json_string "$(json_flat "$1")" version)"
    json_objects "$1" >"$TMP_DIR/manifest.lines"
    while IFS= read -r object; do
        [ -n "$object" ] || continue
        name="$(json_string "$object" name)"
        [ -n "$name" ] || continue
        [ "$(json_string "$object" os)" = "$OS" ] || continue
        [ "$(json_string "$object" arch)" = "$ARCH" ] || continue
        case "$(json_string "$object" kind)" in
            ''|cli|brain) ;;
            *) continue ;;
        esac
        ASSET_NAME="$name"
        ASSET_URL="$(json_string "$object" url)"
        ASSET_SHA="$(json_string "$object" sha256)"
        return 0
    done <"$TMP_DIR/manifest.lines"
    return 1
}

checksum_from_file() {
    # checksum_from_file FILE ASSET_NAME: prints the matching sha256, if any.
    local file want line digest name
    file="$1"
    want="$2"
    while IFS= read -r line || [ -n "$line" ]; do
        line=${line%$'\r'}
        case "$line" in
            ''|'#'*) continue ;;
        esac
        read -r digest name _ <<<"$line"
        [ -n "$digest" ] && [ -n "$name" ] || continue
        name=${name#\*}
        if [ "$name" = "$want" ]; then
            tr 'A-Z' 'a-z' <<<"$digest"
            return 0
        fi
    done <"$file"
    return 0
}

is_sha256() {
    [ -n "${1:-}" ] || return 1
    [ "${#1}" -eq 64 ] || return 1
    case "$1" in
        *[!0-9a-f]*) return 1 ;;
    esac
    return 0
}

sha256_file() {
    case "$SHA_TOOL" in
        sha256sum) sha256sum "$1" | awk '{print $1}' ;;
        shasum) shasum -a 256 "$1" | awk '{print $1}' ;;
        openssl) openssl dgst -sha256 "$1" | awk '{print $NF}' ;;
        *) return 1 ;;
    esac
}

assert_artifact_host() {
    case "${1:-}" in
        https://github.com/*) ;;
        https://objects.githubusercontent.com/*) ;;
        # GitHub serves release assets from this host since 2025.
        https://release-assets.githubusercontent.com/*) ;;
        *) die "refusing an artifact from an untrusted host: $1" ;;
    esac
}

# Release manifest signature (Ed25519). The key must match
# internal/update/signature.go; a Go test keeps them in sync.
RELEASE_PUBLIC_KEY_PEM='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEA8ZkT8c37JX6zeXnMEMHEnkcG9K8pGedO19gCAowW3iY=
-----END PUBLIC KEY-----'
SIGNATURE_CONTEXT='lv24-brain release manifest v1'

# find_ed25519_openssl prints an openssl binary that can verify Ed25519
# (OpenSSL 3+; macOS /usr/bin/openssl is LibreSSL and cannot).
find_ed25519_openssl() {
    local candidate key="$TMP_DIR/release-key.pem"
    printf '%s\n' "$RELEASE_PUBLIC_KEY_PEM" > "$key"
    for candidate in "$(command -v openssl 2>/dev/null || true)" \
        /opt/homebrew/bin/openssl /usr/local/bin/openssl \
        /opt/homebrew/opt/openssl@3/bin/openssl /usr/local/opt/openssl@3/bin/openssl; do
        [ -n "$candidate" ] && [ -x "$candidate" ] || continue
        if "$candidate" pkey -pubin -in "$key" -noout >/dev/null 2>&1; then
            printf '%s\n' "$candidate"
            return 0
        fi
    done
    return 1
}

# verify_manifest_signature MANIFEST SIG_URL: fails closed on a bad or
# missing signature when Ed25519 verification is available; otherwise warns
# (or dies when BRAIN_REQUIRE_SIGNATURE=1). The updater always verifies.
verify_manifest_signature() {
    local manifest="$1" sig_url="$2" ossl sig="$TMP_DIR/release.json.sig"
    if ! ossl="$(find_ed25519_openssl)"; then
        if [ "${BRAIN_REQUIRE_SIGNATURE:-}" = "1" ]; then
            die "BRAIN_REQUIRE_SIGNATURE=1 but no OpenSSL with Ed25519 support was found"
        fi
        note "release.json signature not checked (no OpenSSL 3 with Ed25519); relying on HTTPS and SHA-256"
        return 0
    fi
    http_try "$sig_url" "$sig" || die "release.json.sig is missing for $TAG"
    { printf '%s\n' "$SIGNATURE_CONTEXT"; cat "$manifest"; } > "$TMP_DIR/release-signed.bin"
    "$ossl" base64 -d -A -in "$sig" -out "$TMP_DIR/release-sig.bin" 2>/dev/null ||
        die "release.json.sig is not valid base64"
    if ! "$ossl" pkeyutl -verify -pubin -inkey "$TMP_DIR/release-key.pem" -rawin \
        -in "$TMP_DIR/release-signed.bin" -sigfile "$TMP_DIR/release-sig.bin" >/dev/null 2>&1; then
        die "release.json signature verification failed for $TAG"
    fi
    note "release.json signature verified"
}

resolve_package() {
    local manifest_url manifest_ok checksums_url manifest_checksums
    manifest_ok=0
    VERSION_NUMBER="${TAG#v}"
    manifest_url="$RELEASE_ROOT/$REPO/releases/download/$TAG/release.json"

    step "Reading release metadata"
    if http_try "$manifest_url" "$TMP_DIR/release-manifest.json"; then
        verify_manifest_signature "$TMP_DIR/release-manifest.json" "$manifest_url.sig"
        if manifest_lookup "$TMP_DIR/release-manifest.json"; then
            manifest_ok=1
            if [ -n "$MANIFEST_VERSION" ] && [ "$MANIFEST_VERSION" != "$VERSION_NUMBER" ]; then
                die "release.json version $MANIFEST_VERSION does not match tag $TAG"
            fi
        else
            note "release.json has no $OS/$ARCH CLI asset; using checksums.txt"
        fi
    else
        note "release.json is not available for $TAG; using checksums.txt"
    fi

    if [ -z "$ASSET_NAME" ]; then
        ASSET_NAME="brain_${VERSION_NUMBER}_${OS}_${ARCH}"
        ASSET_URL="$RELEASE_ROOT/$REPO/releases/download/$TAG/$ASSET_NAME"
    fi
    case "$ASSET_NAME" in
        *[!A-Za-z0-9._+-]*) die "release metadata contains an unexpected asset name: $ASSET_NAME" ;;
    esac

    if [ -n "$ASSET_SHA" ]; then
        EXPECTED_SHA="$(printf '%s' "$ASSET_SHA" | tr 'A-Z' 'a-z')"
    fi

    if ! is_sha256 "$EXPECTED_SHA"; then
        checksums_url="$RELEASE_ROOT/$REPO/releases/download/$TAG/checksums.txt"
        if [ "$manifest_ok" -eq 1 ]; then
            manifest_checksums="$(json_string "$(json_flat "$TMP_DIR/release-manifest.json")" checksums_url)"
            if [ -n "$manifest_checksums" ]; then
                checksums_url="$manifest_checksums"
            fi
        fi
        assert_artifact_host "$checksums_url"
        step "Downloading checksums.txt"
        http_get "$checksums_url" "$TMP_DIR/checksums.txt" || die "could not download $checksums_url"
        EXPECTED_SHA="$(checksum_from_file "$TMP_DIR/checksums.txt" "$ASSET_NAME" | tr 'A-Z' 'a-z')"
        is_sha256 "$EXPECTED_SHA" || die "no SHA-256 checksum found for $ASSET_NAME; refusing to install"
    fi

    assert_artifact_host "$ASSET_URL"

    if [ "$FORCE" -eq 0 ] && [ -f "$TARGET_PATH" ] && [ "$(sha256_file "$TARGET_PATH")" = "$EXPECTED_SHA" ]; then
        INSTALLED_ALREADY=1
    fi
}

install_binary() {
    local actual_sha
    if [ "$INSTALLED_ALREADY" -eq 1 ]; then
        step "brain $VERSION_NUMBER is already installed at $TARGET_PATH"
        return 0
    fi

    step "Downloading $ASSET_NAME"
    http_get "$ASSET_URL" "$TMP_DIR/brain-download" || die "download failed: $ASSET_URL"

    actual_sha="$(sha256_file "$TMP_DIR/brain-download")" || die "could not compute SHA-256 for $ASSET_NAME"
    if [ "$actual_sha" != "$EXPECTED_SHA" ]; then
        die "SHA-256 mismatch for $ASSET_NAME: got $actual_sha, expected $EXPECTED_SHA"
    fi
    note "checksum verified: $EXPECTED_SHA"

    mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR"
    [ -w "$INSTALL_DIR" ] || die "$INSTALL_DIR is not writable; choose another --dir (sudo is never used)"

    STAGED_BIN="$INSTALL_DIR/.brain.install.$$"
    cp "$TMP_DIR/brain-download" "$STAGED_BIN" || die "cannot stage the new binary in $INSTALL_DIR"
    chmod 0755 "$STAGED_BIN" || die "cannot make $STAGED_BIN executable"
    if [ -f "$TARGET_PATH" ]; then
        cp -p "$TARGET_PATH" "$TARGET_PATH.previous" || die "cannot keep the previous binary as $TARGET_PATH.previous"
    fi
    mv -f "$STAGED_BIN" "$TARGET_PATH" || die "cannot replace $TARGET_PATH"
    STAGED_BIN=""
    note "installed: $TARGET_PATH"
}

verify_binary() {
    local version_output
    if "$TARGET_PATH" version >/dev/null 2>&1; then
        version_output="$("$TARGET_PATH" version 2>/dev/null | sed -n '1p')"
        if [ -n "$version_output" ]; then
            note "binary check: $version_output"
        fi
    else
        warn "the installed binary did not answer 'version'; it may not match this platform"
    fi
}

print_path_hint() {
    if [ "$NO_PATH_HINT" -eq 1 ]; then
        return 0
    fi
    case ":$PATH:" in
        *":$INSTALL_DIR:"*)
            note "$INSTALL_DIR is already on PATH"
            ;;
        *)
            note "Add $INSTALL_DIR to PATH to run brain from any shell:"
            printf '      export PATH="%s:$PATH"\n' "$INSTALL_DIR"
            note "Add that line to ~/.zshrc (macOS) or ~/.profile (Linux)."
            ;;
    esac
}

resolve_instance_paths() {
    if [ "$MODE" = server ]; then
        if [ -z "$DATA_DIR" ] && [ -z "$CONFIG_PATH" ]; then
            CONFIG_PATH="$HOME/brain/brain.yaml"
        fi
    elif [ -z "$DATA_DIR" ]; then
        case "$MODE" in
            git) DATA_DIR="$HOME/brain-data" ;;
            *) DATA_DIR="$HOME/brain" ;;
        esac
    fi
    if [ -z "$CONFIG_PATH" ]; then
        if [ -n "$DATA_DIR" ]; then
            CONFIG_PATH="$DATA_DIR/brain.yaml"
        else
            CONFIG_PATH="$HOME/brain/brain.yaml"
        fi
    fi
}

init_command() {
    if [ "$MODE" = server ]; then
        printf '%s init --mode server --server-url %q --config %q' \
            "$TARGET_PATH" "$SERVER_URL" "$CONFIG_PATH"
    else
        printf '%s init --mode %s --data-dir %q --config %q' \
            "$TARGET_PATH" "$MODE" "$DATA_DIR" "$CONFIG_PATH"
    fi
}

run_init() {
    local init_args init_output
    init_args=(init --mode "$MODE")
    if [ "$MODE" = server ]; then
        init_args+=(--server-url "$SERVER_URL")
    else
        init_args+=(--data-dir "$DATA_DIR")
    fi
    init_args+=(--config "$CONFIG_PATH")

    step "Initializing a $MODE brain instance"
    if ! init_output="$("$TARGET_PATH" "${init_args[@]}")"; then
        [ -z "$init_output" ] || printf '%s\n' "$init_output" >&2
        die "brain init failed; the binary is installed. Run manually: $(init_command)"
    fi
    [ -z "$init_output" ] || printf '%s\n' "$init_output"
    CONFIG_RESULT="$(printf '%s\n' "$init_output" | sed -n 's/^config:[[:space:]]*//p' | sed -n '1p')"
    if [ -z "$CONFIG_RESULT" ]; then
        CONFIG_RESULT="$CONFIG_PATH"
    fi
}

finish() {
    if [ -n "$MODE" ]; then
        resolve_instance_paths
        CONFIG_RESULT="$CONFIG_PATH"
        if [ "$ASSUME_YES" -eq 1 ]; then
            run_init
        else
            note "brain init was not run. To finish setup, run:"
            note "  $(init_command)"
        fi
    fi

    step "Install complete"
    note "binary:  $TARGET_PATH"
    note "version: $VERSION_NUMBER"
    note "channel: $CHANNEL"
    if [ -n "$CONFIG_RESULT" ]; then
        note "config:  $CONFIG_RESULT"
    fi
    print_path_hint

    if [ -z "$MODE" ]; then
        note "Next: $TARGET_PATH init --mode local --data-dir \"$HOME/brain\""
        note "      $TARGET_PATH status --config \"$HOME/brain/brain.yaml\""
        note "See docs/install.md for git and server modes."
    elif [ "$ASSUME_YES" -eq 0 ]; then
        if [ "$MODE" = server ]; then
            note "Then: $TARGET_PATH login --config \"$CONFIG_RESULT\""
        fi
    elif [ "$MODE" = server ]; then
        note "Next: $TARGET_PATH login --config \"$CONFIG_RESULT\""
        note "      or set BRAIN_TOKEN / run: $TARGET_PATH secret set server-token"
    else
        note "Next: $TARGET_PATH status --config \"$CONFIG_RESULT\""
        note "      $TARGET_PATH doctor --config \"$CONFIG_RESULT\""
    fi
    note "No secrets were stored. Keep tokens and .env values out of the brain data directory."
}

cleanup() {
    if [ -n "${STAGED_BIN:-}" ] && [ -f "$STAGED_BIN" ]; then
        rm -f "$STAGED_BIN" || true
    fi
    if [ -n "${TMP_DIR:-}" ] && [ -d "$TMP_DIR" ]; then
        rm -rf "$TMP_DIR" || true
    fi
}

uninstall_targets() {
    # Prints the files --uninstall removes from INSTALL_DIR that exist, one per line.
    local candidate
    for candidate in "$INSTALL_DIR/brain" "$INSTALL_DIR/brain.previous" \
        "$INSTALL_DIR"/.brain.install.* "$INSTALL_DIR"/.brain*-old-* "$INSTALL_DIR"/.brain*-new-*; do
        if [ -e "$candidate" ] || [ -L "$candidate" ]; then
            printf '%s\n' "$candidate"
        fi
    done
}

uninstall() {
    local targets path
    targets="$(uninstall_targets)"
    if [ -z "$targets" ]; then
        step "Uninstall: nothing to remove in $INSTALL_DIR"
        return 0
    fi

    step "Uninstalling brain from $INSTALL_DIR"
    while IFS= read -r path; do
        note "will remove: $path"
    done <<<"$targets"

    if [ "$ASSUME_YES" -eq 0 ]; then
        if [ ! -t 0 ]; then
            printf 'install.sh: uninstall needs confirmation; rerun with --yes when stdin is not a terminal\n' >&2
            exit 2
        fi
        printf 'Remove the files listed above? [y/N] '
        local answer=""
        IFS= read -r answer || answer=""
        case "$answer" in
            y|Y|yes|YES) ;;
            *) step "Uninstall cancelled; nothing was removed."; exit 1 ;;
        esac
    fi

    while IFS= read -r path; do
        rm -f "$path" || die "cannot remove $path"
        note "removed: $path"
    done <<<"$targets"

    step "Kept (never removed by the uninstaller)"
    note "brain.yaml and data directories (default \$HOME/brain or \$HOME/brain-data, or the --data-dir you passed)"
    note "secrets, .env files, and ~/.claude / ~/.codex settings"
    if [ -n "$DATA_DIR" ]; then
        note "To delete data manually after a backup: rm -rf \"$DATA_DIR\""
    else
        note "To delete data manually after a backup: rm -rf \"\$HOME/brain\" or \"\$HOME/brain-data\""
    fi
    note "Remove any PATH export you added to your shell profile yourself; this installer does not add one."
}

main() {
    local uname_s uname_m

    parse_args "$@"
    validate_args

    if [ -z "${HOME:-}" ]; then
        die "HOME is not set; pass --dir to choose an install directory"
    fi
    if [ -z "$INSTALL_DIR" ]; then
        INSTALL_DIR="$HOME/.local/bin"
    fi
    INSTALL_DIR="$(absolute_path "$(expand_path "$INSTALL_DIR")")"
    TARGET_PATH="$INSTALL_DIR/brain"
    if [ -n "$DATA_DIR" ]; then
        DATA_DIR="$(absolute_path "$(expand_path "$DATA_DIR")")"
    fi
    if [ -n "$CONFIG_PATH" ]; then
        CONFIG_PATH="$(absolute_path "$(expand_path "$CONFIG_PATH")")"
    fi

    # Uninstall only removes local files, so it does not need a supported platform check.
    if [ "$UNINSTALL" -eq 1 ]; then
        uninstall
        return 0
    fi

    uname_s="$(uname -s)"
    case "$uname_s" in
        Linux) OS="linux" ;;
        Darwin) OS="darwin" ;;
        *) die "unsupported operating system: $uname_s (releases cover Linux and macOS)" ;;
    esac
    uname_m="$(uname -m)"
    case "$uname_m" in
        x86_64|amd64) ARCH="amd64" ;;
        arm64|aarch64) ARCH="arm64" ;;
        *) die "unsupported architecture: $uname_m (releases cover amd64 and arm64)" ;;
    esac

    check_prereqs
    TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/lv24-brain-install.XXXXXX")" || die "cannot create a temporary directory"
    trap cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM

    step "Installing brain for $OS/$ARCH into $INSTALL_DIR"
    resolve_release
    if [ "$FORCE" -eq 0 ] && binary_reports_version "$VERSION_NUMBER"; then
        step "brain $VERSION_NUMBER is already installed at $TARGET_PATH; download skipped (use --force to reinstall)"
        INSTALLED_ALREADY=1
    else
        resolve_package
        install_binary
    fi
    verify_binary
    finish
}

# Tests source this file with LV24_INSTALLER_SOURCE_ONLY=1 to call helpers such as semver_compare.
if [ "${LV24_INSTALLER_SOURCE_ONLY:-0}" != 1 ]; then
    main "$@"
fi
