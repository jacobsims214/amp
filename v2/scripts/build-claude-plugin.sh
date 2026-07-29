#!/usr/bin/env zsh
# build-claude-plugin.sh
#
# Regenerates the Claude Code plugin at v2/plugins/amp/ from the opencode
# sources under v2/.opencode/ and v2/opencode.json.
#
# The plugin directory is generated output — it is wiped and rebuilt on every
# run. Edit the opencode sources, not the plugin. The generated tree IS
# committed so users can install without running anything.
#
# Usage:
#   ./scripts/build-claude-plugin.sh          (from v2/)
#   /abs/path/to/v2/scripts/build-claude-plugin.sh

set -euo pipefail

SCRIPT_DIR="${0:A:h}"
V2_DIR="${SCRIPT_DIR:h}"
REPO_DIR="${V2_DIR:h}"
PLUGIN_DIR="${V2_DIR}/plugins/amp"
CONVERTER="${SCRIPT_DIR}/lib/opencode_to_claude.py"

info() { print -P "%F{blue}→%f $*" }
ok()   { print -P "%F{green}✓%f $*" }
warn() { print -P "%F{yellow}!%f $*" }
die()  { print -P "%F{red}✗%f $*" >&2; exit 1 }

print ""
print "AMP v2 opencode → Claude Code plugin"
print "Source : ${V2_DIR}/.opencode"
print "Output : ${PLUGIN_DIR}"
print ""

command -v python3 &>/dev/null || die "python3 required"
[[ -f "${CONVERTER}" ]] || die "converter not found: ${CONVERTER}"

info "Converting agents, skills, and MCP config..."
python3 "${CONVERTER}" "${V2_DIR}" "${PLUGIN_DIR}"

# The plugin ships its own README so it reads correctly on GitHub and in
# `claude plugin details amp`.
info "Writing plugin README..."
cat > "${PLUGIN_DIR}/README.md" << 'EOF'
# AMP — Claude Code plugin

A manager/worker/reviewer agent team that plans work onto the [AMP](https://amp.simstech.cloud)
board over MCP, dispatches specialists in review-gated waves, and accumulates project knowledge
in the AMP knowledge base.

## Install

```
/plugin marketplace add coulterac/amp
/plugin install amp@amp
```

Then restart Claude Code.

## Use

```
/amp:manager plan the auth refactor
```

`/amp:manager` puts the current session into the manager role: it plans, builds the board, and
dispatches the specialist subagents. It never edits project code itself.

On a repo with no `.amp.json`, ask it to initialise first — it will pick up the `amp-init` skill.

## What's inside

| Component | Contents |
|---|---|
| Command | `/amp:manager` — adopt the manager protocol in the main session |
| Agents | workers (backend/frontend/docs), reviewers, researchers, KB curator |
| Skills | AMP protocol (planning, execution, review, KB, MCP reference) plus engineering skills (Go, React, Docker, testing, git, TFE, UI/UX) |
| MCP | `amp` (board + knowledge base), `context7` (library docs), `chrome-devtools` |

## Pointing at a different AMP server

The `amp` MCP server URL defaults to the hosted instance. Override it per machine:

```
export AMP_MCP_URL=http://localhost:8000/mcp
```

## Regenerating

This directory is generated from `v2/.opencode/` by `v2/scripts/build-claude-plugin.sh`.
Edit the opencode sources and rebuild — direct edits here are overwritten.
EOF
ok "  README.md"

# Refresh the marketplace manifest version so `claude plugin update` sees changes.
MARKETPLACE="${REPO_DIR}/.claude-plugin/marketplace.json"
if [[ -f "${MARKETPLACE}" ]]; then
  ok "  marketplace: ${MARKETPLACE#${REPO_DIR}/}"
else
  warn "  no marketplace manifest at ${MARKETPLACE}"
fi

if command -v claude &>/dev/null; then
  info "Validating plugin manifest..."
  if claude plugin validate "${PLUGIN_DIR}" 2>&1; then
    ok "  plugin valid"
  else
    die "plugin validation failed"
  fi
  if [[ -f "${MARKETPLACE}" ]]; then
    info "Validating marketplace manifest..."
    claude plugin validate "${REPO_DIR}" 2>&1 && ok "  marketplace valid" || die "marketplace validation failed"
  fi
else
  warn "claude CLI not found — skipping validation"
fi

print ""
ok "Built ${PLUGIN_DIR}"
print ""
print "  Agents   : $(ls -1 ${PLUGIN_DIR}/agents 2>/dev/null | wc -l | tr -d ' ')"
print "  Commands : $(ls -1 ${PLUGIN_DIR}/commands 2>/dev/null | wc -l | tr -d ' ')"
print "  Skills   : $(ls -1 ${PLUGIN_DIR}/skills 2>/dev/null | wc -l | tr -d ' ')"
print ""
print "Install it with: ./scripts/setup-claude-code.sh"
print ""
