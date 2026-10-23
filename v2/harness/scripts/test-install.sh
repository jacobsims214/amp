#!/bin/sh
# Offline tests for install.sh, package.sh and render-formula.sh (#2115, #2276). `make install-test`
# runs it; so does CI, on Linux and macOS. Everything happens under target/install-test with HOME,
# XDG_CONFIG_HOME and TMPDIR pointed there, so the real ~/.local/bin, ~/.config/amp-harness, shell
# profiles and keychain are never touched. A stub binary stands in for amp-harness (it logs the
# `setup` it is handed instead of running one) unless AMP_HARNESS_TEST_ARCHIVE names a real archive
# (with its .sha256 beside it), which is then installed and run too. The terminal cases run
# install.sh on a pseudo-terminal through scripts/pty-run.py (python3); without python3 they are
# skipped with a note.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
T="$here/target/install-test"
rm -rf "$T"
mkdir -p "$T/home" "$T/tmp" "$T/stub"
export HOME="$T/home" TMPDIR="$T/tmp" SHELL=/bin/sh XDG_CONFIG_HOME="$T/home/.config"
unset AMP_HARNESS_VERSION AMP_HARNESS_PREFIX AMP_HARNESS_ARCHIVE AMP_HARNESS_SHA256 AMP_HARNESS_TARGET \
    AMP_HARNESS_BASE_URL AMP_HARNESS_RELEASES_URL AMP_HARNESS_PRESET AMP_HARNESS_NO_SETUP AMP_HARNESS_CONFIG_DIR
# The stub appends each `setup` it is handed (its arguments, and whether stdin is a terminal) here.
export STUB_LOG="$T/setup.log"

install_sh="$here/install.sh"
version=$(sed -n '/^\[workspace.package\]/,/^\[/s/^version = "\(.*\)"/\1/p' "$here/Cargo.toml" | head -n 1)
tgt=x86_64-unknown-linux-gnu
pass=0
fail=0

ok() { pass=$((pass + 1)); echo "ok   - $1"; }
bad() { fail=$((fail + 1)); echo "FAIL - $1"; [ -f "$T/out" ] && sed 's/^/       /' "$T/out"; }
# expect_ok NAME CMD...   /  expect_fail NAME CMD...   (output kept in $T/out)
expect_ok() { n=$1; shift; if "$@" >"$T/out" 2>&1; then ok "$n"; else bad "$n"; fi; }
expect_fail() { n=$1; shift; if "$@" >"$T/out" 2>&1; then bad "$n (should have failed)"; else ok "$n"; fi; }
out_has() { if grep -qF -- "$2" "$T/out"; then ok "$1"; else bad "$1: no '$2' in output"; fi; }

log_has() { if grep -qF -- "$2" "$STUB_LOG" 2>/dev/null; then ok "$1"; else bad "$1: no '$2' in setup.log"; [ -f "$STUB_LOG" ] && sed 's/^/       log: /' "$STUB_LOG"; fi; }
no_setup_ran() { if [ -s "$STUB_LOG" ]; then bad "$1: setup ran"; sed 's/^/       log: /' "$STUB_LOG"; else ok "$1"; fi; }

# stub DIR VERSION: an amp-harness stand-in reporting VERSION. `setup`/`init` log and exit with
# STUB_SETUP_EXIT (default 0); anything else prints the version line.
stub() {
    mkdir -p "$1"
    cat >"$1/amp-harness" <<STUB
#!/bin/sh
case "\${1:-}" in
    setup | init)
        {
            printf 'setup:'
            for a in "\$@"; do printf ' [%s]' "\$a"; done
            if [ -t 0 ]; then echo ' stdin=tty'; else echo ' stdin=other'; fi
        } >>"\$STUB_LOG"
        echo "stub setup ran"
        exit "\${STUB_SETUP_EXIT:-0}"
        ;;
esac
echo "amp-harness $2 (stub)"
STUB
    chmod +x "$1/amp-harness"
}
stub "$T/stub" "$version"

# package.sh: archive + sha256sum-format checksum.
archive=$("$here/scripts/package.sh" --bin "$T/stub/amp-harness" --target "$tgt" --out "$T/dist" 2>/dev/null | tail -n 1)
name="amp-harness-$version-$tgt.tar.gz"
if [ "$archive" = "$T/dist/$name" ] && [ -f "$archive" ] && [ -f "$archive.sha256" ]; then ok "package.sh writes $name + .sha256"; else bad "package.sh output: $archive"; fi
if tar -tzf "$archive" | grep -qx "amp-harness-$version-$tgt/amp-harness" && tar -tzf "$archive" | grep -qx "amp-harness-$version-$tgt/LICENSE"; then ok "archive layout"; else bad "archive layout"; fi
if grep -q "  $name\$" "$archive.sha256"; then ok "checksum file names the archive"; else bad "checksum file format"; fi

# Local archive install.
expect_ok "install --archive" sh "$install_sh" --archive "$archive" --prefix "$T/p1" --no-path-check
out_has "reports checksum OK" "Checksum OK"
out_has "reports a fresh install" "Installed amp-harness $version to $T/p1/amp-harness"
out_has "prints the setup step (no terminal)" "Next: run \`$T/p1/amp-harness setup\`"
no_setup_ran "no terminal: setup is not started"
if [ "$("$T/p1/amp-harness")" = "amp-harness $version (stub)" ]; then ok "installed binary runs"; else bad "installed binary"; fi
if [ -z "$(find "$T/p1" -name amp-harness -perm -0755)" ]; then bad "mode 0755"; else ok "mode 0755"; fi
expect_ok "reinstall over an existing binary" sh "$install_sh" --archive "$archive" --prefix "$T/p1" --no-path-check
out_has "says it is already up to date" "Reinstalled amp-harness $version at $T/p1/amp-harness (already up to date)"

# Checksum failures install nothing.
cp "$archive" "$T/tampered.tar.gz"
cp "$archive.sha256" "$T/tampered.tar.gz.sha256"
printf 'x' >>"$T/tampered.tar.gz"
expect_fail "tampered archive is rejected" sh "$install_sh" --archive "$T/tampered.tar.gz" --prefix "$T/p2" --no-path-check
out_has "says checksum mismatch" "checksum mismatch"
if [ -e "$T/p2/amp-harness" ]; then bad "nothing installed after mismatch"; else ok "nothing installed after mismatch"; fi
cp "$archive" "$T/nosum.tar.gz"
expect_fail "archive without .sha256 is rejected" sh "$install_sh" --archive "$T/nosum.tar.gz" --prefix "$T/p2" --no-path-check
hex=$(cut -d' ' -f1 <"$archive.sha256")
expect_ok "--sha256 HEX" sh "$install_sh" --archive "$T/nosum.tar.gz" --sha256 "$hex" --prefix "$T/p3" --no-path-check
expect_fail "--sha256 garbage" sh "$install_sh" --archive "$T/nosum.tar.gz" --sha256 nothex --prefix "$T/p3" --no-path-check

# Download path, from a file:// mirror of the GitHub Releases layout.
rel="$T/releases/harness-v$version"
mkdir -p "$rel"
cp "$archive" "$archive.sha256" "$rel/"
export AMP_HARNESS_BASE_URL="file://$T/releases"
expect_ok "download + verify (--version v$version)" sh "$install_sh" --version "v$version" --target "$tgt" --prefix "$T/p4" --no-path-check
out_has "download URL uses the harness-v tag" "harness-v$version/$name"
if [ -x "$T/p4/amp-harness" ]; then ok "downloaded binary installed"; else bad "downloaded binary installed"; fi
expect_ok "env-var options (AMP_HARNESS_VERSION/_PREFIX/_TARGET)" env AMP_HARNESS_VERSION="$version" AMP_HARNESS_TARGET="$tgt" AMP_HARNESS_PREFIX="$T/p5" sh "$install_sh" --no-path-check
expect_ok "curl | sh style (script on stdin)" sh -c "cat '$install_sh' | sh -s -- --version $version --target $tgt --prefix '$T/p6' --no-path-check"
printf '%064d  %s\n' 0 "$name" >"$rel/$name.sha256"
expect_fail "download with a wrong published checksum is rejected" sh "$install_sh" --version "$version" --target "$tgt" --prefix "$T/p7" --no-path-check
expect_fail "missing release is an error" sh "$install_sh" --version 0.0.0-nope --target "$tgt" --prefix "$T/p7" --no-path-check
unset AMP_HARNESS_BASE_URL

# Login-shell PATH check (SHELL=/bin/sh reads $HOME/.profile; HOME is the test home).
expect_ok "install with prefix not on PATH" sh "$install_sh" --archive "$archive" --prefix "$T/p8"
out_has "explains how to add it to PATH" "is not on your login shell's PATH"
if [ -e "$HOME/.profile" ]; then bad "profile untouched without --modify-path"; else ok "profile untouched without --modify-path"; fi
echo "# existing profile" >"$HOME/.profile"
expect_ok "--modify-path" sh "$install_sh" --archive "$archive" --prefix "$T/p8" --modify-path
if grep -q "export PATH=\"$T/p8:" "$HOME/.profile" && [ -f "$HOME/.profile.bak-amp-harness-install" ]; then ok "profile appended with a backup"; else bad "profile appended with a backup"; fi
expect_ok "re-install once on PATH" sh "$install_sh" --archive "$archive" --prefix "$T/p8"
if grep -q "not on your login shell's PATH" "$T/out"; then bad "login PATH now includes the prefix"; else ok "login PATH now includes the prefix"; fi
# Once the prefix is on the login PATH, the next step names plain `amp-harness`.
out_has "next step uses amp-harness once on PATH" "Next: run \`amp-harness setup\`"

# Upgrades: a file:// mirror with an older and the current release, and a release list (newest
# first, with a non-harness AMP tag on top) to find the newest harness-v* release.
sum() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }
old=0.0.1
mirror="$T/mirror"
for v in "$old" "$version"; do
    d="$T/build-$v"
    stub "$d/amp-harness-$v-$tgt" "$v"
    mkdir -p "$mirror/harness-v$v"
    tar -czf "$mirror/harness-v$v/amp-harness-$v-$tgt.tar.gz" -C "$d" "amp-harness-$v-$tgt"
    printf '%s  %s\n' "$(sum "$mirror/harness-v$v/amp-harness-$v-$tgt.tar.gz")" "amp-harness-$v-$tgt.tar.gz" \
        >"$mirror/harness-v$v/amp-harness-$v-$tgt.tar.gz.sha256"
done
cat >"$mirror/releases.json" <<JSON
[
  {
    "tag_name": "v9.9.9",
    "name": "AMP 9.9.9"
  },
  {
    "tag_name": "harness-v$version",
    "name": "amp-harness $version"
  },
  {
    "tag_name": "harness-v$old",
    "name": "amp-harness $old"
  }
]
JSON
export AMP_HARNESS_BASE_URL="file://$mirror" AMP_HARNESS_RELEASES_URL="file://$mirror/releases.json"
expect_ok "install an older release" sh "$install_sh" --version "$old" --target "$tgt" --prefix "$T/up" --no-path-check
out_has "older release installed" "Installed amp-harness $old to $T/up/amp-harness"
expect_ok "re-run upgrades in place" sh "$install_sh" --target "$tgt" --prefix "$T/up" --no-path-check
out_has "the newest harness-v release is picked" "Downloading amp-harness $version for $tgt"
out_has "says old → new" "Updated amp-harness $old → $version at $T/up/amp-harness"
if [ "$("$T/up/amp-harness" --version)" = "amp-harness $version (stub)" ]; then ok "upgraded binary runs"; else bad "upgraded binary runs"; fi
expect_ok "re-run with curl | sh when up to date" sh -c "cat '$install_sh' | sh -s -- --target $tgt --prefix '$T/up' --no-path-check"
out_has "says already up to date" "Reinstalled amp-harness $version at $T/up/amp-harness (already up to date)"
unset AMP_HARNESS_BASE_URL AMP_HARNESS_RELEASES_URL

# --uninstall removes only the binary.
mkdir -p "$XDG_CONFIG_HOME/amp-harness"
echo '# kept' >"$XDG_CONFIG_HOME/amp-harness/config.toml"
expect_ok "--uninstall" sh "$install_sh" --uninstall --prefix "$T/up"
if [ -e "$T/up/amp-harness" ]; then bad "binary removed"; else ok "binary removed"; fi
out_has "names what it removed" "Removed amp-harness $version from $T/up/amp-harness."
out_has "says the settings are kept, and where" "your settings: $XDG_CONFIG_HOME/amp-harness"
out_has "says the saved keys are kept, and where" "items with service \"amp-harness\""
if [ -f "$XDG_CONFIG_HOME/amp-harness/config.toml" ]; then ok "config left in place"; else bad "config left in place"; fi
expect_ok "--uninstall again is a no-op" sh -c "cat '$install_sh' | sh -s -- --uninstall --prefix '$T/up'"
out_has "says there is nothing to remove" "No amp-harness in $T/up; nothing to remove"

# --preset without a terminal: checked, made absolute, and printed in the next step.
mkdir -p "$T/work"
printf '[preset]\nname = "Example"\n' >"$T/work/preset.toml"
: >"$STUB_LOG"
expect_ok "--preset FILE (relative)" sh -c "cd '$T/work' && sh '$install_sh' --archive '$archive' --prefix '$T/ps' --no-path-check --preset preset.toml"
out_has "next step passes the preset, made absolute" "Next: run \`$T/ps/amp-harness setup --preset $T/work/preset.toml\`"
no_setup_ran "no terminal: setup is not started with a preset either"
expect_ok "AMP_HARNESS_PRESET=URL" env AMP_HARNESS_PRESET=https://intranet.example.com/amp-preset.toml sh "$install_sh" --archive "$archive" --prefix "$T/ps" --no-path-check
out_has "next step passes the preset URL" "setup --preset https://intranet.example.com/amp-preset.toml\`"
expect_fail "a missing preset file is refused first" sh "$install_sh" --archive "$archive" --prefix "$T/ps2" --no-path-check --preset "$T/work/nope.toml"
out_has "says the preset is missing" "preset file not found: $T/work/nope.toml"
if [ -e "$T/ps2/amp-harness" ]; then bad "nothing installed with a missing preset"; else ok "nothing installed with a missing preset"; fi
expect_fail "a file:// preset is refused" sh "$install_sh" --archive "$archive" --prefix "$T/ps2" --no-path-check --preset "file://$T/work/preset.toml"

# On a terminal (a pty from scripts/pty-run.py, so /dev/tty works as under `curl | sh`).
pty="$here/scripts/pty-run.py"
if command -v python3 >/dev/null 2>&1; then
    : >"$STUB_LOG"
    expect_ok "terminal: Enter runs setup (curl | sh, relative --preset)" python3 "$pty" --expect "[Y/n]" --send '\n' -- \
        sh -c "cd '$T/work' && cat '$install_sh' | sh -s -- --archive '$archive' --prefix '$T/tty1' --no-path-check --preset preset.toml"
    out_has "asks before running setup" "Run \`$T/tty1/amp-harness setup --preset $T/work/preset.toml\` now? [Y/n]"
    out_has "setup ran" "stub setup ran"
    log_has "setup gets --preset with the absolute file" "setup: [setup] [--preset] [$T/work/preset.toml]"
    log_has "setup's stdin is the terminal" "stdin=tty"

    : >"$STUB_LOG"
    expect_ok "terminal: y runs setup without a preset" python3 "$pty" --expect "[Y/n]" --send 'y\n' -- \
        sh "$install_sh" --archive "$archive" --prefix "$T/tty2" --no-path-check
    log_has "setup gets no --preset" "setup: [setup] stdin=tty"

    : >"$STUB_LOG"
    expect_ok "terminal: n skips setup" python3 "$pty" --expect "[Y/n]" --send 'n\n' -- \
        env AMP_HARNESS_PRESET=https://intranet.example.com/amp-preset.toml sh "$install_sh" --archive "$archive" --prefix "$T/tty2" --no-path-check
    out_has "says how to run it later" "Skipped. Run \`$T/tty2/amp-harness setup --preset https://intranet.example.com/amp-preset.toml\`"
    no_setup_ran "terminal: n does not start setup"

    expect_ok "terminal: --no-setup does not ask" python3 "$pty" -- \
        sh "$install_sh" --archive "$archive" --prefix "$T/tty3" --no-path-check --no-setup
    if grep -qF "[Y/n]" "$T/out"; then bad "--no-setup asked anyway"; else ok "--no-setup prints no prompt"; fi
    out_has "--no-setup prints the next step" "Next: run \`$T/tty3/amp-harness setup\`"
    no_setup_ran "terminal: --no-setup does not start setup"

    : >"$STUB_LOG"
    python3 "$pty" --expect "[Y/n]" --send 'y\n' -- env STUB_SETUP_EXIT=3 \
        sh "$install_sh" --archive "$archive" --prefix "$T/tty4" --no-path-check >"$T/out" 2>&1 && rc=0 || rc=$?
    if [ "$rc" = 3 ]; then ok "terminal: a failed setup exits with its status"; else bad "terminal: failed setup exit $rc, want 3"; fi
    out_has "says setup did not finish and how to retry" "Setup did not finish (exit 3). amp-harness is installed; run \`$T/tty4/amp-harness setup\` to try again."
    if [ -x "$T/tty4/amp-harness" ]; then ok "the binary stays installed after a failed setup"; else bad "binary kept after failed setup"; fi
else
    echo "skip - terminal cases (no python3)"
fi


# Homebrew formula.
for t in aarch64-apple-darwin x86_64-apple-darwin aarch64-unknown-linux-gnu x86_64-unknown-linux-gnu; do
    printf '%064d  amp-harness-%s-%s.tar.gz\n' 1 "$version" "$t" >"$T/dist/amp-harness-$version-$t.tar.gz.sha256"
done
expect_ok "render-formula.sh with all four targets" sh -c "'$here/scripts/render-formula.sh' '$version' '$T/dist' > '$T/amp-harness.rb'"
if grep -v '^#' "$T/amp-harness.rb" | grep -q '@[A-Z0-9_]*@'; then bad "formula has no placeholders"; else ok "formula has no placeholders"; fi
if [ "$(grep -c "harness-v$version/amp-harness-$version-" "$T/amp-harness.rb")" = 4 ]; then ok "formula has four URLs"; else bad "formula has four URLs"; fi
rm "$T/dist/amp-harness-$version-x86_64-apple-darwin.tar.gz.sha256"
expect_fail "render-formula.sh refuses a partial release" "$here/scripts/render-formula.sh" "$version" "$T/dist"

# Optional: the real binary.
if [ -n "${AMP_HARNESS_TEST_ARCHIVE:-}" ]; then
    expect_ok "install the real archive" sh "$install_sh" --archive "$AMP_HARNESS_TEST_ARCHIVE" --prefix "$T/real" --no-path-check
    out_has "real binary reports its version" "Installed amp-harness $version"
    if "$T/real/amp-harness" --version | head -n 1 | grep -q "^amp-harness $version"; then ok "real --version"; else bad "real --version"; fi
fi

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
