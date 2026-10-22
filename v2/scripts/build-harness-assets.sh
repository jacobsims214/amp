#!/usr/bin/env bash
# build-harness-assets.sh — regenerate the AMP harness's embedded agents and skills
# (v2/harness/assets) from v2/.opencode, rewriting host glue with scripts/lib/harness_overrides.yaml.
# The output is committed; edit the sources or the overrides, not the output.
#
#   scripts/build-harness-assets.sh           regenerate v2/harness/assets
#   scripts/build-harness-assets.sh --check   regenerate into a temp dir under v2/harness/target and
#                                             fail if it differs from the committed assets (CI drift check)
#
# Needs python3 with PyYAML. Design: project 2 KB decisions/amp-harness-architecture.md (D5).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
V2_DIR="$(dirname "$SCRIPT_DIR")"
OUT="${V2_DIR}/harness/assets"
GEN="${SCRIPT_DIR}/lib/build_harness_assets.py"

if [[ "${1:-}" == "--check" ]]; then
  TMP="${V2_DIR}/harness/target/assets-check"
  rm -rf "$TMP"
  mkdir -p "$(dirname "$TMP")"
  python3 "$GEN" "$V2_DIR" "$TMP"
  if ! diff -r "$OUT" "$TMP" >/dev/null 2>&1; then
    diff -ru "$OUT" "$TMP" | head -60 || true
    rm -rf "$TMP"
    echo "build-harness-assets: v2/harness/assets is out of date with v2/.opencode;" >&2
    echo "  run v2/scripts/build-harness-assets.sh and commit the result." >&2
    exit 1
  fi
  rm -rf "$TMP"
  echo "build-harness-assets: v2/harness/assets is up to date"
  exit 0
fi

python3 "$GEN" "$V2_DIR" "$OUT"
echo "Built ${OUT}"
