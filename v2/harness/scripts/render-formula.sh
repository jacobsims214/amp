#!/bin/sh
# Renders packaging/homebrew/amp-harness.rb.tmpl into a Homebrew formula on stdout (#2115).
#
#   scripts/render-formula.sh VERSION DIST_DIR > amp-harness.rb
#
# DIST_DIR must hold amp-harness-VERSION-<target>.tar.gz.sha256 for all four release targets
# (what scripts/package.sh writes). Fails if any is missing, so a partial release never renders.
set -eu

[ $# -eq 2 ] || { echo "usage: render-formula.sh VERSION DIST_DIR" >&2; exit 2; }
version=$1
dist=$2
here=$(cd "$(dirname "$0")/.." && pwd)

script="s/@VERSION@/$version/g"
for target in aarch64-apple-darwin x86_64-apple-darwin aarch64-unknown-linux-gnu x86_64-unknown-linux-gnu; do
    f="$dist/amp-harness-$version-$target.tar.gz.sha256"
    [ -f "$f" ] || { echo "render-formula.sh: missing $f" >&2; exit 1; }
    hex=$(cut -d' ' -f1 <"$f" | head -n 1)
    case "$hex" in *[!0-9a-f]* | "") echo "render-formula.sh: bad digest in $f" >&2; exit 1 ;; esac
    [ ${#hex} -eq 64 ] || { echo "render-formula.sh: bad digest in $f" >&2; exit 1; }
    key=$(echo "$target" | tr 'a-z-' 'A-Z_')
    script="$script;s/@SHA256_${key}@/$hex/g"
done

out=$(sed -e "$script" "$here/packaging/homebrew/amp-harness.rb.tmpl")
if printf '%s\n' "$out" | grep -q '@[A-Z0-9_]*@'; then
    echo "render-formula.sh: unfilled placeholder left in the formula" >&2
    exit 1
fi
printf '%s\n' "$out"
