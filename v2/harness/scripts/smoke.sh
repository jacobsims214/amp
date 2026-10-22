#!/usr/bin/env bash
# Harness smoke test (D13). M1-1 scaffold: checks the built binary reports its version and pins.
# M1-5 extends this into the compose/cluster leg against local AMP (AMP_MCP_URL, AMP_TOKEN, AMP_CA_FILE).
set -euo pipefail

cd "$(dirname "$0")/.."
BIN="${AMP_HARNESS_BIN:-target/debug/amp-harness}"

if [[ ! -x "$BIN" ]]; then
  echo "smoke: $BIN not found; run make -C v2/harness build first" >&2
  exit 1
fi

out="$("$BIN" --version)"
echo "$out"
for name in rpi-ai rpi-agent rpi-tools rpi-harness rpi-tui rmcp; do
  grep -Eq "^${name} [0-9]+\.[0-9]+\.[0-9]+$" <<<"$out" || { echo "smoke: FAIL: no ${name} version" >&2; exit 1; }
done
echo "smoke: OK"
