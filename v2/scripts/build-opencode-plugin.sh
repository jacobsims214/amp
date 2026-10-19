#!/usr/bin/env zsh
# build-opencode-plugin.sh — regenerate AMP's opencode plugin (v2/plugins/opencode)
# from v2/.opencode and v2/opencode.json. Edit those sources, not the output.
set -euo pipefail
SCRIPT_DIR="${0:A:h}"
V2_DIR="${SCRIPT_DIR:h}"
OUT="${V2_DIR}/plugins/opencode"
python3 "${SCRIPT_DIR}/lib/build_opencode_plugin.py" "${V2_DIR}" "${OUT}"
cp "${SCRIPT_DIR}/lib/opencode_plugin/README.md" "${OUT}/README.md"
print "Built ${OUT}"
