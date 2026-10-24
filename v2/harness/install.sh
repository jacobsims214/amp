#!/bin/sh
# Install amp-harness on macOS or Linux (AMP KB decisions/amp-harness-architecture.md, D12; #2115, #2276).
#
#   curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh
#
# With your company's preset (its AMP server, model endpoint and models per role; no secrets):
#
#   curl -fsSL https://raw.githubusercontent.com/jacobsims214/amp/main/v2/harness/install.sh | sh -s -- --preset https://intranet.example.com/amp-preset.toml
#
# Downloads amp-harness-<version>-<target>.tar.gz and its .sha256 from the release tagged
# harness-v<version> on jacobsims214/amp, verifies the checksum, and installs the binary into
# ~/.local/bin (no sudo). Re-running it upgrades in place. On a terminal it then offers to run
# `amp-harness setup` (sign in to AMP, pick a project, connect a model provider) for the current
# folder; this works under `curl | sh` too, because the answer is read from /dev/tty. Pass options
# with `sh -s -- <options>`.
#
# Options (most have an environment variable, for `curl | sh` without -s):
#   --preset FILE|URL AMP_HARNESS_PRESET    hand this org preset to `amp-harness setup --preset`
#   --no-setup        AMP_HARNESS_NO_SETUP=1  install only; print the setup command instead of offering it
#   --version X.Y.Z   AMP_HARNESS_VERSION   release to install (default: the newest harness-v* release)
#   --prefix DIR      AMP_HARNESS_PREFIX    directory the binary goes in (default: ~/.local/bin)
#   --uninstall                             remove DIR/amp-harness (your config and saved keys stay)
#   --archive FILE    AMP_HARNESS_ARCHIVE   install a local archive instead of downloading; its checksum
#                                           is read from FILE.sha256 unless --sha256 is given
#   --sha256 HEX|FILE AMP_HARNESS_SHA256    expected SHA-256 of the archive (hex, or a .sha256 file)
#   --target TRIPLE   AMP_HARNESS_TARGET    override the detected platform (aarch64-apple-darwin, ...)
#   --modify-path                           if DIR is not on your login shell's PATH, append it to your
#                                           shell profile (a backup is kept). Off by default.
#   --no-path-check                         skip the login-shell PATH check
#   -h, --help
#
# Platforms: macOS 13+ (Apple silicon or Intel) and Linux on x86_64 or aarch64 with glibc 2.35 or
# newer (Ubuntu 22.04+, Debian 12+, Fedora, RHEL 10, ...). Needs curl or wget, tar, and sha256sum,
# shasum or openssl. Alpine and other musl distros are not supported yet.
#
# AMP_HARNESS_BASE_URL overrides the download base (default
# https://github.com/jacobsims214/amp/releases/download); the files are fetched from
# <base>/harness-v<version>/<file>. AMP_HARNESS_RELEASES_URL overrides the release list used to find
# the newest version (default: the GitHub API). Both accept file:// URLs, which is how the tests run
# offline.
#
# The whole script is a function called on the last line, so a truncated download runs nothing.

set -eu

REPO="jacobsims214/amp"
TAG_PREFIX="harness-v"
BIN="amp-harness"
GLIBC_MIN="2.35" # the Linux builds are made on ubuntu-22.04

say() { printf '%s\n' "$*"; }
err() { printf 'install.sh: error: %s\n' "$*" >&2; exit 1; }
warn() { printf 'install.sh: warning: %s\n' "$*" >&2; }
has() { command -v "$1" >/dev/null 2>&1; }

usage() {
    if [ -f "$0" ]; then
        sed -n '2,/^# The whole script/p' "$0" | sed -e 's/^# \{0,1\}//' -e '$d'
    else
        say "usage: install.sh [--preset FILE|URL] [--no-setup] [--version X.Y.Z] [--prefix DIR] [--uninstall]"
        say "       [--archive FILE [--sha256 HEX|FILE]] [--target TRIPLE] [--modify-path] [--no-path-check]"
    fi
}

# Linux only: refuse musl (Alpine) and a glibc older than the one the release is built against, with
# a message, before anything is downloaded.
check_libc() {
    [ "$(uname -s)" = Linux ] || return 0
    glibc=""
    if has getconf; then
        glibc=$(getconf GNU_LIBC_VERSION 2>/dev/null | sed -n 's/^glibc //p') || glibc=""
    fi
    if [ -z "$glibc" ]; then
        if ls /lib/ld-musl-* >/dev/null 2>&1 || { has ldd && ldd --version 2>&1 | grep -qi musl; }; then
            err "this Linux uses musl libc (Alpine or similar), and amp-harness ships glibc builds only for now. Use a glibc distro or container (Debian, Ubuntu, Fedora, ...)."
        fi
        warn "could not tell which C library this Linux uses; amp-harness needs glibc $GLIBC_MIN or newer"
        return 0
    fi
    if has awk && ! printf '%s %s\n' "$glibc" "$GLIBC_MIN" | awk '{
            split($1, h, "."); split($2, m, ".")
            exit !(h[1] + 0 > m[1] + 0 || (h[1] + 0 == m[1] + 0 && h[2] + 0 >= m[2] + 0))
        }'; then
        err "amp-harness needs glibc $GLIBC_MIN or newer, and this system has glibc $glibc (Ubuntu 22.04+, Debian 12+ and Fedora 36+ are new enough)"
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
                *) err "unsupported Linux architecture: $arch (amp-harness ships for x86_64 and aarch64)" ;;
            esac
            ;;
        *) err "unsupported OS: $os (amp-harness ships for macOS and Linux)" ;;
    esac
}

need_downloader() {
    has curl || has wget ||
        err "need curl or wget to download amp-harness; install one with your package manager (e.g. apt-get install curl), or pass --archive FILE"
}

# fetch URL DEST
fetch() {
    if has curl; then
        curl --proto '=https,file' --tlsv1.2 -fsSL --retry 3 -o "$2" "$1" || return 1
    elif has wget; then
        case "$1" in file://*) cp "${1#file://}" "$2" || return 1 ;; *) wget -q -O "$2" "$1" || return 1 ;; esac
    else
        need_downloader
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
        err "need sha256sum, shasum or openssl to verify the download (install coreutils or openssl)"
    fi
}

# Newest harness-v* tag from the GitHub API. The repo also publishes AMP releases, so GitHub's own
# "latest release" can't be used. Releases come newest first; drafts are not listed without auth.
latest_version() {
    api=${AMP_HARNESS_RELEASES_URL:-"https://api.github.com/repos/$REPO/releases?per_page=100"}
    tmp_json="$1/releases.json"
    fetch "$api" "$tmp_json" || err "could not list releases from $api (pass --version X.Y.Z)"
    v=$(sed -n "s/.*\"tag_name\": *\"$TAG_PREFIX\\([^\"]*\\)\".*/\\1/p" "$tmp_json" | head -n 1)
    [ -n "$v" ] || err "no $TAG_PREFIX* release found on $REPO yet (pass --version, or --archive for a local build)"
    echo "$v"
}

# The version an amp-harness binary reports (second word of `--version`'s first line), or nothing.
version_of() {
    [ -x "$1" ] || return 0
    "$1" --version 2>/dev/null | head -n 1 | cut -d' ' -f2 || true
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

# Sets on_path=1 when the prefix is on the login shell's PATH (or was just added to it).
check_path() {
    prefix=$1
    modify=$2
    lp=$(login_path)
    if [ -z "$lp" ]; then
        warn "could not read your login shell's PATH; make sure $prefix is on it"
        return 0
    fi
    if path_has "$lp" "$prefix"; then
        on_path=1
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
            on_path=0
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

# True when we can talk to a person: stdout is a terminal and a controlling terminal can be opened
# for the answer (stdin is the script itself under `curl | sh`).
interactive() {
    [ -t 1 ] || return 1
    { : </dev/tty; } 2>/dev/null
}

# Runs a command with the terminal as its stdin: stdin itself when it is one, else /dev/tty (stdin
# is the script under `curl | sh`). amp-harness reads keys from either (crossterm's use-dev-tty,
# #2330: mio's kqueue can't poll a /dev/tty fd on macOS), but the person's own terminal is the
# plainer hand-off when there is one.
on_tty() {
    if [ -t 0 ]; then "$@"; else "$@" </dev/tty; fi
}

# Quotes one word for a command line shown to the person (single quotes only when needed).
shell_word() {
    case "$1" in
        *[!A-Za-z0-9_./:=@%+,-]* | "") printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")" ;;
        *) printf '%s' "$1" ;;
    esac
}

uninstall() {
    prefix=$1
    path="$prefix/$BIN"
    if [ -e "$path" ] || [ -L "$path" ]; then
        old=$(version_of "$path")
        rm -f "$path" || err "could not remove $path"
        say "Removed $BIN${old:+ $old} from $path."
    else
        say "No $BIN in $prefix; nothing to remove (pass --prefix DIR if you installed it elsewhere)."
    fi
    if other=$(command -v "$BIN" 2>/dev/null) && [ -n "$other" ] && [ "$other" != "$path" ]; then
        say "Another $BIN is still on your PATH: $other (Homebrew: brew uninstall $BIN)."
    fi
    config_dir=${AMP_HARNESS_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/amp-harness}
    say ""
    say "Kept, in case you reinstall:"
    say "  - your settings: $config_dir (config.toml, and credentials.json if keys were kept in a file)"
    case "$(uname -s)" in
        Darwin)
            say "  - saved keys and AMP logins: your login keychain, items with service \"$BIN\""
            say "    (Keychain Access; or \`security delete-generic-password -s $BIN\` once per item)"
            ;;
        *)
            say "  - saved keys and AMP logins: your Secret Service keyring, items with service \"$BIN\""
            say "    (Passwords and Keys / seahorse; or \`secret-tool clear service $BIN\`)"
            ;;
    esac
    say "  - each project's .amp.json and .amp/harness/ (they belong to the project)"
    prof=$(profile_file)
    if [ -f "$prof" ] && grep -q "Added by the amp-harness installer" "$prof" 2>/dev/null; then
        say "  - the PATH line install.sh --modify-path added to $prof"
    fi
    say "Delete those yourself to remove everything."
}

main() {
    version=${AMP_HARNESS_VERSION:-}
    prefix=${AMP_HARNESS_PREFIX:-}
    archive=${AMP_HARNESS_ARCHIVE:-}
    sha=${AMP_HARNESS_SHA256:-}
    target=${AMP_HARNESS_TARGET:-}
    preset=${AMP_HARNESS_PRESET:-}
    base_url=${AMP_HARNESS_BASE_URL:-https://github.com/$REPO/releases/download}
    modify_path=0
    path_check=1
    run_setup=1
    case "${AMP_HARNESS_NO_SETUP:-}" in "" | 0 | false | no) ;; *) run_setup=0 ;; esac
    do_uninstall=0

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
            --preset) [ $# -ge 2 ] || err "--preset needs a file or URL"; preset=$2; shift 2 ;;
            --preset=*) preset=${1#*=}; shift ;;
            --no-setup) run_setup=0; shift ;;
            --uninstall) do_uninstall=1; shift ;;
            --modify-path) modify_path=1; shift ;;
            --no-path-check) path_check=0; shift ;;
            -h | --help) usage; exit 0 ;;
            *) err "unknown option: $1 (see --help)" ;;
        esac
    done

    [ -n "${HOME:-}" ] || err "HOME is not set"
    [ -n "$prefix" ] || prefix="$HOME/.local/bin"
    case "$prefix" in "~"/*) prefix="$HOME/${prefix#\~/}" ;; esac

    if [ "$do_uninstall" = 1 ]; then
        uninstall "$prefix"
        return 0
    fi

    # A preset file is checked now (and made absolute, so the printed command works from anywhere);
    # a URL is fetched and checked by `amp-harness setup` itself.
    case "$preset" in
        "" | https://* | http://*) ;;
        *://*) err "--preset takes a file or an https:// URL: $preset" ;;
        *)
            case "$preset" in "~"/*) preset="$HOME/${preset#\~/}" ;; esac
            [ -f "$preset" ] || err "preset file not found: $preset"
            case "$preset" in /*) ;; *) preset="$(cd "$(dirname "$preset")" && pwd)/$(basename "$preset")" ;; esac
            ;;
    esac

    version=${version#v}
    version=${version#"$TAG_PREFIX"}

    [ -n "$target" ] || check_libc
    has tar || err "need tar to unpack the archive; install it with your package manager"
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
        need_downloader
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
    tar -xzf "$archive" -C "$work/x" || err "could not unpack $archive (is gzip installed?)"
    found=$(find "$work/x" -type f -name "$BIN" | head -n 1)
    [ -n "$found" ] || err "$BIN not found inside $(basename "$archive")"

    mkdir -p "$prefix" 2>/dev/null || err "could not create $prefix (pick another with --prefix; no sudo is needed for a directory you own)"
    [ -w "$prefix" ] || err "$prefix is not writable (pick another with --prefix)"
    old=$(version_of "$prefix/$BIN")
    # Copy then rename, so a running amp-harness is never overwritten in place.
    cp "$found" "$prefix/.$BIN.new"
    chmod 0755 "$prefix/.$BIN.new"
    mv -f "$prefix/.$BIN.new" "$prefix/$BIN"

    out=$("$prefix/$BIN" --version 2>/dev/null) ||
        err "installed $prefix/$BIN but it does not run on this machine (wrong --target?)"
    new=$(printf '%s\n' "$out" | head -n 1 | cut -d' ' -f2)
    if [ -z "$old" ]; then
        say "Installed $BIN $new to $prefix/$BIN"
    elif [ "$old" = "$new" ]; then
        say "Reinstalled $BIN $new at $prefix/$BIN (already up to date)"
    else
        say "Updated $BIN $old → $new at $prefix/$BIN"
    fi

    on_path=0
    if [ "$path_check" = 1 ]; then
        check_path "$prefix" "$modify_path"
    fi

    # The command the person runs next: `amp-harness` when the login PATH finds this one.
    if [ "$on_path" = 1 ]; then cmd=$BIN; else cmd=$(shell_word "$prefix/$BIN"); fi
    setup_cmd="$cmd setup"
    [ -z "$preset" ] || setup_cmd="$setup_cmd --preset $(shell_word "$preset")"

    say ""
    if [ "$run_setup" = 1 ] && interactive; then
        say "Setup signs you in to AMP, links this folder ($PWD) to a project and connects a model"
        say "provider${preset:+, starting from the preset $preset}."
        printf "Run \`%s\` now? [Y/n] " "$setup_cmd"
        answer=""
        read -r answer </dev/tty || answer=n
        case "$answer" in
            "" | [yY] | [yY][eE][sS])
                say ""
                status=0
                if [ -n "$preset" ]; then
                    on_tty "$prefix/$BIN" setup --preset "$preset" || status=$?
                else
                    on_tty "$prefix/$BIN" setup || status=$?
                fi
                if [ "$status" != 0 ]; then
                    say ""
                    say "Setup did not finish (exit ${status}). $BIN is installed; run \`$setup_cmd\` to try again."
                    exit "$status"
                fi
                return 0
                ;;
        esac
        say "Skipped. Run \`$setup_cmd\` when you're ready; \`$cmd doctor\` checks the setup."
        return 0
    fi
    say "Next: run \`$setup_cmd\` in your project directory. It signs you in to AMP, links the folder to"
    say "a project and connects a model provider. \`$cmd doctor\` checks the setup."
}

main "$@"
