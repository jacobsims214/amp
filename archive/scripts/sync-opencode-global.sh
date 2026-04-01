#!/usr/bin/env bash
# sync-opencode-global.sh
#
# Merges this project's opencode.json and .opencode/ skills into the global
# OpenCode config at ~/.config/opencode/
#
# Safe to run multiple times — always writes the current state.
# Skills are copied by directory name so adding a new skill just means
# adding a new directory under .opencode/skills/ and re-running this.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
GLOBAL_DIR="$HOME/.config/opencode"
PROJECT_CONFIG="$PROJECT_ROOT/opencode.json"
PROJECT_SKILLS="$PROJECT_ROOT/.opencode/skills"

# ── Colours ──────────────────────────────────────────────────────────────────
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'
ok()   { echo -e "${GREEN}✓${NC} $*"; }
info() { echo -e "${BLUE}→${NC} $*"; }
warn() { echo -e "${YELLOW}!${NC} $*"; }

echo ""
echo "AMP → OpenCode global sync"
echo "Project: $PROJECT_ROOT"
echo "Global:  $GLOBAL_DIR"
echo ""

# ── Require jq ───────────────────────────────────────────────────────────────
if ! command -v jq &>/dev/null; then
  echo "Error: jq is required. Install with: brew install jq"
  exit 1
fi

# ── 1. Merge opencode.json into global config ─────────────────────────────────
GLOBAL_CONFIG="$GLOBAL_DIR/opencode.json"

info "Merging opencode.json into global config..."

# Extract only the keys we want to propagate globally:
#   mcp       — MCP server definitions
#   agent     — agent definitions
# We intentionally do NOT propagate model/provider/permission — those are
# machine-specific and should stay in the project file.

PROJECT_MCP=$(jq -c '.mcp // {}' "$PROJECT_CONFIG")
# Rewrite relative {file:./...} prompt paths to absolute so they resolve
# correctly when loaded from ~/.config/opencode/ instead of the project root
PROJECT_AGENT=$(jq -c \
  --arg root "$PROJECT_ROOT" \
  '
    .agent // {} |
    with_entries(
      if (.value.prompt? // "") | startswith("{file:./")
      then .value.prompt = "{file:" + $root + "/" + (.value.prompt | ltrimstr("{file:./") | rtrimstr("}")) + "}"
      else .
      end
    )
  ' "$PROJECT_CONFIG")

if [[ -f "$GLOBAL_CONFIG" ]]; then
  EXISTING=$(cat "$GLOBAL_CONFIG")
else
  EXISTING='{}'
fi

# Deep merge: project values win for conflicting keys within mcp/agent
MERGED=$(echo "$EXISTING" | jq \
  --argjson mcp "$PROJECT_MCP" \
  --argjson agent "$PROJECT_AGENT" \
  '
    .["$schema"] = "https://opencode.ai/config.json" |
    .mcp   = ((.mcp   // {}) * $mcp) |
    .agent = ((.agent // {}) * $agent)
  ')

echo "$MERGED" | jq . > "$GLOBAL_CONFIG"
ok "Global config written: $GLOBAL_CONFIG"
echo ""

# Show what was merged
MCP_KEYS=$(echo "$PROJECT_MCP" | jq -r 'keys[]' 2>/dev/null | tr '\n' ' ')
AGENT_KEYS=$(echo "$PROJECT_AGENT" | jq -r 'keys[]' 2>/dev/null | tr '\n' ' ')
[[ -n "$MCP_KEYS" ]]   && echo "  MCP servers: $MCP_KEYS"
[[ -n "$AGENT_KEYS" ]] && echo "  Agents: $AGENT_KEYS"
echo ""

# ── 2. Sync skills ────────────────────────────────────────────────────────────
GLOBAL_SKILLS="$GLOBAL_DIR/skills"

if [[ ! -d "$PROJECT_SKILLS" ]]; then
  warn "No .opencode/skills/ directory found — skipping skills sync"
else
  info "Syncing skills to $GLOBAL_SKILLS ..."
  mkdir -p "$GLOBAL_SKILLS"

  synced=0
  for skill_dir in "$PROJECT_SKILLS"/*/; do
    [[ -d "$skill_dir" ]] || continue
    skill_name=$(basename "$skill_dir")
    target="$GLOBAL_SKILLS/$skill_name"

    mkdir -p "$target"
    cp -r "$skill_dir"* "$target/" 2>/dev/null || true
    ok "  $skill_name"
    ((synced++)) || true
  done

  echo ""
  ok "$synced skill(s) synced"
  echo ""
fi

# ── 3. Summary ────────────────────────────────────────────────────────────────
echo "Global config state:"
echo ""
jq '{mcp: (.mcp // {} | keys), agent: (.agent // {} | keys)}' "$GLOBAL_CONFIG"
echo ""

if [[ -d "$GLOBAL_SKILLS" ]]; then
  echo "Global skills:"
  for d in "$GLOBAL_SKILLS"/*/; do
    [[ -d "$d" ]] && echo "  $(basename "$d")"
  done
  echo ""
fi

ok "Sync complete. Restart OpenCode to pick up changes."
