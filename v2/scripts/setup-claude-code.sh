#!/usr/bin/env zsh
# setup-claude-code.sh
#
# Sets up AMP for Claude Code by installing the AMP plugin from this repo.
# The plugin carries the agents, skills, slash command, and MCP servers, so
# nothing is copied into ~/.claude by hand and uninstalling is one command.
#
# Safe to run repeatedly — it re-points the marketplace at this checkout and
# reinstalls/updates the plugin.
#
# Usage:
#   ./scripts/setup-claude-code.sh                (from v2/)
#   ./scripts/setup-claude-code.sh --from-github  install from coulterac/amp
#   AMP_MCP_URL=http://localhost:8000/mcp ./scripts/setup-claude-code.sh
#
# To undo: claude plugin uninstall amp && claude plugin marketplace remove amp

set -euo pipefail

SCRIPT_DIR="${0:A:h}"
V2_DIR="${SCRIPT_DIR:h}"
REPO_DIR="${V2_DIR:h}"
PLUGIN_DIR="${V2_DIR}/plugins/amp"
MARKETPLACE_NAME="amp"
GITHUB_REPO="coulterac/amp"

SOURCE="${REPO_DIR}"
FROM_GITHUB=0
[[ "${1:-}" == "--from-github" ]] && { SOURCE="${GITHUB_REPO}"; FROM_GITHUB=1 }

info() { print -P "%F{blue}→%f $*" }
ok()   { print -P "%F{green}✓%f $*" }
warn() { print -P "%F{yellow}!%f $*" }
die()  { print -P "%F{red}✗%f $*" >&2; exit 1 }

print ""
print "AMP v2 → Claude Code setup"
print "Source : ${SOURCE}"
print ""

command -v claude &>/dev/null || die "claude CLI not found — install Claude Code first: https://claude.com/claude-code"

if (( ! FROM_GITHUB )); then
  [[ -f "${REPO_DIR}/.claude-plugin/marketplace.json" ]] || die "marketplace manifest missing — run ./scripts/build-claude-plugin.sh"
  if [[ ! -f "${PLUGIN_DIR}/.claude-plugin/plugin.json" ]]; then
    warn "plugin not built yet — building now"
    "${SCRIPT_DIR}/build-claude-plugin.sh"
  fi
fi

# 1. Marketplace — `add` fails if it already exists, so update in that case.
info "Registering marketplace..."
if claude plugin marketplace list 2>/dev/null | grep -q "${MARKETPLACE_NAME}"; then
  claude plugin marketplace update "${MARKETPLACE_NAME}" >/dev/null && ok "  updated ${MARKETPLACE_NAME}"
else
  claude plugin marketplace add "${SOURCE}" >/dev/null && ok "  added ${MARKETPLACE_NAME} from ${SOURCE}"
fi

# 2. Plugin
info "Installing plugin..."
if claude plugin list 2>/dev/null | grep -q "^amp\b\|amp@${MARKETPLACE_NAME}"; then
  claude plugin update "amp@${MARKETPLACE_NAME}" && ok "  updated amp"
else
  claude plugin install "amp@${MARKETPLACE_NAME}" && ok "  installed amp"
fi

# 3. AMP server reachability — a dead MCP server is the usual first failure.
AMP_URL="${AMP_MCP_URL:-https://amp.simstech.cloud/mcp}"
info "Checking AMP MCP server at ${AMP_URL}..."
if command -v curl &>/dev/null; then
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${AMP_URL}" 2>/dev/null || print 000)"
  if [[ "${code}" == "000" ]]; then
    warn "  unreachable — start the server, or set AMP_MCP_URL to point elsewhere"
  else
    ok "  reachable (HTTP ${code})"
  fi
else
  warn "  curl not found — skipping check"
fi

# 4. Project marker
if [[ -f "${PWD}/.amp.json" ]]; then
  ok "Project marker: ${PWD}/.amp.json"
else
  warn "No .amp.json in ${PWD} — run /amp:manager and ask it to initialise the project"
fi

print ""
ok "Done."
print ""
print "  Restart Claude Code, then:"
print ""
print "    /amp:manager plan <what you want built>"
print ""
if [[ -n "${AMP_MCP_URL:-}" ]]; then
  print "  AMP_MCP_URL is set to ${AMP_MCP_URL} — export it in your shell profile"
  print "  so Claude Code picks it up in future sessions."
  print ""
fi
print "  Inspect what got installed : claude plugin details amp"
print "  Remove it                  : claude plugin uninstall amp"
print ""
