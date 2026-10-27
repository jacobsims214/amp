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
# A folder is linked to AMP by the nearest .amp.json in it or a parent (#2341); none above $T counts.
export AMP_HARNESS_CEILING_DIR="$T"
unset AMP_CREDENTIAL_STORE
cd "$T"
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

# Another amp-harness on this shell's PATH (#2336): a shell that hashed it keeps running it, so
# install.sh says \`hash -r\`. A plain PATH (no person's own ~/.local/bin) for the case with none.
clean_path=/usr/bin:/bin:/usr/sbin:/sbin
stub "$T/plain" 0.0.1
expect_ok "install with no other amp-harness on PATH" env PATH="$clean_path" sh "$install_sh" --archive "$archive" --prefix "$T/h1" --no-path-check
if grep -qF "hash -r" "$T/out"; then bad "no hash -r note without another amp-harness"; else ok "no hash -r note without another amp-harness"; fi
expect_ok "install while another amp-harness comes first on PATH" env PATH="$T/plain:$clean_path" sh "$install_sh" --archive "$archive" --prefix "$T/h1" --no-path-check
out_has "says which one this shell runs" "Note: \`amp-harness\` in this shell runs $T/plain/amp-harness, not this one."
out_has "says how to remove it" "(install.sh --uninstall --prefix $T/plain, or rm $T/plain/amp-harness) or put $T/h1 first on PATH"
out_has "suggests hash -r / rehash" "then run \`hash -r\` (zsh: \`rehash\`)."
expect_ok "install ahead of another amp-harness on PATH" env PATH="$T/h1:$T/plain:$clean_path" sh "$install_sh" --archive "$archive" --prefix "$T/h1" --no-path-check
out_has "names the other one" "Note: another amp-harness is on your PATH ($T/plain/amp-harness)."
out_has "suggests hash -r for a shell that hashed it" "running it: run \`hash -r\` (zsh: \`rehash\`) or open a new terminal."
# #2341: with the login PATH check on too, the shadowing binary is named once, not twice.
mkdir -p "$T/home-shadow"
# shellcheck disable=SC2016 # $PATH is for the login shell
printf 'export PATH="%s:%s:$PATH"\n' "$T/plain" "$T/h1" >"$T/home-shadow/.profile"
expect_ok "install shadowed on this shell's and the login PATH" env HOME="$T/home-shadow" PATH="$T/plain:$T/h1:$clean_path" sh "$install_sh" --archive "$archive" --prefix "$T/h1"
out_has "the note names the shadowing binary" "Note: \`amp-harness\` in this shell runs $T/plain/amp-harness, not this one."
if [ "$(grep -c "$T/plain/amp-harness" "$T/out")" = 2 ] && ! grep -qF "comes before" "$T/out"; then ok "the shadowing binary is not reported twice"; else bad "the shadowing binary is not reported twice"; fi
out_has "the next step names the full path while shadowed" "$T/h1/amp-harness setup"
expect_ok "login PATH shadows only" env HOME="$T/home-shadow" PATH="$T/h1:$clean_path" sh "$install_sh" --archive "$archive" --prefix "$T/h1"
out_has "warns when only the login PATH finds another first" "$T/plain/amp-harness comes before $T/h1 on your PATH"

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
# first, with a non-harness AMP tag and a harness prerelease on top) to find the newest harness-v*
# release that isn't a prerelease (#2122).
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
    "tag_name": "harness-v9.0.0-rc.1",
    "name": "amp-harness 9.0.0-rc.1 (prerelease: never picked as the newest)"
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
# #2341: an older release over a newer one is a downgrade, not an update.
expect_ok "install an older release over a newer one" sh "$install_sh" --version "$old" --target "$tgt" --prefix "$T/down" --no-path-check
expect_ok "install the newer one first" sh "$install_sh" --version "$version" --target "$tgt" --prefix "$T/down" --no-path-check
expect_ok "then the older one over it" sh "$install_sh" --version "$old" --target "$tgt" --prefix "$T/down" --no-path-check
out_has "says Downgraded new → old" "Downgraded amp-harness $version → $old at $T/down/amp-harness"
if grep -qF "Updated" "$T/out"; then bad "a downgrade does not say Updated"; else ok "a downgrade does not say Updated"; fi
unset AMP_HARNESS_BASE_URL AMP_HARNESS_RELEASES_URL

# --uninstall removes only the binary.
mkdir -p "$XDG_CONFIG_HOME/amp-harness"
echo '# kept' >"$XDG_CONFIG_HOME/amp-harness/config.toml"
expect_ok "--uninstall" sh "$install_sh" --uninstall --prefix "$T/up"
if [ -e "$T/up/amp-harness" ]; then bad "binary removed"; else ok "binary removed"; fi
out_has "names what it removed" "Removed amp-harness $version from $T/up/amp-harness."
out_has "says the settings are kept, and where" "your settings: $XDG_CONFIG_HOME/amp-harness"
out_has "says the saved keys are kept, and where" "items with service \"amp-harness\""
# #2341: with the keychain turned off, the keys are in the file store, not the keychain.
expect_ok "--uninstall with AMP_CREDENTIAL_STORE=file" env AMP_CREDENTIAL_STORE=file sh "$install_sh" --uninstall --prefix "$T/up"
out_has "names the credentials file" "saved keys and AMP logins: $XDG_CONFIG_HOME/amp-harness/credentials.json (AMP_CREDENTIAL_STORE=file)"
if grep -qiE "keychain|keyring" "$T/out"; then bad "no keychain with the file store"; else ok "no keychain with the file store"; fi
if [ -f "$XDG_CONFIG_HOME/amp-harness/config.toml" ]; then ok "config left in place"; else bad "config left in place"; fi
expect_ok "--uninstall again is a no-op" sh -c "cat '$install_sh' | sh -s -- --uninstall --prefix '$T/up'"
out_has "says there is nothing to remove" "No amp-harness in $T/up; nothing to remove"

# The other amp-harness left on PATH (#2336): Homebrew's only when it is in a Cellar keg.
expect_ok "--uninstall with a plain amp-harness elsewhere on PATH" env PATH="$T/plain:$PATH" sh "$install_sh" --uninstall --prefix "$T/up"
out_has "names the plain file and how to remove it" "Another amp-harness is still on your PATH: $T/plain/amp-harness (to remove it: install.sh --uninstall --prefix $T/plain, or rm $T/plain/amp-harness)."
if grep -qF "brew" "$T/out"; then bad "a plain file is not called Homebrew's"; else ok "a plain file is not called Homebrew's"; fi
stub "$T/brew/Cellar/amp-harness/0.0.1/bin" 0.0.1
mkdir -p "$T/brew/bin"
ln -s ../Cellar/amp-harness/0.0.1/bin/amp-harness "$T/brew/bin/amp-harness"
expect_ok "--uninstall with Homebrew's amp-harness on PATH" env PATH="$T/brew/bin:$PATH" sh "$install_sh" --uninstall --prefix "$T/up"
out_has "suggests brew uninstall for a Cellar keg" "Another amp-harness is still on your PATH: $T/brew/bin/amp-harness (to remove it: brew uninstall amp-harness)."

# Reinstall/upgrade with no terminal where a config exists (#2336): kept, and doctor, not setup.
expect_ok "install with no terminal on a machine already set up" sh "$install_sh" --archive "$archive" --prefix "$T/k1" --no-path-check
out_has "says the settings, keys and logins were kept" "Your settings ($XDG_CONFIG_HOME/amp-harness), saved keys and AMP logins were kept; \`$T/k1/amp-harness doctor\` checks"
if grep -qF "Next: run" "$T/out"; then bad "no setup step when already set up"; else ok "no setup step when already set up"; fi
expect_ok "AMP_HARNESS_CONFIG_DIR is where the config is looked for" env AMP_HARNESS_CONFIG_DIR="$T/elsewhere" sh "$install_sh" --archive "$archive" --prefix "$T/k1" --no-path-check
out_has "an empty config dir still gets the setup step" "Next: run \`$T/k1/amp-harness setup\`"
# #2341: set up and in a linked folder (an .amp.json here or in a parent): kept, and doctor.
mkdir -p "$T/linked/sub"
echo '{"project_id": 1}' >"$T/linked/.amp.json"
expect_ok "no terminal, in a linked folder" sh -c "cd '$T/linked/sub' && sh '$install_sh' --archive '$archive' --prefix '$T/k1' --no-path-check"
out_has "says kept and linked" "linked to AMP ($T/linked/.amp.json). \`$T/k1/amp-harness doctor\` checks the setup."
if grep -qF "Next: run" "$T/out"; then bad "no setup step in a linked folder"; else ok "no setup step in a linked folder"; fi

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
    # #2341: an upgrade in a terminal, set up and in a linked folder, does not offer setup again.
    : >"$STUB_LOG"
    expect_ok "terminal: upgrade in a linked folder" python3 "$pty" -- \
        sh -c "cd '$T/linked' && cat '$install_sh' | sh -s -- --archive '$archive' --prefix '$T/k1' --no-path-check"
    if grep -qF "[Y/n]" "$T/out"; then bad "terminal: no setup offer in a linked folder"; else ok "terminal: no setup offer in a linked folder"; fi
    out_has "terminal: says kept and linked" "linked to AMP ($T/linked/.amp.json). \`$T/k1/amp-harness doctor\` checks the setup."
    no_setup_ran "terminal: setup is not started in a linked folder"
    : >"$STUB_LOG"
    expect_ok "terminal: a preset in a linked folder still offers setup" python3 "$pty" --expect "[Y/n]" --send 'n\n' -- \
        sh -c "cd '$T/linked' && sh '$install_sh' --archive '$archive' --prefix '$T/k1' --no-path-check --preset '$T/work/preset.toml'"
    out_has "terminal: offers setup with the preset" "Run \`$T/k1/amp-harness setup --preset $T/work/preset.toml\` now? [Y/n]"
fi
rm -rf "$XDG_CONFIG_HOME/amp-harness"

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
    # The setup hand-off with the real binary (#2330): under `curl | sh` stdin is the pipe, so
    # install.sh hands setup /dev/tty, which crossterm on macOS could not poll ("read key: Failed
    # to initialize input reader"). The server prompt must take keys, and Ctrl+C must cancel.
    if command -v python3 >/dev/null 2>&1; then
        mkdir -p "$T/real-project"
        python3 "$pty" --timeout 30 --expect "[Y/n]" --send 'y\n' --expect "AMP server [" --send 'abc' --expect 'abc' --send '\x03' -- \
            env -u AMP_MCP_URL -u AMP_TOKEN -u AMP_HARNESS_PROMPTS -u AMP_HARNESS_PROFILE AMP_CREDENTIAL_STORE=file \
            sh -c "cd '$T/real-project' && cat '$install_sh' | sh -s -- --archive '$AMP_HARNESS_TEST_ARCHIVE' --prefix '$T/real' --no-path-check" \
            >"$T/out" 2>&1 && rc=0 || rc=$?
        if [ "$rc" = 130 ]; then ok "real setup from curl | sh: Ctrl+C at the server prompt cancels (exit 130)"; else bad "real setup from curl | sh: exit $rc, want 130"; fi
        out_has "real setup reads typed keys from /dev/tty" "AMP server [https://amp.simstech.cloud]: abc^C"
        out_has "real setup says it was cancelled" "amp-harness: setup cancelled"
        if grep -qF "read key" "$T/out"; then bad "real setup could not read keys"; else ok "no key-reader error"; fi
        if [ -e "$T/real-project/.amp.json" ]; then bad "cancelled setup linked the folder"; else ok "cancelled setup wrote no .amp.json"; fi
    fi
fi

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
