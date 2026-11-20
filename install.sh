#!/bin/sh
set -eu

fail() {
    printf 'adg installer: %s\n' "$1" >&2
    exit 1
}

latest_release_tag() {
    # The public releases list includes prereleases and excludes drafts.
    release=$(curl -fsSL 'https://api.github.com/repos/WaferJay/adbgroup/releases?per_page=1') || return 1
    # Read the single release's ASCII tag from either compact or indented JSON.
    printf '%s\n' "$release" | awk -v RS='"' '
        $0 == "tag_name" {
            if ((getline) > 0 && $0 ~ /^[[:space:]]*:[[:space:]]*$/ && (getline) > 0) {
                print
                exit
            }
        }'
}

same_binary() {
    [ -f "$1" ] && [ -x "$1" ] && cmp -s "$1" "$2"
}

report_path() {
    selected=$(command -v adg 2>/dev/null || true)
    if [ -n "$selected" ] && [ "$selected" -ef "$1" ]; then
        return
    fi
    if [ -n "$selected" ]; then
        printf 'PATH currently selects %s.\n' "$selected"
    fi
    printf 'To use %s, put %s first on PATH.\n' "$1" "${1%/*}"
}

main() {
    requested_tag=
    requested_dir=
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --tag|--bin-dir)
                [ -n "${2:-}" ] || fail "$1 requires a value"
                case "$1" in
                    --tag) requested_tag=$2 ;;
                    --bin-dir) requested_dir=$2 ;;
                esac
                shift 2 ;;
            -h|--help)
                printf '%s\n' 'Usage: sh install.sh [--tag TAG] [--bin-dir DIR]' \
                    'Install the newest published GitHub release, including prereleases, in ~/.local/bin.' \
                    'Use --tag to install a specific release.'
                return ;;
            *) fail "unknown option: $1" ;;
        esac
    done

    bin_dir=${requested_dir:-${HOME:?Set HOME or pass --bin-dir}/.local/bin}
    case "$bin_dir" in
        /*) ;;
        *) bin_dir=$PWD/$bin_dir ;;
    esac
    target=${bin_dir%/}/adg

    platform=$(uname -s)
    case "$platform" in
        Darwin) platform=darwin ;;
        Linux) platform=linux ;;
        *) fail "unsupported operating system: $platform" ;;
    esac
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64) arch=amd64 ;;
        arm64|aarch64) arch=arm64 ;;
        *) fail "unsupported architecture: $arch" ;;
    esac

    [ ! -d "$target" ] || fail "install target is a directory: $target"
    tag=$requested_tag
    if [ -z "$tag" ]; then
        tag=$(latest_release_tag) || fail 'could not query GitHub releases'
        [ -n "$tag" ] || fail 'no published release found on GitHub'
    fi
    case "$tag" in
        *[!a-zA-Z0-9.+-]*) fail "invalid release tag: $tag" ;;
        v[0-9]*) ;;
        *) fail "expected a version tag beginning with v: $tag" ;;
    esac
    archive=adbgroup_${tag#v}_${platform}_${arch}.tar.gz
    release_url=https://github.com/WaferJay/adbgroup/releases/download/$tag
    mkdir -p "$bin_dir"
    staging=$(mktemp -d "$bin_dir/.adg-install.XXXXXXXX")
    trap 'rm -rf "$staging"' 0
    trap 'exit 1' HUP INT TERM

    curl -fsSL -o "$staging/checksums.txt" "$release_url/checksums.txt"
    expected=$(awk -v file="$archive" '$2 == file { print $1 }' "$staging/checksums.txt")
    [ -n "$expected" ] || fail "no checksum for $archive"
    curl -fsSL -o "$staging/$archive" "$release_url/$archive"
    if command -v sha256sum >/dev/null 2>&1; then
        actual=$(sha256sum < "$staging/$archive")
    elif command -v shasum >/dev/null 2>&1; then
        actual=$(shasum -a 256 < "$staging/$archive")
    else
        fail 'install sha256sum or shasum to verify the download'
    fi
    [ "${actual%% *}" = "$expected" ] || fail "SHA-256 mismatch for $archive"

    tar -xzOf "$staging/$archive" adg > "$staging/adg"
    chmod 755 "$staging/adg"
    "$staging/adg" --help >/dev/null 2>&1 || fail 'downloaded adg failed its startup check'
    if [ -z "$requested_dir" ] && [ -z "$requested_tag" ]; then
        current=$(command -v adg 2>/dev/null || true)
        if same_binary "$current" "$staging/adg"; then
            printf 'adg %s is already installed at %s\n' "$tag" "$current"
            return
        fi
    fi
    if same_binary "$target" "$staging/adg"; then
        printf 'adg %s is already installed at %s\n' "$tag" "$target"
        report_path "$target"
        return
    fi
    mv -f "$staging/adg" "$target"
    printf 'Installed adg %s at %s\n' "$tag" "$target"
    report_path "$target"
}

main "$@"
