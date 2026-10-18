#!/usr/bin/env zsh
# sync-global-config.sh
#
# Merges the AMP v2 opencode config into your global ~/.config/opencode config.
# Safe to run repeatedly — it upserts, never clobbers unrelated config.
#
# For Claude Code, use ./scripts/setup-claude-code.sh instead — it installs the
# same roster as a plugin. See scripts/README.md.
#
# What it does:
#   1. Copies skills  v2/.opencode/skills/*  → ~/.config/opencode/skills/
#   2. Copies agents  v2/.opencode/agent/*.md → ~/.config/opencode/agent/
#   3. Merges opencode.json — upserts provider, providers (per model) and mcp (agents are
#      picked up by the file copy above, not merged into JSON)
#
# Usage:
#   ./scripts/sync-global-config.sh          (from v2/)
#   /abs/path/to/v2/scripts/sync-global-config.sh

set -euo pipefail

SCRIPT_DIR="${0:A:h}"
V2_DIR="${SCRIPT_DIR:h}"
V2_OPENCODE="${V2_DIR}/.opencode"
V2_JSON="${V2_DIR}/opencode.json"
GLOBAL_DIR="${HOME}/.config/opencode"
GLOBAL_JSON="${GLOBAL_DIR}/opencode.json"
AGENT_DEST="${GLOBAL_DIR}/agent"
SKILLS_DEST="${GLOBAL_DIR}/skills"

info()    { print -P "%F{blue}→%f $*" }
ok()      { print -P "%F{green}✓%f $*" }
die()     { print -P "%F{red}✗%f $*" >&2; exit 1 }

print ""
print "AMP v2 → global opencode sync"
print "Source : ${V2_DIR}"
print "Dest   : ${GLOBAL_DIR}"
print ""

[[ -d "${V2_OPENCODE}" ]]      || die ".opencode not found: ${V2_OPENCODE}"
[[ -f "${V2_JSON}" ]]          || die "opencode.json not found: ${V2_JSON}"
mkdir -p "${GLOBAL_DIR}"
command -v python3 &>/dev/null || die "python3 required"

# 1. Skills
info "Syncing skills..."
mkdir -p "${SKILLS_DEST}"
for d in "${V2_OPENCODE}/skills"/*/; do
  [[ -d "$d" ]] || continue
  name="${d:t}"
  cp -r "$d" "${SKILLS_DEST}/${name}"
  ok "  skill: ${name}"
done

# 2. Agents
info "Syncing agents..."
mkdir -p "${AGENT_DEST}"
for f in "${V2_OPENCODE}/agent"/*.md; do
  [[ -f "$f" ]] || continue
  cp "$f" "${AGENT_DEST}/"
  ok "  agent: ${f:t}"
done

# 2b. Plugins (e.g. amp_session_usage) and the npm packages they import.
info "Syncing plugins..."
mkdir -p "${GLOBAL_DIR}/plugin"
for f in "${V2_OPENCODE}/plugin"/*.(js|ts)(N); do
  cp "$f" "${GLOBAL_DIR}/plugin/"
  ok "  plugin: ${f:t}"
done
python3 - "${GLOBAL_DIR}/package.json" <<'PYEOF'
import json, os, sys
dst = sys.argv[1]
d = json.load(open(dst)) if os.path.exists(dst) else {}
deps = d.setdefault("dependencies", {})
# What the AMP plugins import (.opencode/package.json is generated and gitignored).
deps.setdefault("zod", "4.1.8")
json.dump(d, open(dst, "w"), indent=2)
PYEOF
(cd "${GLOBAL_DIR}" && npm install --silent >/dev/null 2>&1) && ok "  plugin dependencies installed" || info "  run npm install in ${GLOBAL_DIR} for plugin dependencies"

# 3. Merge JSON — write python to a temp file then run it
TMPPY="$(mktemp /tmp/amp-merge.XXXXXX.py)"
trap "rm -f ${TMPPY}" EXIT

cat > "${TMPPY}" << 'PYEOF'
import sys, json, re
from pathlib import Path

global_path = Path(sys.argv[1])
v2_path     = Path(sys.argv[2])

def load_jsonc(path):
    """Load JSON that may contain trailing commas (JSONC/JSON5 style)."""
    text = Path(path).read_text()
    # Remove trailing commas before } or ]
    text = re.sub(r',(\s*[}\]])', r'\1', text)
    return json.loads(text)

with open(global_path) as f:
    gcfg = json.load(f)
v2cfg = load_jsonc(v2_path)

gcfg.setdefault('provider', {})
gcfg.setdefault('mcp', {})

for name, cfg in v2cfg.get('provider', {}).items():
    gcfg['provider'][name] = cfg
    print(f"  provider: {name}")

# opencode 2 'providers': upsert per model, so models you added yourself to the
# same provider (e.g. other OpenRouter models) are kept.
for name, cfg in v2cfg.get('providers', {}).items():
    dst = gcfg.setdefault('providers', {}).setdefault(name, {})
    for key, val in cfg.items():
        if key == 'models':
            dst.setdefault('models', {}).update(val)
        elif isinstance(val, dict) and isinstance(dst.get(key), dict):
            dst[key].update(val)
        else:
            dst[key] = val
    print(f"  providers: {name}  models={', '.join(cfg.get('models', {}))}")

for name, cfg in v2cfg.get('mcp', {}).items():
    gcfg['mcp'][name] = cfg
    print(f"  mcp:   {name}  url={cfg.get('url','')}")

# Remove stale inline agent definitions that conflict with the file-based
# agents in ~/.config/opencode/agent/*.md. Agents defined in files take
# precedence over JSON inline definitions per opencode's own docs (agents
# defined in both JSON and files get the file's definition, so stale JSON
# entries can silently shadow the newer file-based ones — see the
# "AMP v2 → global opencode sync" header comment above).
#
# The old setup had amp-manager and amp-worker defined inline in JSON with
# a completely different model (z-ai/glm-5.2) and permission set
# (webfetch: allow), which conflicted with the newer file-based agents
# (amp-manager.md, amp-worker-*.md). Removing them entirely — the file
# copies in step 2 above are the source of truth for agent definitions.
removed = []
for agent_name in list(gcfg.get('agent', {}).keys()):
    del gcfg['agent'][agent_name]
    removed.append(agent_name)
if removed:
    print(f"  removed stale inline agents: {', '.join(removed)}")
if not gcfg.get('agent'):
    gcfg.pop('agent', None)

with open(global_path, 'w') as f:
    json.dump(gcfg, f, indent=2)
    f.write('\n')
PYEOF

info "Merging opencode.json (provider + mcp only)..."
if [[ -f "${GLOBAL_JSON}" ]]; then
  BACKUP="${GLOBAL_JSON}.bak.$(date +%Y%m%d_%H%M%S)"
  cp "${GLOBAL_JSON}" "${BACKUP}"
  ok "  backed up to ${BACKUP:t}"
else
  # First run on this machine: start from an empty config to merge into.
  print '{ "$schema": "https://opencode.ai/config.json" }' > "${GLOBAL_JSON}"
  ok "  created ${GLOBAL_JSON}"
fi

python3 "${TMPPY}" "${GLOBAL_JSON}" "${V2_JSON}"

ok "  wrote ${GLOBAL_JSON}"

print ""
ok "Done."
print ""
print "  Skills : ${SKILLS_DEST}"
print "  Agents : ${AGENT_DEST}"
print "  Config : ${GLOBAL_JSON}"
print ""
print "Restart opencode from any directory to pick up the changes."
print "The amp MCP server must be running on localhost:8000."
print ""
