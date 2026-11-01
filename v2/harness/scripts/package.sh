#!/bin/sh
# Packs a built amp-harness into a release archive plus its checksum (#2115, D12):
#
#   target/dist/amp-harness-<version>-<target>.tar.gz
#   target/dist/amp-harness-<version>-<target>.tar.gz.sha256   ("<hex>  <file name>", sha256sum format)
#
# The archive holds one directory, amp-harness-<version>-<target>/, with the binary, README.md and
# LICENSE. install.sh consumes exactly this layout. Run `make dist` rather than this script directly;
# it builds first. Options:
#   --target TRIPLE  the target triple in the name (default: rustc's host); the binary is read from
#                    target/TRIPLE/release when that exists, else target/release
#   --bin PATH       the binary to pack (overrides the lookup above)
#   --out DIR        output directory (default: target/dist)
# Prints the archive path on the last line of stdout.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd) # v2/harness
target=""
bin=""
out="$here/target/dist"

while [ $# -gt 0 ]; do
    case "$1" in
        --target) target=$2; shift 2 ;;
        --bin) bin=$2; shift 2 ;;
        --out) out=$2; shift 2 ;;
        *) echo "package.sh: unknown option: $1" >&2; exit 2 ;;
    esac
done

# The version is the workspace's ([workspace.package] is the only `version = ` at column 0 before
# any [dependencies] table). Release tags are harness-v<this>.
version=$(sed -n '/^\[workspace.package\]/,/^\[/s/^version = "\(.*\)"/\1/p' "$here/Cargo.toml" | head -n 1)
[ -n "$version" ] || { echo "package.sh: no [workspace.package] version in Cargo.toml" >&2; exit 1; }

if [ -z "$target" ]; then
    target=$(rustc -vV | sed -n 's/^host: //p')
fi
if [ -z "$bin" ]; then
    if [ -x "$here/target/$target/release/amp-harness" ]; then
        bin="$here/target/$target/release/amp-harness"
    else
        bin="$here/target/release/amp-harness"
    fi
fi
[ -x "$bin" ] || { echo "package.sh: no binary at $bin (run make release)" >&2; exit 1; }

name="amp-harness-$version-$target"
mkdir -p "$out"
stage="$out/.stage-$name"
rm -rf "$stage"
mkdir -p "$stage/$name"
cp "$bin" "$stage/$name/amp-harness"
chmod 0755 "$stage/$name/amp-harness"
cp "$here/README.md" "$stage/$name/README.md"
cp "$here/../../LICENSE" "$stage/$name/LICENSE"

# COPYFILE_DISABLE keeps macOS tar from adding ._ AppleDouble files.
(cd "$stage" && COPYFILE_DISABLE=1 tar -czf "$out/$name.tar.gz" "$name")
rm -rf "$stage"

if command -v sha256sum >/dev/null 2>&1; then
    hex=$(sha256sum "$out/$name.tar.gz" | cut -d' ' -f1)
else
    hex=$(shasum -a 256 "$out/$name.tar.gz" | cut -d' ' -f1)
fi
printf '%s  %s\n' "$hex" "$name.tar.gz" >"$out/$name.tar.gz.sha256"

echo "packed $name.tar.gz ($(wc -c <"$out/$name.tar.gz" | tr -d ' ') bytes, sha256 $hex)" >&2
echo "$out/$name.tar.gz"
