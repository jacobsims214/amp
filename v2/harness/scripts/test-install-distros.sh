#!/bin/sh
# install.sh on real Linux distributions, in containers (#2276). Needs docker; CI runs it on the
# ubuntu job, `make install-test-distros` runs it locally. Each case runs install.sh as root in a
# throwaway container with only the image's own tools, against a stub binary (a shell script, so it
# runs on any libc) and a file:// release mirror under target/install-distros:
#
#   debian:bookworm-slim  glibc 2.36, no curl and no wget: a download names the missing tools;
#                         --archive installs, upgrades nothing, uninstalls
#   ubuntu:24.04 + wget   wget only (no curl): the file:// mirror download installs
#   ubuntu:20.04          glibc 2.31: refused before downloading, with the glibc it needs
#   alpine:3.20           musl: refused before downloading, with a clear message
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
T="$here/target/install-distros"
rm -rf "$T"
mkdir -p "$T/stub" "$T/mirror"
command -v docker >/dev/null 2>&1 || { echo "test-install-distros: docker is needed" >&2; exit 2; }

version=$(sed -n '/^\[workspace.package\]/,/^\[/s/^version = "\(.*\)"/\1/p' "$here/Cargo.toml" | head -n 1)
case "$(docker version --format '{{.Server.Arch}}' 2>/dev/null)" in
    arm64 | aarch64) tgt=aarch64-unknown-linux-gnu ;;
    *) tgt=x86_64-unknown-linux-gnu ;;
esac
pass=0
fail=0
ok() { pass=$((pass + 1)); echo "ok   - $1"; }
bad() { fail=$((fail + 1)); echo "FAIL - $1"; [ -f "$T/out" ] && sed 's/^/       /' "$T/out"; }
out_has() { if grep -qF -- "$2" "$T/out"; then ok "$1"; else bad "$1: no '$2' in output"; fi; }

printf '#!/bin/sh\necho "amp-harness %s (stub)"\n' "$version" >"$T/stub/amp-harness"
chmod +x "$T/stub/amp-harness"
archive=$("$here/scripts/package.sh" --bin "$T/stub/amp-harness" --target "$tgt" --out "$T/mirror/harness-v$version" 2>/dev/null | tail -n 1)
cp "$here/install.sh" "$T/install.sh"
name=$(basename "$archive")

for img in debian:bookworm-slim ubuntu:24.04 ubuntu:20.04 alpine:3.20; do
    docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img" >/dev/null ||
        docker pull -q "$img" >/dev/null || docker pull -q "$img" >/dev/null ||
        { echo "test-install-distros: could not pull $img" >&2; exit 2; }
done

# run IMAGE SCRIPT: runs SCRIPT with sh in IMAGE, $T mounted at /t; output in $T/out, status returned.
# Docker's own failures (125-127) stop the whole run, so they never pass as an expected failure.
run() {
    rc=0
    docker run --rm -v "$T:/t" -e HOME=/root -e AMP_HARNESS_BASE_URL=file:///t/mirror \
        "$1" sh -c "$2" >"$T/out" 2>&1 || rc=$?
    if [ "$rc" -ge 125 ]; then
        cat "$T/out" >&2
        echo "test-install-distros: docker run $1 failed ($rc)" >&2
        exit 2
    fi
    return "$rc"
}
expect_ok() { n=$1; shift; if run "$@"; then ok "$n"; else bad "$n"; fi; }
expect_fail() { n=$1; shift; if run "$@"; then bad "$n (should have failed)"; else ok "$n"; fi; }

img=debian:bookworm-slim
expect_fail "$img: download without curl or wget fails" "$img" "sh /t/install.sh --version $version --prefix /opt/p --no-path-check"
out_has "$img: names the missing tools" "need curl or wget to download amp-harness"
expect_ok "$img: --archive installs with tar + sha256sum only" "$img" \
    "sh /t/install.sh --archive /t/mirror/harness-v$version/$name --prefix /opt/p && /opt/p/amp-harness && sh /t/install.sh --uninstall --prefix /opt/p && test ! -e /opt/p/amp-harness"
out_has "$img: installed" "Installed amp-harness $version to /opt/p/amp-harness"
out_has "$img: PATH advice for a login shell" "/opt/p is not on your login shell's PATH"
out_has "$img: uninstalled" "Removed amp-harness $version from /opt/p/amp-harness."
out_has "$img: Linux keyring note" "Secret Service keyring"

img=ubuntu:24.04
expect_ok "$img: wget-only download" "$img" \
    "apt-get -qq update >/dev/null && apt-get -qq install -y wget >/dev/null && ! command -v curl && sh /t/install.sh --version $version --prefix /opt/p --no-path-check && /opt/p/amp-harness"
out_has "$img: downloaded and installed" "Installed amp-harness $version to /opt/p/amp-harness"
out_has "$img: stub runs" "amp-harness $version (stub)"

img=ubuntu:20.04
expect_fail "$img: glibc 2.31 is refused" "$img" "sh /t/install.sh --version $version --prefix /opt/p --no-path-check"
out_has "$img: says which glibc it needs" "amp-harness needs glibc 2.35 or newer, and this system has glibc 2.31"
if grep -qF "Downloading" "$T/out"; then bad "$img: nothing downloaded"; else ok "$img: nothing downloaded"; fi

img=alpine:3.20
expect_fail "$img: musl is refused" "$img" "sh /t/install.sh --prefix /opt/p --no-path-check"
out_has "$img: says musl is not supported yet" "this Linux uses musl libc (Alpine or similar)"
expect_fail "$img: --archive is refused too" "$img" "sh /t/install.sh --archive /t/mirror/harness-v$version/$name --prefix /opt/p --no-path-check"
out_has "$img: same message" "ships glibc builds only for now"

echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
