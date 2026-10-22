#!/usr/bin/env bash
# Harness smoke test (D13; v1 AMP ticket #2086, run -p leg #2095). Runs the built amp-harness binary against a real AMP.
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
# comment) created with the hidden `mcp call`, read back with `tickets list|get` and with `run -p` (the
# manager on a scripted faux model calling amp_get_task), and deleted again.
# M2-8 (#2099) adds, per leg: the manager dispatches worker-backend with amp_run_ticket (worker on its
# own faux model, in a worktree of a throwaway git repo under target/); the worker comments and
# completes with only task_id, and the ticket's run must carry the worker's model and tokens; a
# worker whose script runs out leaves an abandoned run with a note; and denied calls (the manager's
# bash, the worker writing outside its worktree) come back as visible tool errors and never run.
# M3-6 (#2110, smoke v3) adds, per leg: `debug render-chrome --live` must show the project's name
# and #id and the same status counts as amp_list_tasks (before and after a question is answered);
# a question is answered through the chat's /questions panel (`--snapshot --meta`, Enter, the
# answer, `y`) and the ticket must leave the Question state with the answer as a comment; and
# /open must print the ticket's web UI URL (AMP_SMOKE_WEB_URL on compose, the MCP origin on the
# cluster).
# AMP has no hard delete for projects, so "deleted" means its tickets, story and epic are deleted
# and the project is archived; the leg checks it is gone from amp_list_projects.
#
# keychain (#2165, opt-in: AMP_SMOKE_LEGS="keychain" or "compose keychain"; macOS only, skipped
#          elsewhere). The binary as installed (AMP_HARNESS_BIN=$(command -v amp-harness) to test the
#          installed one), no key in the environment, keys from the keychain only. With the real HOME
#          (the login keychain) but a throwaway service amp-harness-smoke-<pid>, never your entries.
#          `init --key-env` stores a dummy OpenRouter key through the harness; then `config show` and
#          `run -p` (faux manager, compose server) each run in a new process with keychain dialogs
#          turned off (AMP_HARNESS_KEYCHAIN_PROMPTS=never: a read that needs approval fails instead of
#          prompting), must find the key, and must read each keychain item at most once
#          (AMP_HARNESS_KEYCHAIN_TRACE). The item is deleted on exit.
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
# Compose serves the web UI on its own port (amp-ui, :5173), not on the MCP origin.
COMPOSE_WEB_URL="${AMP_SMOKE_WEB_URL:-http://localhost:5173}"

# Keep the cluster credentials aside and out of the environment, so no other leg ever sends them.
CLUSTER_TOKEN="${AMP_TOKEN:-}"
CLUSTER_CA="${AMP_CA_FILE:-}"
unset AMP_TOKEN AMP_CA_FILE AMP_MCP_URL AMP_MCP_PROTOCOL AMP_WEB_URL

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

# The keychain leg needs the real HOME: the macOS login keychain lives there.
REAL_HOME="$HOME"
KC_SERVICE=""

# --- isolation ------------------------------------------------------------------------------------
mkdir -p "$HARNESS_DIR/target"
WORK="$(mktemp -d "$HARNESS_DIR/target/smoke.XXXXXX")"
export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config" XDG_STATE_HOME="$WORK/home/.local/state"
export AMP_CREDENTIAL_STORE=file AMP_HARNESS_CEILING_DIR="$WORK"
unset AMP_HARNESS_PROFILE AMP_HARNESS_FAUX_SCRIPT
mkdir -p "$XDG_CONFIG_HOME/amp-harness" "$WORK/project"
# `run -p` (M2-4) and dispatch (M2-8) use scripted faux models: offline, no LLM key needed. The
# manager runs on provider `boss` and workers on `wf`, so the worker's own model is visible in the
# runs AMP records (the faux providers call themselves "faux" in messages; compare model refs).
cat >"$XDG_CONFIG_HOME/amp-harness/config.toml" <<'TOML'
[providers.boss]
api = "faux"
[providers.boss.models."boss-1"]
[providers.wf]
api = "faux"
[providers.wf.models."wf-1"]
[roles.manager]
model = "boss/boss-1"
[roles.worker-backend]
model = "wf/wf-1"
TOML
cd "$WORK/project"
# The manager's cwd is a throwaway git repo under target/, so workers get their worktrees at
# target/smoke.*/project/.worktrees/<ticket> (never /tmp); the exit trap removes all of it.
export GIT_AUTHOR_NAME=smoke GIT_AUTHOR_EMAIL=smoke@amp.local GIT_COMMITTER_NAME=smoke GIT_COMMITTER_EMAIL=smoke@amp.local
git init -q -b main . && printf 'smoke repo\n' >README.md && git add README.md && git commit -q -m init ||
  die "could not make the throwaway git repo"

# --- per-leg state + cleanup ----------------------------------------------------------------------
LEG="" LEG_URL="" LEG_PROTO="" LEG_TOKEN="" LEG_CA="" LEG_WEB=""
P_ID="" P_NAME="" EPIC_ID="" STORY_ID="" TASK_IDS=() LEG_WEB_BASE=""
RESULTS=()

# h ARGS...: run the harness against the current leg. Connection settings go through the
# environment (AMP_MCP_URL, AMP_MCP_PROTOCOL, AMP_TOKEN, AMP_CA_FILE, AMP_WEB_URL), never argv.
h() {
  local -a e=(AMP_MCP_URL="$LEG_URL" AMP_MCP_PROTOCOL="$LEG_PROTO")
  [[ -z "$LEG_TOKEN" ]] || e+=(AMP_TOKEN="$LEG_TOKEN" AMP_CA_FILE="$LEG_CA")
  [[ -z "$LEG_WEB" ]] || e+=(AMP_WEB_URL="$LEG_WEB")
  env "${e[@]}" "$BIN" "$@"
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
  if [[ -n "$KC_SERVICE" ]]; then
    security delete-generic-password -s "$KC_SERVICE" -a provider:openrouter >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
  if ((${#RESULTS[@]})); then
    printf 'smoke: %s\n' "${RESULTS[@]}"
  fi
  if ((rc == 0)); then log "OK"; else echo "smoke: FAILED" >&2; fi
  exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT TERM

# --- dispatch, abandoned run, policy deny (M2-8) -----------------------------------------------
# runs_of TASK: the ticket with its comments and runs.
runs_of() { call amp_get_task "{\"task_id\":$1,\"include\":[\"comments\",\"runs\"]}"; }

# dispatch_cases TICKET: needs .amp.json for the leg's project. TICKET must be ready.
dispatch_cases() {
  local t2="$1" t3 out report

  # 1. The manager (faux `boss`) first tries a bash command its policy denies, then dispatches
  #    worker-backend on TICKET with amp_run_ticket. The worker (faux `wf`, its own model, in its
  #    own worktree) reads the ticket, tries to write outside its worktree (denied), comments and
  #    completes with only task_id: the harness fills model and tokens (M2-6).
  jq -nc --argjson t "$t2" '{
    boss: [
      {tool_calls: [{name: "bash", arguments: {command: "touch denied-by-policy"}}]},
      {tool_calls: [{name: "amp_run_ticket", arguments: {task_id: $t, agent_id: "amp:amp-worker-backend"}}]},
      {text: "Worker report: {{last_tool_result}}"}],
    wf: [
      {tool_calls: [{name: "amp_get_task", arguments: {task_id: $t}}]},
      {tool_calls: [{name: "write", arguments: {path: "../escape.txt", content: "must not be written"}}]},
      {tool_calls: [{name: "amp_add_task_comment", arguments: {task_id: $t, body: "smoke worker comment", author: "amp-worker"}}]},
      {tool_calls: [{name: "amp_complete_task", arguments: {task_id: $t}}]},
      {text: "Done."}]}' >"$WORK/dispatch.json"
  out="$(AMP_HARNESS_FAUX_SCRIPT="$WORK/dispatch.json" h run -p "Run ticket #$t2" --json --no-session)" || {
    printf '%s\n' "$out" | tail -c 4000 >&2
    die "[$LEG] dispatch: run -p failed"
  }
  # Policy deny: the manager sees a tool error naming the policy, the command never ran, the run went on.
  jq -s -e '
      (.[-1] | .type == "result" and .outcome == "completed" and .tools_used == ["bash", "amp_run_ticket"]) and
      ([.[] | select(.type == "tool_end" and .name == "bash")][0]
        | .is_error == true and (.text | contains("Denied by the amp-manager tool policy")))' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" | tail -c 4000 >&2
    die "[$LEG] policy deny: expected a visible denial of the manager's bash call"
  }
  [[ ! -e denied-by-policy ]] || die "[$LEG] policy deny: the denied bash command ran"
  report="$(jq -s -c '[.[] | select(.type == "tool_end" and .name == "amp_run_ticket")][0]' <<<"$out")"
  jq -e --argjson t "$t2" '.is_error == false and (.text | fromjson
      | .task_id == $t and .outcome == "completed" and .agent_id == "amp:amp-worker-backend"
        and .model == "wf/wf-1" and .tool_errors == 1 and (.worktree | type == "string")
        and .usage.input_tokens > 0)' >/dev/null <<<"$report" || {
    printf '%s\n' "$report" | head -c 4000 >&2
    die "[$LEG] dispatch: amp_run_ticket did not report a completed worker on wf/wf-1 with one denied call"
  }
  local wt
  wt="$(jq -r '.text | fromjson | .worktree' <<<"$report")"
  [[ "$wt" == "$WORK/project/.worktrees/$t2" || "$wt" == "$(cd "$WORK/project" && pwd -P)/.worktrees/$t2" ]] ||
    die "[$LEG] dispatch: worker worktree $wt is not under the throwaway repo"
  [[ ! -e "$(dirname "$wt")/escape.txt" ]] || die "[$LEG] policy deny: the worker wrote outside its worktree"
  out="$(runs_of "$t2")"
  jq -e '(.task.state == "completed") and ([.comments[]? | .body] | index("smoke worker comment")) != null and
      ([.runs[]? | select(.outcome == "completed" and .model == "wf/wf-1"
        and ((.input_tokens // 0) + (.output_tokens // 0)) > 0)] | length) == 1' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" | head -c 4000 >&2
    die "[$LEG] dispatch: expected ticket $t2 completed, the worker comment, and a completed run on wf/wf-1 with tokens"
  }
  log "[$LEG] dispatch ok: worker-backend on wf/wf-1 completed #$t2 (run has model and tokens); policy denials visible"

  # 2. Abandoned run: the worker's script runs out after one step, so it ends as `error` without
  #    closing its ticket, and the harness reports an abandoned run with a note.
  out="$(call amp_create_tasks "$(jq -nc --argjson p "$P_ID" --argjson s "$STORY_ID" '{project_id:$p, tasks:[
      {ref:"third", story_id:$s, name:"smoke ticket three", description:"Throwaway; its worker gives up.", acceptance_criteria:"None.", assigned_to:"amp:amp-worker-backend"}]}')")"
  t3="$(jq -r '.created[]? | select(.ref == "third") | .id' <<<"$out")"
  [[ "$t3" =~ ^[0-9]+$ ]] || die "[$LEG] amp_create_tasks: no id for ticket three"
  TASK_IDS+=("$t3")
  jq -nc --argjson t "$t3" '{
    boss: [
      {tool_calls: [{name: "amp_run_ticket", arguments: {task_id: $t, agent_id: "amp:amp-worker-backend"}}]},
      {text: "Worker report: {{last_tool_result}}"}],
    wf: [{tool_calls: [{name: "amp_get_task", arguments: {task_id: $t}}]}]}' >"$WORK/abandon.json"
  out="$(AMP_HARNESS_FAUX_SCRIPT="$WORK/abandon.json" h run -p "Run ticket #$t3" --json --no-session)" || {
    printf '%s\n' "$out" | tail -c 4000 >&2
    die "[$LEG] abandoned: run -p failed"
  }
  report="$(jq -s -c '[.[] | select(.type == "tool_end" and .name == "amp_run_ticket")][0]' <<<"$out")"
  jq -e '.is_error == false and (.text | fromjson | .outcome == "error"
      and ([.notes[]? | select(test("amp_report_run|abandoned"; "i"))] | length) > 0)' >/dev/null <<<"$report" || {
    printf '%s\n' "$report" | head -c 4000 >&2
    die "[$LEG] abandoned: expected the worker to end as error with a run-report note"
  }
  out="$(runs_of "$t3")"
  jq -e '(.task.state == "in_progress") and
      ([.runs[]? | select(.outcome == "abandoned" and .model == "wf/wf-1" and .agent_id == "amp:amp-worker-backend"
        and (.note | startswith("abandoned: error:") and (startswith("abandoned: error: error") | not))
        and ((.input_tokens // 0) + (.output_tokens // 0)) > 0)] | length) == 1' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" | head -c 4000 >&2
    die "[$LEG] abandoned: expected one abandoned run on #$t3 with a note, the worker model and tokens"
  }
  log "[$LEG] abandoned ok: #$t3 has an abandoned run ($(jq -r '[.runs[] | select(.outcome == "abandoned")][0].note' <<<"$out" | cut -c1-80))"
}

# --- live chrome, answering a question, /open (M3-6, smoke v3) -------------------------------------
# check_chrome TICKET WHAT: `debug render-chrome --live` shows the leg's project (name and #id) and
# the same status-line counts as AMP's own compact amp_list_tasks, with TICKET on the second line.
check_chrome() {
  local t="$1" what="$2" out list status n label
  out="$(h debug render-chrome --live --project "$P_ID" --ticket "$t" --plain --width 220 --wait 20)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] render-chrome --live ($what) failed or did not finish loading"
  }
  list="$(call amp_list_tasks "{\"project_id\":$P_ID,\"owner\":\"any\"}")"
  (($(wc -l <<<"$out") == 3)) || {
    printf '%s\n' "$out" >&2
    die "[$LEG] render-chrome --live ($what): expected two header lines and the status line"
  }
  local line1 line2
  line1="$(sed -n 1p <<<"$out")" line2="$(sed -n 2p <<<"$out")" status="$(sed -n 3p <<<"$out")"
  [[ "$line1" == *"$P_NAME #$P_ID"* ]] || die "[$LEG] chrome ($what): header has no '$P_NAME #$P_ID': $line1"
  [[ "$line1" == *"● connected"* ]] || die "[$LEG] chrome ($what): header is not connected: $line1"
  [[ "$line2" == *"#$t "* ]] || die "[$LEG] chrome ($what): second line is not ticket #$t: $line2"
  # The counts the status line draws (Board::nonzero): questions as the chip, then In progress,
  # Ready and Scheduled; a zero count is left out, and nothing open at all reads "no open tickets".
  local nq ni nr ns
  nq="$(jq '.questions | length' <<<"$list")" ni="$(jq '.in_progress | length' <<<"$list")"
  nr="$(jq '.ready_to_dispatch | length' <<<"$list")" ns="$(jq '.scheduled // 0' <<<"$list")"
  if ((nq > 0)); then
    label=QUESTION; ((nq == 1)) || label=QUESTIONS
    [[ "$status" == *"■  $nq $label "* ]] || die "[$LEG] chrome ($what): expected the '$nq $label' chip: $status"
  else
    [[ "$status" != *QUESTION* ]] || die "[$LEG] chrome ($what): a questions chip with no question on the board: $status"
  fi
  for pair in "$ni:In progress" "$nr:Ready" "$ns:Scheduled"; do
    n="${pair%%:*}" label="${pair#*:}"
    if ((n > 0)); then
      [[ "$status" == *" $n $label"* ]] || die "[$LEG] chrome ($what): expected '$n $label' (amp_list_tasks): $status"
    else
      [[ "$status" != *" $label"* ]] || die "[$LEG] chrome ($what): '$label' shown but amp_list_tasks has none: $status"
    fi
  done
  if ((nq + ni + nr + ns == 0)); then
    [[ "$status" == *"no open tickets"* ]] || die "[$LEG] chrome ($what): expected 'no open tickets': $status"
  fi
  log "[$LEG] chrome ok ($what): ${status}"
}

# chrome_cases STAMP: needs .amp.json for the leg's project. Adds a ticket with a question.
chrome_cases() {
  local stamp="$1" t4 out
  out="$(call amp_create_tasks "$(jq -nc --argjson p "$P_ID" --argjson s "$STORY_ID" '{project_id:$p, tasks:[
      {ref:"fourth", story_id:$s, name:"smoke question ticket", description:"Throwaway; it asks a question.", acceptance_criteria:"None.", assigned_to:"amp:amp-worker-backend"}]}')")"
  t4="$(jq -r '.created[]? | select(.ref == "fourth") | .id' <<<"$out")"
  [[ "$t4" =~ ^[0-9]+$ ]] || die "[$LEG] amp_create_tasks: no id for the question ticket"
  TASK_IDS+=("$t4")
  call amp_block_task "{\"task_id\":$t4,\"reason\":\"Smoke question $stamp: red or blue?\"}" >/dev/null
  out="$(call amp_list_tasks "{\"project_id\":$P_ID,\"owner\":\"any\"}")"
  expect "$out" "[.questions[].id] == [$t4]" "expected #$t4 as the board's only question"

  # 1. render-chrome --live: project, counts (a question, the abandoned worker's ticket in progress).
  check_chrome "$t4" "with a question"

  # 2. Answer it through the chat's /questions panel: Enter on the question, the answer, then y.
  out="$(h --no-session --snapshot 140x40 --meta --input /questions --input key:enter \
    --input "smoke answer $stamp" --input key:y)" || {
    printf '%s\n' "$out" | tail -c 4000 >&2
    die "[$LEG] /questions: the snapshot chat failed"
  }
  grep -q "answered #$t4" <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] /questions: no 'answered #$t4' after key:y"
  }
  out="$(runs_of "$t4")"
  jq -e --arg a "smoke answer $stamp" '.task.state != "blocked" and ([.comments[]?.body] | index($a)) != null' \
    >/dev/null <<<"$out" || {
    printf '%s\n' "$out" | head -c 4000 >&2
    die "[$LEG] /questions: #$t4 is still a question, or the answer is not on it"
  }
  out="$(call amp_list_tasks "{\"project_id\":$P_ID,\"owner\":\"any\"}")"
  expect "$out" '.questions == []' "the board still lists a question after the answer"
  log "[$LEG] /questions ok: answered #$t4, now $(jq -r .task.state <<<"$(runs_of "$t4")")"
  check_chrome "$t4" "after the answer"

  # 3. /open prints the ticket's web UI URL (the snapshot never opens a browser).
  local url="$LEG_WEB_BASE/project/$P_ID/board?task=$t4"
  out="$(h --no-session --snapshot 200x30 --meta --input "/ticket $t4" --input /open)" || {
    printf '%s\n' "$out" | tail -c 4000 >&2
    die "[$LEG] /open: the snapshot chat failed"
  }
  grep -qF "/open $url: --snapshot does not open a browser" <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] /open: expected $url"
  }
  local code
  code="$(curl -s -o /dev/null -m 5 -w '%{http_code}' ${LEG_CA:+--cacert "$LEG_CA"} "$url" || true)"
  if [[ "$code" == 200 ]]; then
    log "[$LEG] /open ok: $url (the web UI answers 200)"
  else
    log "[$LEG] /open ok: $url (WARN: the web UI answered '${code:-nothing}', is it up?)"
  fi
}

# --- one leg --------------------------------------------------------------------------------------
# run_leg NAME URL PROTOCOL TOKEN CA EXPECT_EMAIL ('' = any) WEB_URL ('' = the MCP URL's origin)
run_leg() {
  LEG="$1/$3" LEG_URL="$2" LEG_PROTO="$3" LEG_TOKEN="$4" LEG_CA="$5" LEG_WEB="$7"
  LEG_WEB_BASE="${LEG_WEB:-$(sed -E 's#^([a-z]+://[^/?#]+).*#\1#' <<<"$LEG_URL")}"
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
  P_NAME="harness-smoke $LEG $stamp"
  out="$(call amp_create_project "{\"name\":\"$P_NAME\",\"code\":\"$code\",\"description\":\"Throwaway project from v2/harness/scripts/smoke.sh; deleted by the run.\"}")"
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

  # run -p (M2-4): the manager, on a scripted faux model, reads ticket two through AMP's own
  # amp_get_task tool, and the tool's result reaches the model.
  printf '[{"tool_calls":[{"name":"amp_get_task","arguments":{"task_id":%s}}]},{"text":"Ticket: {{last_tool_result}}"}]\n' "$t2" >"$WORK/faux.json"
  out="$(AMP_HARNESS_FAUX_SCRIPT="$WORK/faux.json" h run -p "Read ticket #$t2" --json --no-session)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] run -p failed"
  }
  jq -s -e --argjson p "$P_ID" '
      (.[0] | .type == "session" and .role == "amp-manager" and .project_id == $p and .mcp.instructions == true
        and (.tools | index("amp_get_task")) != null and ([.tools[] | select(startswith("amp_amp_"))] == [])) and
      (.[-1] | .type == "result" and .outcome == "completed" and .tools_used == ["amp_get_task"] and .tool_errors == 0
        and (.text | contains("smoke ticket two")))' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] run -p: expected the manager to read ticket $t2 with amp_get_task"
  }
  log "[$LEG] run -p ok: the manager called amp_get_task on the faux model"

  dispatch_cases "$t2"
  chrome_cases "$stamp"

  # A tool error exits 1 with the message on stderr.
  rc=0
  h mcp call amp_get_task '{"task_id": 999999999}' >/dev/null 2>"$WORK/err" || rc=$?
  ((rc == 1)) || die "[$LEG] mcp call of a missing ticket: expected exit 1, got $rc"
  [[ -s "$WORK/err" ]] || die "[$LEG] mcp call of a missing ticket printed no error"

  rm -f .amp.json
  cleanup_leg || die "[$LEG] cleanup failed"
  RESULTS+=("PASS $LEG ($proto_seen)")
}

# --- keychain only (#2165) -------------------------------------------------------------------------
# kc_check PROCESS: the trace holds one process that read each item at most once, found the
# OpenRouter key, and hit no keychain error.
kc_check() {
  local trace="$WORK/keychain-trace.tsv"
  [[ -s "$trace" ]] || die "[keychain] $1: no keychain calls traced"
  awk -F'\t' -v what="$1" '
    { pids[$5] = 1 }
    $1 == "read" { reads[$3]++ }
    $1 == "read" && $3 == "provider:openrouter" && $4 == "found" { found = 1 }
    $4 ~ /^error/ { print "smoke: [keychain] " what ": keychain error: " $1 " " $3 " " $4 > "/dev/stderr"; bad = 1 }
    END {
      n = 0; for (p in pids) n++
      if (n != 1) { print "smoke: [keychain] " what ": expected one process, saw " n > "/dev/stderr"; bad = 1 }
      for (a in reads) if (reads[a] > 1) { print "smoke: [keychain] " what ": " a " read " reads[a] " times" > "/dev/stderr"; bad = 1 }
      if (!found) { print "smoke: [keychain] " what ": the OpenRouter key was not read from the keychain" > "/dev/stderr"; bad = 1 }
      exit bad
    }' "$trace" || die "[keychain] $1: keychain calls not as expected (trace: $(tr '\t' ' ' <"$trace" | tr '\n' ';'))"
  log "[keychain] $1 ok: $(awk -F'\t' '$1 == "read" { n++ } END { print n + 0 }' "$trace") keychain reads, one per item, no dialog"
}

keychain_leg() {
  LEG="keychain"
  if [[ "$(uname -s)" != Darwin ]]; then
    RESULTS+=("SKIP keychain (macOS only)")
    return 0
  fi
  KC_SERVICE="amp-harness-smoke-$$"
  local trace="$WORK/keychain-trace.tsv" out
  log "== leg keychain: $BIN, service $KC_SERVICE, no key in the environment"
  # k ARGS...: the binary with the real HOME, the keychain store under the throwaway service, no
  # provider key in the environment, no stdin, and keychain dialogs off.
  k() {
    env -u OPENROUTER_API_KEY -u ANTHROPIC_API_KEY HOME="$REAL_HOME" AMP_CREDENTIAL_STORE=keychain \
      AMP_HARNESS_KEYCHAIN_SERVICE="$KC_SERVICE" AMP_HARNESS_KEYCHAIN_PROMPTS=never \
      AMP_HARNESS_KEYCHAIN_TRACE="$trace" AMP_MCP_URL="$COMPOSE_URL" AMP_WEB_URL="$COMPOSE_WEB_URL" \
      "$@" "$BIN" "${KARGS[@]}" </dev/null
  }
  rm -f "$trace" .amp.json
  KARGS=(init --yes --no-project --key-env AMP_SMOKE_DUMMY_KEY --provider openrouter --json)
  out="$(k AMP_SMOKE_DUMMY_KEY="sk-or-v1-smoke-dummy-$$")" || die "[keychain] init could not store the key"
  grep -q $'^write\t'"$KC_SERVICE"$'\tprovider:openrouter\tok' "$trace" ||
    die "[keychain] init did not write the key through the harness"
  log "[keychain] init stored a dummy OpenRouter key through the harness"

  rm -f "$trace"
  KARGS=(config show --json)
  out="$(k)" || die "[keychain] config show failed"
  expect "$out" '.providers | tostring | contains("keychain")' "config show does not report the keychain as a key source"
  kc_check "config show"

  rm -f "$trace"
  printf '[{"text": "keychain smoke"}]\n' >"$WORK/kc-script.json"
  KARGS=(run -p "say hi" --json)
  out="$(k AMP_HARNESS_FAUX_SCRIPT="$WORK/kc-script.json")" || {
    printf '%s\n' "$out" >&2
    die "[keychain] run -p failed"
  }
  kc_check "run -p"
  RESULTS+=("PASS keychain")
}

for leg in $LEGS; do
  case "$leg" in
    keychain) keychain_leg ;;
    compose)
      for proto in $PROTOCOLS; do run_leg compose "$COMPOSE_URL" "$proto" "" "" "$COMPOSE_EMAIL" "$COMPOSE_WEB_URL"; done ;;
    cluster)
      if [[ -z "$CLUSTER_TOKEN" || -z "$CLUSTER_CA" ]]; then
        RESULTS+=("SKIP cluster (set AMP_TOKEN and AMP_CA_FILE to run it against $CLUSTER_URL)")
        continue
      fi
      [[ -r "$CLUSTER_CA" ]] || die "AMP_CA_FILE $CLUSTER_CA is not readable"
      for proto in $PROTOCOLS; do run_leg cluster "$CLUSTER_URL" "$proto" "$CLUSTER_TOKEN" "$CLUSTER_CA" "" ""; done ;;
    *) die "unknown leg '$leg' in AMP_SMOKE_LEGS" ;;
  esac
done
