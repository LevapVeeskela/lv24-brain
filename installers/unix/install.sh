#!/usr/bin/env bash
#
# LV24 Brain installer for macOS and Linux.
#
# Resolves a published GitHub release, reads release.json (or checksums.txt),
# verifies the SHA-256 checksum, installs the brain binary atomically, keeps
# the previous binary as brain.previous, and optionally initializes a brain
# instance.
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
  --yes                  Non-interactive: run "brain init" when --mode is set.
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

    if [ -z "$MODE" ] && { [ -n "$DATA_DIR" ] || [ -n "$CONFIG_PATH" ]; }; then
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
        # Redirects may only land on the two approved artifact hosts.
        assert_artifact_host "$effective"
    fi
}

http_try() {
    # Like http_get, but quiet: used for optional metadata probes.
    http_get "$1" "$2" "${3:-artifact}" 2>/dev/null
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

resolve_release() {
    local release_file release_json
    release_file="$TMP_DIR/release.json"

    if [ -n "$REQUESTED_VERSION" ]; then
        TAG="$REQUESTED_VERSION"
        step "Looking up release $TAG"
        if ! api_get "/repos/$REPO/releases/tags/$TAG" "$release_file"; then
            die "release $TAG was not found in $REPO (or GitHub was unreachable)"
        fi
    elif [ "$CHANNEL" = stable ]; then
        step "Looking up the latest stable release"
        if ! api_get "/repos/$REPO/releases/latest" "$release_file"; then
            die "no stable release resolved; set GITHUB_TOKEN for rate limits or use --channel beta"
        fi
    else
        step "Looking up the latest $CHANNEL release"
        if ! api_get "/repos/$REPO/releases?per_page=1" "$release_file"; then
            die "could not read the release list for $REPO"
        fi
    fi

    release_json="$(json_flat "$release_file")"
    if [ -z "$TAG" ]; then
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
        *) die "refusing an artifact from an untrusted host: $1" ;;
    esac
}

resolve_package() {
    local manifest_url manifest_ok checksums_url manifest_checksums
    manifest_ok=0
    VERSION_NUMBER="${TAG#v}"
    manifest_url="$RELEASE_ROOT/$REPO/releases/download/$TAG/release.json"

    step "Reading release metadata"
    if http_try "$manifest_url" "$TMP_DIR/release-manifest.json"; then
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

    if [ -f "$TARGET_PATH" ] && [ "$(sha256_file "$TARGET_PATH")" = "$EXPECTED_SHA" ]; then
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

main() {
    local uname_s uname_m

    parse_args "$@"
    validate_args

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

    check_prereqs
    TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/lv24-brain-install.XXXXXX")" || die "cannot create a temporary directory"
    trap cleanup EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM

    step "Installing brain for $OS/$ARCH into $INSTALL_DIR"
    resolve_release
    resolve_package
    install_binary
    verify_binary
    finish
}

main "$@"
