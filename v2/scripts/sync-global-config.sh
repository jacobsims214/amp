#!/usr/bin/env zsh
# sync-global-config.sh
#
# Merges the AMP v2 opencode config into your global ~/.config/opencode config.
# Safe to run repeatedly — it upserts, never clobbers unrelated config.
#
# What it does:
#   1. Copies skills   v2/.opencode/skills/*   → ~/.config/opencode/skills/
#   2. Copies prompts  v2/.opencode/prompts/*  → ~/.config/opencode/prompts/
#   3. Merges opencode.json — upserts agent + mcp keys, rewrites prompt
#      paths to absolute so they work from any directory
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
PROMPTS_DEST="${GLOBAL_DIR}/prompts"
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
[[ -d "${GLOBAL_DIR}" ]]       || die "~/.config/opencode not found"
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

# 2. Prompts
info "Syncing prompts..."
mkdir -p "${PROMPTS_DEST}"
for f in "${V2_OPENCODE}/prompts"/*.txt; do
  [[ -f "$f" ]] || continue
  cp "$f" "${PROMPTS_DEST}/"
  ok "  prompt: ${f:t}"
done

# 3. Merge JSON — write python to a temp file then run it
TMPPY="$(mktemp /tmp/amp-merge.XXXXXX.py)"
trap "rm -f ${TMPPY}" EXIT

cat > "${TMPPY}" << 'PYEOF'
import sys, json, re
from pathlib import Path

global_path  = Path(sys.argv[1])
v2_path      = Path(sys.argv[2])
prompts_dest = Path(sys.argv[3])

with open(global_path) as f:
    gcfg = json.load(f)
with open(v2_path) as f:
    v2cfg = json.load(f)

def rewrite_prompt(val, dest):
    if not isinstance(val, str):
        return val
    m = re.match(r'^\{file:(.+)\}$', val)
    if not m:
        return val
    filename = Path(m.group(1)).name
    return '{file:' + str(dest / filename) + '}'

def rewrite_agent(agent, dest):
    a = dict(agent)
    if 'prompt' in a:
        a['prompt'] = rewrite_prompt(a['prompt'], dest)
    return a

gcfg.setdefault('agent', {})
gcfg.setdefault('mcp', {})

for name, cfg in v2cfg.get('agent', {}).items():
    gcfg['agent'][name] = rewrite_agent(cfg, prompts_dest)
    print(f"  agent: {name}  prompt={gcfg['agent'][name].get('prompt','')}")

for name, cfg in v2cfg.get('mcp', {}).items():
    gcfg['mcp'][name] = cfg
    print(f"  mcp:   {name}  url={cfg.get('url','')}")

with open(global_path, 'w') as f:
    json.dump(gcfg, f, indent=2)
    f.write('\n')
PYEOF

info "Merging opencode.json..."
BACKUP="${GLOBAL_JSON}.bak.$(date +%Y%m%d_%H%M%S)"
cp "${GLOBAL_JSON}" "${BACKUP}"
ok "  backed up to ${BACKUP:t}"

python3 "${TMPPY}" "${GLOBAL_JSON}" "${V2_JSON}" "${PROMPTS_DEST}"

ok "  wrote ${GLOBAL_JSON}"

print ""
ok "Done."
print ""
print "  Skills  : ${SKILLS_DEST}"
print "  Prompts : ${PROMPTS_DEST}"
print "  Config  : ${GLOBAL_JSON}"
print ""
print "Restart opencode from any directory to pick up the changes."
print "The amp MCP server must be running on localhost:8000."
print ""
