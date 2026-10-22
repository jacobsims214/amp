#!/bin/sh
# Offline tests for install.sh, package.sh and render-formula.sh (#2115). `make install-test` runs it;
# so does CI. Everything happens under target/install-test with HOME pointed there, so the real
# ~/.local/bin and shell profiles are never touched. A stub binary stands in for amp-harness unless
# AMP_HARNESS_TEST_ARCHIVE names a real archive (with its .sha256 beside it), which is then installed
# and run too.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
T="$here/target/install-test"
rm -rf "$T"
mkdir -p "$T/home" "$T/tmp" "$T/stub"
export HOME="$T/home" TMPDIR="$T/tmp" SHELL=/bin/sh
unset AMP_HARNESS_VERSION AMP_HARNESS_PREFIX AMP_HARNESS_ARCHIVE AMP_HARNESS_SHA256 AMP_HARNESS_TARGET AMP_HARNESS_BASE_URL

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
out_has() { if grep -q "$2" "$T/out"; then ok "$1"; else bad "$1: no '$2' in output"; fi; }

cat >"$T/stub/amp-harness" <<STUB
#!/bin/sh
echo "amp-harness $version (stub)"
STUB
chmod +x "$T/stub/amp-harness"

# package.sh: archive + sha256sum-format checksum.
archive=$("$here/scripts/package.sh" --bin "$T/stub/amp-harness" --target "$tgt" --out "$T/dist" 2>/dev/null | tail -n 1)
name="amp-harness-$version-$tgt.tar.gz"
if [ "$archive" = "$T/dist/$name" ] && [ -f "$archive" ] && [ -f "$archive.sha256" ]; then ok "package.sh writes $name + .sha256"; else bad "package.sh output: $archive"; fi
if tar -tzf "$archive" | grep -qx "amp-harness-$version-$tgt/amp-harness" && tar -tzf "$archive" | grep -qx "amp-harness-$version-$tgt/LICENSE"; then ok "archive layout"; else bad "archive layout"; fi
if grep -q "  $name\$" "$archive.sha256"; then ok "checksum file names the archive"; else bad "checksum file format"; fi

# Local archive install.
expect_ok "install --archive" sh "$install_sh" --archive "$archive" --prefix "$T/p1" --no-path-check
out_has "reports checksum OK" "Checksum OK"
out_has "prints the next step" "Next: run \`amp-harness\`"
if [ "$("$T/p1/amp-harness")" = "amp-harness $version (stub)" ]; then ok "installed binary runs"; else bad "installed binary"; fi
if [ -z "$(find "$T/p1" -name amp-harness -perm -0755)" ]; then bad "mode 0755"; else ok "mode 0755"; fi
expect_ok "reinstall over an existing binary" sh "$install_sh" --archive "$archive" --prefix "$T/p1" --no-path-check

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
