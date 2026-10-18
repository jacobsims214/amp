#!/usr/bin/env zsh
# setup-opencode.sh — install AMP for opencode as a plugin from this checkout.
#
#   ./scripts/setup-opencode.sh [--url https://amp.example.com/mcp] [--token <AMP token>] [--inherit-models]
#
# Installs the plugin's runtime dependency and adds one entry to your global
# opencode config (~/.config/opencode/opencode.json). Nothing else in your
# config is touched; remove the entry to uninstall. Once AMP is published to
# npm this becomes `opencode plugin add amp-opencode`.
#
# Replaces sync-global-config.sh, which copied agents and skills into your
# config. If you used it, the copies (~/.config/opencode/agent/amp-*.md and the
# AMP skills) shadow the plugin's: remove them, or run with --clean.
set -euo pipefail
SCRIPT_DIR="${0:A:h}"
V2_DIR="${SCRIPT_DIR:h}"
PLUGIN_DIR="${V2_DIR}/plugins/opencode"
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/opencode"
URL="" TOKEN="" MODELS="" CLEAN=0
while (( $# )); do
  case "$1" in
    --url) URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --inherit-models) MODELS="inherit"; shift ;;
    --clean) CLEAN=1; shift ;;
    *) print -u2 "unknown option $1"; exit 2 ;;
  esac
done

[[ -f "${PLUGIN_DIR}/server.js" ]] || "${SCRIPT_DIR}/build-opencode-plugin.sh"
(cd "${PLUGIN_DIR}" && npm install --omit=dev --omit=peer --legacy-peer-deps --no-audit --no-fund --silent)
mkdir -p "${CONFIG_DIR}"

python3 - "${CONFIG_DIR}/opencode.json" "file://${PLUGIN_DIR}" "${URL}" "${TOKEN}" "${MODELS}" <<'PYEOF'
import json, os, sys
path, pkg, url, token, models = sys.argv[1:]
cfg = json.load(open(path)) if os.path.exists(path) else {"$schema": "https://opencode.ai/config.json"}
opts = {k: v for k, v in {"url": url, "token": token, "models": models}.items() if v}
entry = {"package": pkg, "options": opts} if opts else pkg
plugins = [p for p in cfg.get("plugins", []) if (p if isinstance(p, str) else p.get("package")) != pkg]
plugins.append(entry)
cfg["plugins"] = plugins
json.dump(cfg, open(path, "w"), indent=2)
open(path, "a").write("\n")
print(f"  plugin: {pkg}" + (f"  options: {sorted(opts)}" if opts else ""))
PYEOF

if (( CLEAN )); then
  for f in "${V2_DIR}"/.opencode/agent/*.md(N); do rm -f "${CONFIG_DIR}/agent/${f:t}" && print "  removed copied agent ${f:t}"; done
  for d in "${V2_DIR}"/.opencode/skills/*(N/); do rm -rf "${CONFIG_DIR}/skills/${d:t}" && print "  removed copied skill ${d:t}"; done
  rm -f "${CONFIG_DIR}/plugin/amp-usage.js"
fi
print "AMP for opencode is installed. Start opencode; the manager is your default agent."
