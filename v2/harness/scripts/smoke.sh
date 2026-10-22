#!/usr/bin/env bash
# Harness smoke test v1 (D13, AMP ticket #2086). Runs the built amp-harness binary against a real AMP.
#
#   make -C v2/harness smoke          # builds first, then runs this script
#
# Legs (AMP_SMOKE_LEGS, default "compose cluster"):
#   compose  AMP_MCP_URL (default http://localhost:8000/mcp): auth off, so you are local-dev.
#            Runs once per protocol in AMP_SMOKE_PROTOCOLS (default "auto 2025-11-25", i.e. Auto
#            negotiation, which must land on the stateless 2026-07-28 era, and a forced legacy run).
#   cluster  Only when AMP_TOKEN (an amp_ token from the UI) and AMP_CA_FILE (amp-local-ca.crt) are set.
#            URL AMP_SMOKE_CLUSTER_URL (default https://amp.local/mcp, envoy on 443). Same protocols.
#            Otherwise it is reported as skipped. The token is only ever sent to the cluster leg.
#
# Each leg: doctor --json, whoami --json, then a throwaway project (epic, story, two tickets, a
# comment) created with the hidden `mcp call`, read back with `tickets list|get`, and deleted again.
# AMP has no hard delete for projects, so "deleted" means its tickets, story and epic are deleted
# and the project is archived; the leg checks it is gone from amp_list_projects.
#
# Exits non-zero on any failure. Cleanup runs on every exit path (trap), including failures.
# The harness runs with HOME/XDG_CONFIG_HOME under target/ and AMP_CREDENTIAL_STORE=file, so your
# real keychain and config.toml are never read or written.
set -euo pipefail

cd "$(dirname "$0")/.."
HARNESS_DIR="$PWD"
BIN="${AMP_HARNESS_BIN:-$HARNESS_DIR/target/debug/amp-harness}"
COMPOSE_URL="${AMP_MCP_URL:-http://localhost:8000/mcp}"
CLUSTER_URL="${AMP_SMOKE_CLUSTER_URL:-https://amp.local/mcp}"
PROTOCOLS="${AMP_SMOKE_PROTOCOLS:-auto 2025-11-25}"
LEGS="${AMP_SMOKE_LEGS:-compose cluster}"
COMPOSE_EMAIL="${AMP_SMOKE_COMPOSE_EMAIL:-local-dev@amp.local}"

# Keep the cluster credentials aside and out of the environment, so no other leg ever sends them.
CLUSTER_TOKEN="${AMP_TOKEN:-}"
CLUSTER_CA="${AMP_CA_FILE:-}"
unset AMP_TOKEN AMP_CA_FILE AMP_MCP_URL AMP_MCP_PROTOCOL

log() { printf 'smoke: %s\n' "$*"; }
die() {
  printf 'smoke: FAIL: %s\n' "$*" >&2
  exit 1
}

command -v jq >/dev/null || die "jq is required"
[[ -x "$BIN" ]] || die "$BIN not found; run make -C v2/harness build first"

# --- version (M1-1) -------------------------------------------------------------------------------
out="$("$BIN" --version)"
for name in rpi-ai rpi-agent rpi-tools rpi-harness rpi-tui rmcp; do
  grep -Eq "^${name} [0-9]+\.[0-9]+\.[0-9]+$" <<<"$out" || die "--version has no ${name} version"
done
log "version ok ($(head -1 <<<"$out"))"

# --- isolation ------------------------------------------------------------------------------------
mkdir -p "$HARNESS_DIR/target"
WORK="$(mktemp -d "$HARNESS_DIR/target/smoke.XXXXXX")"
export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config"
export AMP_CREDENTIAL_STORE=file AMP_HARNESS_CEILING_DIR="$WORK"
mkdir -p "$XDG_CONFIG_HOME" "$WORK/project"
cd "$WORK/project"

# --- per-leg state + cleanup ----------------------------------------------------------------------
LEG="" LEG_URL="" LEG_PROTO="" LEG_TOKEN="" LEG_CA=""
P_ID="" EPIC_ID="" STORY_ID="" TASK_IDS=()
RESULTS=()

# h ARGS...: run the harness against the current leg. Connection settings go through the
# environment (AMP_MCP_URL, AMP_MCP_PROTOCOL, AMP_TOKEN, AMP_CA_FILE), never argv.
h() {
  if [[ -n "$LEG_TOKEN" ]]; then
    AMP_MCP_URL="$LEG_URL" AMP_MCP_PROTOCOL="$LEG_PROTO" AMP_TOKEN="$LEG_TOKEN" AMP_CA_FILE="$LEG_CA" \
      "$BIN" "$@"
  else
    AMP_MCP_URL="$LEG_URL" AMP_MCP_PROTOCOL="$LEG_PROTO" "$BIN" "$@"
  fi
}

# call TOOL JSON: `mcp call`, prints the tool's JSON; fails the leg on a tool error.
call() {
  local out
  out="$(h mcp call "$1" "$2")" || die "[$LEG] mcp call $1 failed"
  printf '%s\n' "$out"
}

# expect JSON JQ-FILTER WHAT: assert a jq filter is true on JSON.
expect() {
  jq -e "$2" >/dev/null <<<"$1" || {
    printf '%s\n' "$1" | head -c 4000 >&2
    die "[$LEG] $3"
  }
}

cleanup_leg() {
  [[ -n "$P_ID" ]] || return 0
  local failed=0 id
  for id in "${TASK_IDS[@]+"${TASK_IDS[@]}"}"; do
    h mcp call amp_delete_task "{\"task_id\":$id}" >/dev/null 2>&1 || { failed=1; echo "smoke: cleanup: could not delete ticket $id" >&2; }
  done
  if [[ -n "$STORY_ID" ]]; then
    h mcp call amp_delete_story "{\"story_id\":$STORY_ID}" >/dev/null 2>&1 || { failed=1; echo "smoke: cleanup: could not delete story $STORY_ID" >&2; }
  fi
  if [[ -n "$EPIC_ID" ]]; then
    h mcp call amp_delete_epic "{\"epic_id\":$EPIC_ID}" >/dev/null 2>&1 || { failed=1; echo "smoke: cleanup: could not delete epic $EPIC_ID" >&2; }
  fi
  h mcp call amp_archive_project "{\"project_id\":$P_ID}" >/dev/null 2>&1 || { failed=1; echo "smoke: cleanup: could not archive project $P_ID" >&2; }
  local listed
  if listed="$(h mcp call amp_list_projects '{}' 2>/dev/null)" &&
    jq -e --argjson id "$P_ID" '[(.projects // .)[]? | .id] | index($id) | not' >/dev/null <<<"$listed"; then
    log "[$LEG] cleanup ok: project $P_ID archived, not in amp_list_projects"
  else
    failed=1
    echo "smoke: cleanup: project $P_ID is still listed (or the list failed)" >&2
  fi
  P_ID="" EPIC_ID="" STORY_ID="" TASK_IDS=()
  return "$failed"
}

on_exit() {
  local rc=$?
  trap - EXIT
  cleanup_leg || rc=1
  rm -rf "$WORK"
  if ((${#RESULTS[@]})); then
    printf 'smoke: %s\n' "${RESULTS[@]}"
  fi
  if ((rc == 0)); then log "OK"; else echo "smoke: FAILED" >&2; fi
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT TERM

# --- one leg --------------------------------------------------------------------------------------
# run_leg NAME URL PROTOCOL TOKEN CA EXPECT_EMAIL ('' = any)
run_leg() {
  LEG="$1/$3" LEG_URL="$2" LEG_PROTO="$3" LEG_TOKEN="$4" LEG_CA="$5"
  local want_email="$6" out rc
  log "== leg $LEG: $LEG_URL"
  rm -f .amp.json

  # doctor: connected, all contract tools, the right protocol era.
  rc=0
  out="$(h doctor --json)" || rc=$?
  expect "$out" '.connection.connected and .ok' "doctor not ok (exit $rc)"
  expect "$out" '.tools.missing_contract == [] and .tools.count > 0' "contract tools missing"
  case "$LEG_PROTO" in
    auto | 2026-07-28)
      expect "$out" '.connection.protocol_version == "2026-07-28" and .connection.era == "stateless" and .connection.fell_back == false' \
        "expected the stateless 2026-07-28 era" ;;
    2025-11-25)
      expect "$out" '.connection.protocol_version == "2025-11-25" and .connection.era == "legacy"' \
        "expected the legacy 2025-11-25 era" ;;
  esac
  expect "$out" '.connection.instructions == true' "server sent no instructions"
  expect "$out" '[.checks[] | select(.name == "project")][0].status == "skip"' "project check should skip without .amp.json"
  local proto_seen
  proto_seen="$(jq -r '"\(.connection.protocol_version) \(.connection.era)"' <<<"$out")"
  log "[$LEG] doctor ok: $proto_seen, $(jq -r .tools.count <<<"$out") tools, auth $(jq -r .auth.method <<<"$out")"

  # whoami
  out="$(h whoami --json)" || die "[$LEG] whoami failed"
  if [[ -n "$want_email" ]]; then
    jq -e --arg e "$want_email" '.email == $e' >/dev/null <<<"$out" || die "[$LEG] whoami: expected $want_email, got $(jq -r .email <<<"$out")"
  else
    expect "$out" '.email | type == "string" and length > 0' "whoami has no email"
  fi
  log "[$LEG] whoami ok: $(jq -r .email <<<"$out")"

  # throwaway project (the code must be unique: 2-6 letters)
  local code stamp
  code="SM$(LC_ALL=C tr -dc '[:upper:]' </dev/urandom | head -c 4 || true)"
  stamp="$(date +%Y%m%d-%H%M%S)"
  out="$(call amp_create_project "{\"name\":\"harness-smoke $LEG $stamp\",\"code\":\"$code\",\"description\":\"Throwaway project from v2/harness/scripts/smoke.sh; deleted by the run.\"}")"
  P_ID="$(jq -r '.id // .project.id // empty' <<<"$out")"
  [[ "$P_ID" =~ ^[0-9]+$ ]] || die "[$LEG] amp_create_project returned no id"
  out="$(call amp_create_epic "{\"project_id\":$P_ID,\"name\":\"smoke epic\"}")"
  EPIC_ID="$(jq -r '.id // .epic.id // empty' <<<"$out")"
  [[ "$EPIC_ID" =~ ^[0-9]+$ ]] || die "[$LEG] amp_create_epic returned no id"
  out="$(call amp_create_story "{\"project_id\":$P_ID,\"epic_id\":$EPIC_ID,\"name\":\"smoke story\"}")"
  STORY_ID="$(jq -r '.id // .story.id // empty' <<<"$out")"
  [[ "$STORY_ID" =~ ^[0-9]+$ ]] || die "[$LEG] amp_create_story returned no id"
  out="$(call amp_create_tasks "$(jq -nc --argjson p "$P_ID" --argjson s "$STORY_ID" '{project_id:$p, tasks:[
      {ref:"first",  story_id:$s, name:"smoke ticket one", description:"Throwaway.", acceptance_criteria:"None.", assigned_to:"amp:amp-worker-backend"},
      {ref:"second", story_id:$s, name:"smoke ticket two", description:"Throwaway.", acceptance_criteria:"None.", assigned_to:"amp:amp-worker-backend", depends_on:["first"]}]}')")"
  local t1 t2
  # Record every created id first, so cleanup deletes them even if a check below fails.
  read -r -a TASK_IDS <<<"$(jq -r '[.created[]?.id] | map(tostring) | join(" ")' <<<"$out")"
  t1="$(jq -r '.created[]? | select(.ref == "first") | .id' <<<"$out")"
  t2="$(jq -r '.created[]? | select(.ref == "second") | .id' <<<"$out")"
  [[ "$t1" =~ ^[0-9]+$ && "$t2" =~ ^[0-9]+$ ]] || {
    printf '%s\n' "$out" >&2
    die "[$LEG] amp_create_tasks: could not read the ticket ids"
  }
  call amp_add_task_comment "{\"task_id\":$t1,\"body\":\"smoke comment $stamp\",\"author\":\"smoke\"}" >/dev/null
  log "[$LEG] created project $P_ID ($code), epic $EPIC_ID, story $STORY_ID, tickets $t1 $t2"

  # tickets list: both open tickets, the second waiting on the first.
  out="$(h tickets list --project "$P_ID" --owner any --json)" || die "[$LEG] tickets list failed"
  jq -e --argjson p "$P_ID" --argjson a "$t1" --argjson b "$t2" '
      .project_id == $p and .count == 2 and
      ([.tickets[].id] | sort) == ([$a, $b] | sort) and
      (.tickets[] | select(.id == $a) | .status == "ready" and .name == "smoke ticket one") and
      (.tickets[] | select(.id == $b) | .status == "waiting" and .blocked_by_ids == [$a])' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] tickets list: unexpected rows"
  }

  # tickets get, with comments.
  out="$(h tickets get "#$t1" --comments --json)" || die "[$LEG] tickets get failed"
  jq -e --argjson a "$t1" --argjson p "$P_ID" --arg c "smoke comment $stamp" '
      .ticket.id == $a and .ticket.project_id == $p and .ticket.name == "smoke ticket one" and
      .ticket.description == "Throwaway." and ([.comments[].body] | index($c)) != null' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] tickets get: unexpected ticket"
  }

  # Finish ticket one: the default list hides done tickets, --state all shows them, and the
  # project comes from .amp.json when --project is left out.
  call amp_complete_task "{\"task_id\":$t1}" >/dev/null
  printf '{"project_id": %s, "project_name": "harness smoke"}\n' "$P_ID" >.amp.json
  out="$(h tickets list --owner any --json)" || die "[$LEG] tickets list (from .amp.json) failed"
  jq -e --argjson p "$P_ID" --argjson b "$t2" '
      .project_id == $p and .count == 1 and .tickets[0].id == $b and .tickets[0].status == "ready"' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] tickets list after completing ticket one: expected only ticket two, now ready"
  }
  out="$(h tickets list --owner any --state all --json)" || die "[$LEG] tickets list --state all failed"
  jq -e --argjson a "$t1" '.count == 2 and (.tickets[] | select(.id == $a) | .status == "done")' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] tickets list --state all: expected ticket one as done"
  }
  out="$(h doctor --json)" || die "[$LEG] doctor with .amp.json failed"
  expect "$out" ".project.found and .project.id == $P_ID" "doctor did not find the .amp.json project"

  # A tool error exits 1 with the message on stderr.
  rc=0
  h mcp call amp_get_task '{"task_id": 999999999}' >/dev/null 2>"$WORK/err" || rc=$?
  ((rc == 1)) || die "[$LEG] mcp call of a missing ticket: expected exit 1, got $rc"
  [[ -s "$WORK/err" ]] || die "[$LEG] mcp call of a missing ticket printed no error"

  rm -f .amp.json
  cleanup_leg || die "[$LEG] cleanup failed"
  RESULTS+=("PASS $LEG ($proto_seen)")
}

for leg in $LEGS; do
  case "$leg" in
    compose)
      for proto in $PROTOCOLS; do run_leg compose "$COMPOSE_URL" "$proto" "" "" "$COMPOSE_EMAIL"; done ;;
    cluster)
      if [[ -z "$CLUSTER_TOKEN" || -z "$CLUSTER_CA" ]]; then
        RESULTS+=("SKIP cluster (set AMP_TOKEN and AMP_CA_FILE to run it against $CLUSTER_URL)")
        continue
      fi
      [[ -r "$CLUSTER_CA" ]] || die "AMP_CA_FILE $CLUSTER_CA is not readable"
      for proto in $PROTOCOLS; do run_leg cluster "$CLUSTER_URL" "$proto" "$CLUSTER_TOKEN" "$CLUSTER_CA" ""; done ;;
    *) die "unknown leg '$leg' in AMP_SMOKE_LEGS" ;;
  esac
done
