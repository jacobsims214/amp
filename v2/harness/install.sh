#!/bin/sh
# Install amp-harness from GitHub Releases (AMP KB decisions/amp-harness-architecture.md, D12).
#
#   curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh
#
# Downloads amp-harness-<version>-<target>.tar.gz and its .sha256 from the release tagged
# harness-v<version> on jacobsims214/amp, verifies the checksum, and installs the binary into
# ~/.local/bin (no sudo). Pass options through sh with `sh -s -- <options>`:
#
#   curl -fsSL .../install.sh | sh -s -- --version 0.1.0 --prefix ~/bin
#
# Options (each has an environment variable, for `curl | sh` without -s):
#   --version X.Y.Z   AMP_HARNESS_VERSION   release to install (default: the newest harness-v* release)
#   --prefix DIR      AMP_HARNESS_PREFIX    directory the binary goes in (default: ~/.local/bin)
#   --archive FILE    AMP_HARNESS_ARCHIVE   install a local archive instead of downloading; its checksum
#                                           is read from FILE.sha256 unless --sha256 is given
#   --sha256 HEX|FILE AMP_HARNESS_SHA256    expected SHA-256 of the archive (hex, or a .sha256 file)
#   --target TRIPLE   AMP_HARNESS_TARGET    override the detected platform (aarch64-apple-darwin, ...)
#   --modify-path                           if DIR is not on your login shell's PATH, append it to your
#                                           shell profile (a backup is kept). Off by default.
#   --no-path-check                         skip the login-shell PATH check
#   -h, --help
#
# AMP_HARNESS_BASE_URL overrides the download base (default
# https://github.com/jacobsims214/amp/releases/download); the files are fetched from
# <base>/harness-v<version>/<file>. It accepts file:// URLs, which is how the tests run offline.
#
# The whole script is a function called on the last line, so a truncated download runs nothing.

set -eu

REPO="jacobsims214/amp"
TAG_PREFIX="harness-v"
BIN="amp-harness"

say() { printf '%s\n' "$*"; }
err() { printf 'install.sh: error: %s\n' "$*" >&2; exit 1; }
warn() { printf 'install.sh: warning: %s\n' "$*" >&2; }
has() { command -v "$1" >/dev/null 2>&1; }

usage() {
    if [ -f "$0" ]; then
        sed -n '2,/^# The whole script/p' "$0" | sed -e 's/^# \{0,1\}//' -e '$d'
    else
        say "usage: install.sh [--version X.Y.Z] [--prefix DIR] [--archive FILE [--sha256 HEX|FILE]]"
        say "       [--target TRIPLE] [--modify-path] [--no-path-check]"
    fi
}

# Prints the Rust target triple for this machine, among the four we publish.
detect_target() {
    os=$(uname -s)
    arch=$(uname -m)
    case "$os" in
        Darwin)
            # A shell running under Rosetta reports x86_64 on Apple silicon; install the native build.
            if [ "$arch" = "x86_64" ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = "1" ]; then
                arch=arm64
            fi
            case "$arch" in
                arm64 | aarch64) echo aarch64-apple-darwin ;;
                x86_64) echo x86_64-apple-darwin ;;
                *) err "unsupported macOS architecture: $arch" ;;
            esac
            ;;
        Linux)
            case "$arch" in
                x86_64 | amd64) echo x86_64-unknown-linux-gnu ;;
                aarch64 | arm64) echo aarch64-unknown-linux-gnu ;;
                *) err "unsupported Linux architecture: $arch" ;;
            esac
            ;;
        *) err "unsupported OS: $os (amp-harness ships for macOS and Linux)" ;;
    esac
}

# fetch URL DEST
fetch() {
    if has curl; then
        curl --proto '=https,file' --tlsv1.2 -fsSL --retry 3 -o "$2" "$1" || return 1
    elif has wget; then
        case "$1" in file://*) cp "${1#file://}" "$2" || return 1 ;; *) wget -q -O "$2" "$1" || return 1 ;; esac
    else
        err "need curl or wget to download"
    fi
}

# Prints the SHA-256 of a file as lowercase hex.
sha256_of() {
    if has sha256sum; then
        sha256sum "$1" | cut -d' ' -f1
    elif has shasum; then
        shasum -a 256 "$1" | cut -d' ' -f1
    elif has openssl; then
        openssl dgst -sha256 "$1" | sed 's/^.*= *//'
    else
        err "need sha256sum, shasum or openssl to verify the download"
    fi
}

# Newest harness-v* tag from the GitHub API. The repo also publishes AMP releases, so GitHub's own
# "latest release" can't be used. Releases come newest first; drafts are not listed without auth.
latest_version() {
    api="https://api.github.com/repos/$REPO/releases?per_page=100"
    tmp_json="$1/releases.json"
    fetch "$api" "$tmp_json" || err "could not list releases from $api (pass --version X.Y.Z)"
    v=$(sed -n "s/.*\"tag_name\": *\"$TAG_PREFIX\\([^\"]*\\)\".*/\\1/p" "$tmp_json" | head -n 1)
    [ -n "$v" ] || err "no $TAG_PREFIX* release found on $REPO yet (pass --version, or --archive for a local build)"
    echo "$v"
}

# Prints the PATH a login shell sees. Shell profiles can print noise, so the value is read from a
# marked line instead of the whole output stream.
login_path() {
    sh_path=${SHELL:-/bin/sh}
    [ -x "$sh_path" ] || sh_path=/bin/sh
    # shellcheck disable=SC2016 # $PATH is expanded by the login shell, not here
    "$sh_path" -lc 'printf "\n__AMP_HARNESS_PATH__=%s\n" "$PATH"' </dev/null 2>/dev/null |
        sed -n 's/^__AMP_HARNESS_PATH__=//p' | tail -n 1
}

path_has() {
    case ":$1:" in *":$2:"*) return 0 ;; *) return 1 ;; esac
}

# The profile a login shell reads, for --modify-path.
profile_file() {
    case "$(basename "${SHELL:-sh}")" in
        zsh) echo "${ZDOTDIR:-$HOME}/.zprofile" ;;
        bash) if [ -f "$HOME/.bash_profile" ]; then echo "$HOME/.bash_profile"; else echo "$HOME/.profile"; fi ;;
        *) echo "$HOME/.profile" ;;
    esac
}

check_path() {
    prefix=$1
    modify=$2
    lp=$(login_path)
    if [ -z "$lp" ]; then
        warn "could not read your login shell's PATH; make sure $prefix is on it"
        return 0
    fi
    if path_has "$lp" "$prefix"; then
        # Something earlier on PATH could shadow the new binary.
        first=$(
            IFS=:
            for d in $lp; do
                if [ -x "$d/$BIN" ]; then
                    echo "$d/$BIN"
                    break
                fi
            done
        )
        if [ -n "$first" ] && [ "$first" != "$prefix/$BIN" ]; then
            warn "$first comes before $prefix on your PATH, so \`$BIN\` runs that one; remove it or reorder PATH"
        fi
        return 0
    fi
    line="export PATH=\"$prefix:\$PATH\""
    if [ "$modify" = 1 ]; then
        prof=$(profile_file)
        if [ -f "$prof" ]; then
            cp "$prof" "$prof.bak-amp-harness-install"
        fi
        {
            printf '\n# Added by the amp-harness installer (install.sh --modify-path).\n'
            printf '%s\n' "$line"
        } >>"$prof"
        say "Added $prefix to PATH in $prof (backup: $prof.bak-amp-harness-install if it existed)."
        say "Open a new terminal, or run: $line"
    else
        say ""
        say "$prefix is not on your login shell's PATH. Add it with:"
        say "  echo '$line' >> $(profile_file)"
        say "then open a new terminal (or re-run install.sh with --modify-path to do it for you)."
    fi
}

main() {
    version=${AMP_HARNESS_VERSION:-}
    prefix=${AMP_HARNESS_PREFIX:-}
    archive=${AMP_HARNESS_ARCHIVE:-}
    sha=${AMP_HARNESS_SHA256:-}
    target=${AMP_HARNESS_TARGET:-}
    base_url=${AMP_HARNESS_BASE_URL:-https://github.com/$REPO/releases/download}
    modify_path=0
    path_check=1

    while [ $# -gt 0 ]; do
        case "$1" in
            --version) [ $# -ge 2 ] || err "--version needs a value"; version=$2; shift 2 ;;
            --version=*) version=${1#*=}; shift ;;
            --prefix) [ $# -ge 2 ] || err "--prefix needs a value"; prefix=$2; shift 2 ;;
            --prefix=*) prefix=${1#*=}; shift ;;
            --archive) [ $# -ge 2 ] || err "--archive needs a value"; archive=$2; shift 2 ;;
            --archive=*) archive=${1#*=}; shift ;;
            --sha256) [ $# -ge 2 ] || err "--sha256 needs a value"; sha=$2; shift 2 ;;
            --sha256=*) sha=${1#*=}; shift ;;
            --target) [ $# -ge 2 ] || err "--target needs a value"; target=$2; shift 2 ;;
            --target=*) target=${1#*=}; shift ;;
            --modify-path) modify_path=1; shift ;;
            --no-path-check) path_check=0; shift ;;
            -h | --help) usage; exit 0 ;;
            *) err "unknown option: $1 (see --help)" ;;
        esac
    done

    [ -n "${HOME:-}" ] || err "HOME is not set"
    [ -n "$prefix" ] || prefix="$HOME/.local/bin"
    case "$prefix" in "~"/*) prefix="$HOME/${prefix#\~/}" ;; esac
    version=${version#v}
    version=${version#"$TAG_PREFIX"}

    has tar || err "need tar to unpack the archive"
    work=$(mktemp -d "${TMPDIR:-/tmp}/amp-harness-install.XXXXXX") || err "could not create a temp dir"
    # shellcheck disable=SC2064 # expand $work now
    trap "rm -rf '$work'" EXIT
    trap 'exit 130' INT TERM

    if [ -n "$archive" ]; then
        [ -f "$archive" ] || err "archive not found: $archive"
        if [ -z "$sha" ]; then
            [ -f "$archive.sha256" ] || err "no checksum for $archive: expected $archive.sha256, or pass --sha256"
            sha="$archive.sha256"
        fi
        say "Installing $BIN from $archive"
    else
        [ -n "$target" ] || target=$(detect_target)
        [ -n "$version" ] || version=$(latest_version "$work")
        name="$BIN-$version-$target.tar.gz"
        url="$base_url/$TAG_PREFIX$version/$name"
        say "Downloading $BIN $version for $target"
        say "  $url"
        fetch "$url" "$work/$name" || err "download failed: $url"
        if [ -z "$sha" ]; then
            fetch "$url.sha256" "$work/$name.sha256" || err "checksum download failed: $url.sha256"
            sha="$work/$name.sha256"
        fi
        archive="$work/$name"
    fi

    # --sha256 is a hex digest or a file whose first word is one (sha256sum's format).
    if [ -f "$sha" ]; then
        expected=$(head -n 1 "$sha" | cut -d' ' -f1)
    else
        expected=$sha
    fi
    expected=$(printf '%s' "$expected" | tr 'A-F' 'a-f')
    case "$expected" in
        *[!0-9a-f]* | "") err "not a SHA-256 digest: $expected" ;;
    esac
    [ ${#expected} -eq 64 ] || err "not a SHA-256 digest (want 64 hex chars): $expected"
    actual=$(sha256_of "$archive")
    if [ "$actual" != "$expected" ]; then
        err "checksum mismatch for $(basename "$archive"): expected $expected, got $actual. Nothing was installed."
    fi
    say "Checksum OK ($actual)"

    mkdir "$work/x"
    tar -xzf "$archive" -C "$work/x" || err "could not unpack $archive"
    found=$(find "$work/x" -type f -name "$BIN" | head -n 1)
    [ -n "$found" ] || err "$BIN not found inside $(basename "$archive")"

    mkdir -p "$prefix" 2>/dev/null || err "could not create $prefix (pick another with --prefix; no sudo is needed for a directory you own)"
    [ -w "$prefix" ] || err "$prefix is not writable (pick another with --prefix)"
    # Copy then rename, so a running amp-harness is never overwritten in place.
    cp "$found" "$prefix/.$BIN.new"
    chmod 0755 "$prefix/.$BIN.new"
    mv -f "$prefix/.$BIN.new" "$prefix/$BIN"

    out=$("$prefix/$BIN" --version 2>/dev/null) ||
        err "installed $prefix/$BIN but it does not run on this machine (wrong --target?)"
    installed=$(printf '%s\n' "$out" | head -n 1)
    say "Installed $installed to $prefix/$BIN"

    if [ "$path_check" = 1 ]; then
        check_path "$prefix" "$modify_path"
    fi

    say ""
    say "Next: run \`$BIN\` in your project directory. The first run signs you in to AMP, picks or"
    say "creates a project, and stores a model provider key. \`$BIN doctor\` checks the setup."
}

main "$@"
