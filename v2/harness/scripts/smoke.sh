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
# comment) created with `mcp call`, read back with `tickets list|get` and with `run -p` (the
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
#          (AMP_HARNESS_KEYCHAIN_TRACE). `connect --list` must report the key's source and
#          `disconnect openrouter` must remove the item (#2163). The item is deleted on exit.
#
# M4-5 (#2118, smoke v4) follows the human-only-approval server (epic 162): the compose and cluster
# legs create all three dispatchable tickets up front and approve the plan once, as the person (the
# compose leg POSTs /api/projects/P/approve-plan on AMP_SMOKE_REST_URL, which counts as the web UI on
# an auth-off compose; the cluster leg tries amp_approve_plan, which only older servers allow, and
# otherwise waits AMP_SMOKE_APPROVE_WAIT seconds for a person to approve at the printed link); an
# agent dispatches a ticket before completing it; and on compose a person's (human) ticket is refused
# over MCP and marked done through REST. Two new legs, both against the compose server:
# firstrun  A brand-new HOME (under target/), no config: the chat's first run with scripted answers
#           (AMP_HARNESS_PROMPTS=stdin: server as host:port, n, name, code) must save the server and
#           web UI, create a project, write .amp.json at the git root and prefill the editor; a second
#           start asks nothing; then `init --yes --url --project N --json` in another new HOME links a
#           second folder and `doctor --json` there (no env) is ok. The project is archived on exit.
# install   Packs the binary under test with scripts/package.sh (or AMP_SMOKE_ARCHIVE, a real release
#           archive), installs it with install.sh --archive into a prefix under target/ (HOME and the
#           login shell under target/ too), installs it again over itself, and runs the installed
#           binary's --version and doctor --json against compose.
#
# H16 (#2246) adds one leg against the compose server, in its own HOME under target/:
# config   MCP servers, agents, skills and models configured through the CLI, then used. The smoke
#          MCP server (examples/mcp_smoke_server, built by `make build`) runs as a stdio server, a
#          Streamable HTTP server behind a bearer token (from an env var, never printed) and a
#          legacy HTTP+SSE server: `mcp add` each (and its test must list the tools); `mcp import`
#          a Claude Code `.mcp.json`; `mcp list --health` shows all four ready; `agents new smoke-helper
#          --from blank` and `skills new smoke-skill` (preloaded with `agents skills add`); `doctor`
#          must show each server ready with its auth and roles, the agent and skill as the person's
#          own, the agent on worker-backend's model through the fallback, and, with the bearer
#          variable unset, `mcp:web` as a warning naming it; the chat's `/model roles` must say the
#          agent falls back; `models set smoke-helper wf2/wf2-1` then moves it to its own model.
#          Last, on a throwaway project with an approved plan, the manager (faux) calls the
#          imported server's tool and dispatches smoke-helper, whose worker (faux `wf2`) calls the
#          stdio, http and sse servers' tools and completes its ticket.
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
LEGS="${AMP_SMOKE_LEGS:-compose config firstrun install cluster}"
# config leg (#2246): the MCP test server, built with the binary (an example of the amp-harness crate).
MCP_SERVER="${AMP_SMOKE_MCP_SERVER:-$HARNESS_DIR/target/debug/examples/mcp_smoke_server}"
COMPOSE_EMAIL="${AMP_SMOKE_COMPOSE_EMAIL:-local-dev@amp.local}"
# Compose serves the web UI on its own port (amp-ui, :5173), not on the MCP origin.
COMPOSE_WEB_URL="${AMP_SMOKE_WEB_URL:-http://localhost:5173}"
# Compose's REST listener: auth off, so a call here is the person in the web UI (plan approval,
# marking a person's ticket done). Never used for the cluster leg.
REST_URL="${AMP_SMOKE_REST_URL:-http://localhost:3001}"
# Cluster leg: how long to wait for a person to approve the plan when the server refuses agents.
APPROVE_WAIT="${AMP_SMOKE_APPROVE_WAIT:-0}"
# install leg: a release archive to install instead of packing the binary under test.
INSTALL_ARCHIVE="${AMP_SMOKE_ARCHIVE:-}"

# Keep the cluster credentials aside and out of the environment, so no other leg ever sends them.
CLUSTER_TOKEN="${AMP_TOKEN:-}"
CLUSTER_CA="${AMP_CA_FILE:-}"
unset AMP_TOKEN AMP_CA_FILE AMP_MCP_URL AMP_MCP_PROTOCOL AMP_WEB_URL

# fd 3 is the real stdout: the exit trap can run inside a `call ... >/dev/null` (die exits from
# there), and its cleanup and result lines must still be seen.
exec 3>&1
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
# A made-up provider key for the firstrun and install legs: never valid, never sent anywhere (the
# chat only builds its session), and it must not end up in any file or output.
DUMMY_KEY="sk-or-v1-smoke-not-a-key-$$"

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
P_ID="" P_NAME="" EPIC_ID="" STORY_ID="" TASK_IDS=() LEG_WEB_BASE="" LEG_REST=""
RESULTS=()
# Projects the firstrun leg created through the harness itself; archived on exit.
EXTRA_PROJECTS=()
# MCP servers the config leg started in the background; stopped on exit.
MCP_PIDS=()

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
    # An open person's ticket can only be deleted by the person (REST on compose), not over MCP.
    h mcp call amp_delete_task "{\"task_id\":$id}" >/dev/null 2>&1 ||
      { [[ -n "$LEG_REST" ]] && curl -fsS -m 10 -X DELETE "$LEG_REST/api/tasks/$id" >/dev/null 2>&1; } ||
      { failed=1; echo "smoke: cleanup: could not delete ticket $id" >&2; }
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
  exec 1>&3
  cleanup_leg || rc=1
  local p
  for p in "${EXTRA_PROJECTS[@]+"${EXTRA_PROJECTS[@]}"}"; do
    if AMP_MCP_URL="$COMPOSE_URL" "$BIN" mcp call amp_archive_project "{\"project_id\":$p}" >/dev/null 2>&1; then
      log "cleanup ok: first-run project $p archived"
    else
      rc=1
      echo "smoke: cleanup: could not archive first-run project $p" >&2
    fi
  done
  local pid
  for pid in "${MCP_PIDS[@]+"${MCP_PIDS[@]}"}"; do
    kill "$pid" 2>/dev/null || true
  done
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

# --- plan approval and a person's ticket (M4-5, smoke v4) ----------------------------------------
# approve_plan: the person approves the leg's plan. Compose: POST approve-plan on the REST listener
# (the web UI's route) with as_of = the newest planned_at among the open tickets, sent verbatim
# (#2192; a server without as_of ignores it). Cluster: amp_approve_plan, which only servers from
# before human-only approval accept; otherwise a person must approve at the printed link within
# AMP_SMOKE_APPROVE_WAIT seconds.
approve_plan() {
  local out as_of
  if [[ -n "$LEG_REST" ]]; then
    out="$(curl -fsS -m 10 "$LEG_REST/api/projects/$P_ID/tasks?owner=any")" ||
      die "[$LEG] approve: could not list the project's tickets on $LEG_REST"
    # Newest planned_at, compared on a fixed-width key (Go trims trailing zeros of the fraction).
    as_of="$(jq -r 'def key: sub("Z$"; "") | split(".") | .[0] + "." + (((.[1] // "") + "000000")[0:6]);
        [.tasks[]? | select(.state != "completed") | .planned_at // empty] | max_by(key) // empty' <<<"$out")"
    [[ -n "$as_of" ]] || die "[$LEG] approve: no planned_at on the open tickets"
    out="$(curl -sS -m 10 -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' \
      -d "$(jq -nc --arg a "$as_of" '{as_of: $a}')" "$LEG_REST/api/projects/$P_ID/approve-plan")" ||
      die "[$LEG] approve: POST approve-plan failed"
    [[ "$(tail -n 1 <<<"$out")" == 200 ]] || {
      printf '%s\n' "$out" >&2
      die "[$LEG] approve: POST approve-plan did not answer 200"
    }
    out="$(sed '$d' <<<"$out")"
    expect "$out" '.project.plan_approved_at != null and ((.awaiting_approval // []) == [])' \
      "approve-plan left tickets awaiting approval"
    log "[$LEG] plan approved as the person (REST approve-plan, as_of $as_of)"
    return 0
  fi
  if h mcp call amp_approve_plan "{\"project_id\":$P_ID}" >/dev/null 2>"$WORK/err"; then
    log "[$LEG] plan approved over MCP (a server from before human-only approval)"
    return 0
  fi
  local url waited=0
  # The approval page: /project/P/plan on older servers, My work (/project/P/my-work?approve=...) since
  # human-work UX (#2498). The harness may redact the query, so stop at the path.
  url="$(grep -Eo '(https?://[^ "]+)?/project/[0-9]+/(plan|my-work)' "$WORK/err" | head -n 1 || true)"
  [[ "$url" == /* ]] && url="$LEG_WEB_BASE$url"
  ((APPROVE_WAIT > 0)) ||
    die "[$LEG] plan approval needs a person: approve ${url:-the plan of project $P_ID} in the web UI, or set AMP_SMOKE_APPROVE_WAIT=SECS and approve while the smoke waits"
  log "[$LEG] waiting up to ${APPROVE_WAIT}s for a person to approve ${url:-project $P_ID} in the web UI"
  until out="$(call amp_check_plan "{\"project_id\":$P_ID}")" && jq -e '(.awaiting_approval // []) == []' >/dev/null <<<"$out"; do
    ((waited < APPROVE_WAIT)) || die "[$LEG] the plan was not approved within ${APPROVE_WAIT}s"
    sleep 5
    waited=$((waited + 5))
  done
  log "[$LEG] plan approved by a person"
}

# human_cases: a person's ticket can't be marked done over MCP (the refusal names the web UI); the
# person marks it done through REST. Compose only (needs the REST listener as the UI).
human_cases() {
  local th out rc=0
  out="$(call amp_create_tasks "$(jq -nc --argjson p "$P_ID" --argjson s "$STORY_ID" '{project_id:$p, tasks:[
      {ref:"person", story_id:$s, name:"smoke person ticket", description:"Throwaway; a person signs it off.", acceptance_criteria:"None.", performer:"human"}]}')")"
  th="$(jq -r '.created[]? | select(.ref == "person") | .id' <<<"$out")"
  [[ "$th" =~ ^[0-9]+$ ]] || die "[$LEG] amp_create_tasks: no id for the person's ticket"
  TASK_IDS+=("$th")
  h mcp call amp_complete_task "{\"task_id\":$th}" >/dev/null 2>"$WORK/err" || rc=$?
  ((rc == 1)) || die "[$LEG] person's ticket: amp_complete_task over MCP was not refused (exit $rc)"
  grep -q "web UI" "$WORK/err" || {
    cat "$WORK/err" >&2
    die "[$LEG] person's ticket: the MCP refusal does not point to the web UI"
  }
  out="$(curl -fsS -m 10 -X POST -H 'Content-Type: application/json' -d '{"note":"signed off by the smoke test"}' \
    "$LEG_REST/api/tasks/$th/complete")" || die "[$LEG] person's ticket: REST complete failed"
  out="$(runs_of "$th")"
  expect "$out" '.task.state == "completed"' "person's ticket: not completed after the REST call"
  log "[$LEG] person's ticket ok: #$th refused over MCP, marked done through REST"
}

# --- dispatch, abandoned run, policy deny (M2-8) -----------------------------------------------
# runs_of TASK: the ticket with its comments and runs.
runs_of() { call amp_get_task "{\"task_id\":$1,\"include\":[\"comments\",\"runs\"]}"; }

# dispatch_cases TICKET THIRD: needs .amp.json for the leg's project and an approved plan. TICKET
# must be ready; THIRD depends on it.
dispatch_cases() {
  local t2="$1" t3="$2" out report

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
  #    closing its ticket; the harness reports an abandoned run with a note and, since #2802, hands
  #    the ticket back to the backlog (ready to dispatch again). The server may also record
  #    returned_at/returned_reason on the ticket (#2909): accepted, not required.
  #    Ticket three was created and approved with the plan; ticket two's completion made it ready.
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
  jq -e '(.task.state == "backlog") and
      ((.task.returned_reason // "") | type == "string") and
      ([.runs[]? | select(.outcome == "abandoned" and .model == "wf/wf-1" and .agent_id == "amp:amp-worker-backend"
        and (.note | startswith("abandoned: error:") and (startswith("abandoned: error: error") | not))
        and ((.input_tokens // 0) + (.output_tokens // 0)) > 0)] | length) == 1' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" | head -c 4000 >&2
    die "[$LEG] abandoned: expected #$t3 returned to backlog with one abandoned run carrying a note, the worker model and tokens"
  }
  log "[$LEG] abandoned ok: #$t3 back in backlog with an abandoned run ($(jq -r '[.runs[] | select(.outcome == "abandoned")][0].note' <<<"$out" | cut -c1-80))"
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

  # 1. render-chrome --live: project, counts (a question; the abandoned worker's ticket was returned to the backlog).
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
#         REST_URL ('' = none: the plan is approved over MCP or by a person, no person's-ticket case)
run_leg() {
  LEG="$1/$3" LEG_URL="$2" LEG_PROTO="$3" LEG_TOKEN="$4" LEG_CA="$5" LEG_WEB="$7" LEG_REST="$8"
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
      {ref:"second", story_id:$s, name:"smoke ticket two", description:"Throwaway.", acceptance_criteria:"None.", assigned_to:"amp:amp-worker-backend", depends_on:["first"]},
      {ref:"third",  story_id:$s, name:"smoke ticket three", description:"Throwaway; its worker gives up.", acceptance_criteria:"None.", assigned_to:"amp:amp-worker-backend", depends_on:["second"]}]}')")"
  local t1 t2 t3
  # Record every created id first, so cleanup deletes them even if a check below fails.
  read -r -a TASK_IDS <<<"$(jq -r '[.created[]?.id] | map(tostring) | join(" ")' <<<"$out")"
  t1="$(jq -r '.created[]? | select(.ref == "first") | .id' <<<"$out")"
  t2="$(jq -r '.created[]? | select(.ref == "second") | .id' <<<"$out")"
  t3="$(jq -r '.created[]? | select(.ref == "third") | .id' <<<"$out")"
  [[ "$t1" =~ ^[0-9]+$ && "$t2" =~ ^[0-9]+$ && "$t3" =~ ^[0-9]+$ ]] || {
    printf '%s\n' "$out" >&2
    die "[$LEG] amp_create_tasks: could not read the ticket ids"
  }
  call amp_add_task_comment "{\"task_id\":$t1,\"body\":\"smoke comment $stamp\",\"author\":\"smoke\"}" >/dev/null
  log "[$LEG] created project $P_ID ($code), epic $EPIC_ID, story $STORY_ID, tickets $t1 $t2 $t3"

  # Human-only approval (#2167): agents can't start work on an unapproved plan.
  rc=0
  h mcp call amp_dispatch_task "{\"task_id\":$t1,\"agent_id\":\"amp:amp-worker-backend\"}" >/dev/null 2>"$WORK/err" || rc=$?
  if ((rc == 0)); then
    log "[$LEG] note: the server dispatched before the plan was approved (approval not required here)"
    call amp_set_task_state "{\"task_id\":$t1,\"to_state\":\"backlog\"}" >/dev/null
  else
    grep -Eq "/project/$P_ID/(plan|my-work)" "$WORK/err" || {
      cat "$WORK/err" >&2
      die "[$LEG] dispatch before approval: the refusal has no link to the plan"
    }
  fi
  approve_plan

  # tickets list: the open tickets, each waiting on the one before.
  out="$(h tickets list --project "$P_ID" --owner any --json)" || die "[$LEG] tickets list failed"
  jq -e --argjson p "$P_ID" --argjson a "$t1" --argjson b "$t2" --argjson c "$t3" '
      .project_id == $p and .count == 3 and
      ([.tickets[].id] | sort) == ([$a, $b, $c] | sort) and
      (.tickets[] | select(.id == $a) | .status == "ready" and .name == "smoke ticket one") and
      (.tickets[] | select(.id == $b) | .status == "waiting" and .blocked_by_ids == [$a]) and
      (.tickets[] | select(.id == $c) | .status == "waiting" and .blocked_by_ids == [$b])' >/dev/null <<<"$out" || {
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

  # Finish ticket one as an agent does: dispatch, then complete (an undispatched ticket is refused).
  # The default list hides done tickets, --state all shows them, and the project comes from
  # .amp.json when --project is left out.
  rc=0
  h mcp call amp_complete_task "{\"task_id\":$t1}" >/dev/null 2>"$WORK/err" || rc=$?
  if ((rc == 0)); then
    log "[$LEG] note: the server completed an undispatched ticket (a server from before #2167)"
  else
    grep -q "never dispatched" "$WORK/err" || {
      cat "$WORK/err" >&2
      die "[$LEG] complete before dispatch: expected a 'never dispatched' refusal"
    }
    call amp_dispatch_task "{\"task_id\":$t1,\"agent_id\":\"amp:amp-worker-backend\"}" >/dev/null
    call amp_complete_task "{\"task_id\":$t1}" >/dev/null
  fi
  printf '{"project_id": %s, "project_name": "harness smoke"}\n' "$P_ID" >.amp.json
  out="$(h tickets list --owner any --json)" || die "[$LEG] tickets list (from .amp.json) failed"
  jq -e --argjson p "$P_ID" --argjson b "$t2" --argjson c "$t3" '
      .project_id == $p and .count == 2 and
      (.tickets[] | select(.id == $b) | .status == "ready") and
      (.tickets[] | select(.id == $c) | .status == "waiting")' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] tickets list after completing ticket one: expected ticket two ready and three waiting"
  }
  out="$(h tickets list --owner any --state all --json)" || die "[$LEG] tickets list --state all failed"
  jq -e --argjson a "$t1" '.count == 3 and (.tickets[] | select(.id == $a) | .status == "done")' >/dev/null <<<"$out" || {
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

  dispatch_cases "$t2" "$t3"
  if [[ -n "$LEG_REST" ]]; then human_cases; fi
  chrome_cases "$stamp"

  # A tool error exits 1 with the message on stderr.
  rc=0
  h mcp call amp_get_task '{"task_id": 999999999}' >/dev/null 2>"$WORK/err" || rc=$?
  ((rc == 1)) || die "[$LEG] mcp call of a missing ticket: expected exit 1, got $rc"
  [[ -s "$WORK/err" ]] || die "[$LEG] mcp call of a missing ticket printed no error"

  rm -f .amp.json
  cleanup_leg || die "[$LEG] cleanup failed"
  RESULTS+=("PASS $LEG ($proto_seen)")
  LEG_REST=""
}

# --- first run and init (M4-5, smoke v4) -----------------------------------------------------------
# fresh NAME: a new, empty HOME for a first-time user, under target/ (never the real one).
fresh_env() {
  local root="$WORK/$1"
  mkdir -p "$root/home"
  FRESH=(env -u AMP_MCP_URL -u AMP_MCP_PROTOCOL -u AMP_WEB_URL -u AMP_HARNESS_PROMPTS
    HOME="$root/home" XDG_CONFIG_HOME="$root/home/.config" XDG_STATE_HOME="$root/home/.local/state"
    AMP_CREDENTIAL_STORE=file AMP_HARNESS_CEILING_DIR="$WORK" OPENROUTER_API_KEY="$DUMMY_KEY")
}

# no_secret DIR FILES...: the dummy provider key is in none of these files nor anywhere under DIR.
no_secret() {
  local dir="$1"
  shift
  if grep -rqF "$DUMMY_KEY" "$dir" "$@" 2>/dev/null; then
    die "[$LEG] the provider key was written to disk or printed ($(grep -rlF "$DUMMY_KEY" "$dir" "$@" | head -n 3 | tr '\n' ' '))"
  fi
}

firstrun_leg() {
  LEG="firstrun"
  log "== leg firstrun: a new HOME, no config, against $COMPOSE_URL"
  local root="$WORK/fr1" repo out err cfg pid name stamp server code
  stamp="$(date +%Y%m%d-%H%M%S)"
  # Project codes are unique across AMP, archived projects included.
  code="SF$(LC_ALL=C tr -dc '[:upper:]' </dev/urandom | head -c 4 || true)"
  name="harness-smoke first run $stamp"
  fresh_env fr1
  repo="$root/repo"
  mkdir -p "$repo/sub"
  git -C "$repo" init -q -b main . || die "[$LEG] could not make the first-run git repo"
  # Answer the server the way a person types it: host:port for a local http://host:port/mcp.
  server="$COMPOSE_URL"
  if [[ "$COMPOSE_URL" =~ ^http://(localhost|127\.0\.0\.1)(:[0-9]+)?/mcp$ ]]; then
    server="${BASH_REMATCH[1]}${BASH_REMATCH[2]}"
  fi

  # 1. The chat's first run, from a subfolder: server, then a new project, name, code. With active
  # projects the first run shows a picker (answer n, a new one); with none (a fresh server, e.g. CI's
  # compose stack) it asks "Create a project for this folder now? [Y/n]" (answer y).
  local newp=n listed
  listed="$(AMP_MCP_URL="$COMPOSE_URL" "$BIN" mcp call amp_list_projects '{}' 2>/dev/null)" ||
    die "[$LEG] amp_list_projects failed"
  if jq -e '[(.projects // .)[]? | select((.state // "active") == "active")] | length == 0' >/dev/null <<<"$listed"; then
    newp=y
  fi
  out="$(cd "$repo/sub" && printf '%s\n%s\n%s\n%s\n' "$server" "$newp" "$name" "$code" |
    "${FRESH[@]}" AMP_HARNESS_PROMPTS=stdin "$BIN" --snapshot 120x30 --no-session 2>"$root/err")" || {
    cat "$root/err" >&2
    die "[$LEG] the chat's first run failed"
  }
  err="$(cat "$root/err")"
  pid="$(jq -r '.project_id // empty' "$repo/.amp.json" 2>/dev/null || true)"
  [[ "$pid" =~ ^[0-9]+$ ]] || {
    printf '%s\n' "$err" >&2
    die "[$LEG] no .amp.json with a project_id at the git root ($repo)"
  }
  EXTRA_PROJECTS+=("$pid")
  [[ ! -e "$repo/sub/.amp.json" ]] || die "[$LEG] .amp.json was written in the subfolder, not at the git root"
  for want in "AMP server [" "Saved the server $COMPOSE_URL" "as $COMPOSE_EMAIL" "Created #$pid $name ($code)"; do
    grep -qF "$want" <<<"$err" || {
      printf '%s\n' "$err" >&2
      die "[$LEG] first run: expected '$want'"
    }
  done
  # The editor's set-up prompt: a person with no projects before (newp=y) gets the onboarding one.
  local setup_prompt="Set up this new project"
  [[ "$newp" == n ]] || setup_prompt="I'm new to AMP: show me around"
  { grep -qF "project #$pid $name" <<<"$out" && grep -qF "$setup_prompt" <<<"$out"; } || {
    printf '%s\n' "$out" >&2
    die "[$LEG] first run: the chat does not show project #$pid with the set-up prompt in the editor"
  }
  cfg="$root/home/.config/amp-harness/config.toml"
  grep -qF "url = \"$COMPOSE_URL\"" "$cfg" || die "[$LEG] config.toml has no url = \"$COMPOSE_URL\""
  if grep -qF "web_url = \"$COMPOSE_WEB_URL\"" "$cfg"; then
    log "[$LEG] first run saved web_url $COMPOSE_WEB_URL (probed)"
  else
    log "[$LEG] WARN: first run saved no web_url $COMPOSE_WEB_URL (is the UI up?)"
  fi
  jq -e --argjson p "$pid" '.project_id == $p and (.amp_api | type == "string")' "$repo/.amp.json" >/dev/null ||
    die "[$LEG] .amp.json is not as expected: $(cat "$repo/.amp.json")"
  no_secret "$root/home" "$repo/.amp.json" <(printf '%s\n%s\n' "$out" "$err")
  log "[$LEG] chat first run ok: saved the server, created #$pid ($name), .amp.json at the git root, editor prefilled"

  # 2. Second start: everything is set up, nothing is asked, no set-up prompt.
  out="$(cd "$repo" && "${FRESH[@]}" AMP_HARNESS_PROMPTS=stdin "$BIN" --snapshot 120x30 --no-session </dev/null 2>"$root/err")" || {
    cat "$root/err" >&2
    die "[$LEG] second start failed"
  }
  [[ ! -s "$root/err" ]] || {
    cat "$root/err" >&2
    die "[$LEG] second start asked or said something on stderr"
  }
  { grep -qF "project #$pid" <<<"$out" && ! grep -qF "$setup_prompt" <<<"$out"; } ||
    die "[$LEG] second start: expected project #$pid and no set-up prompt"
  log "[$LEG] second start ok: nothing asked"

  # 3. The non-interactive path: init --yes with flags in another new HOME and folder, then doctor
  #    there with no environment (config.toml and .amp.json only).
  fresh_env fr2
  root="$WORK/fr2"
  mkdir -p "$root/repo"
  out="$(cd "$root/repo" && "${FRESH[@]}" "$BIN" init --yes --url "$COMPOSE_URL" --project "$pid" --json </dev/null 2>"$root/err")" || {
    cat "$root/err" >&2
    die "[$LEG] init --yes failed"
  }
  jq -e --arg u "$COMPOSE_URL" --arg e "$COMPOSE_EMAIL" --argjson p "$pid" '
      .server == $u and .user == $e and .auth == "none" and .project.id == $p and .project.created == false
      and (.providers.openrouter | test("env"; "i"))' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] init --yes --json: unexpected summary"
  }
  jq -e --argjson p "$pid" '.project_id == $p' "$root/repo/.amp.json" >/dev/null || die "[$LEG] init wrote no .amp.json for #$pid"
  out="$(cd "$root/repo" && "${FRESH[@]}" "$BIN" doctor --json </dev/null)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] doctor after init failed"
  }
  jq -e --argjson p "$pid" '.ok and .connection.connected and .tools.missing_contract == []
      and .project.found and .project.id == $p' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" | head -c 4000 >&2
    die "[$LEG] doctor after init: expected ok, connected, project #$pid"
  }
  no_secret "$root/home" "$root/repo/.amp.json"
  log "[$LEG] init --yes ok: linked another folder to #$pid; doctor ok from config.toml and .amp.json"
  RESULTS+=("PASS firstrun (chat first run, second start, init --yes, doctor)")
}

# --- install from the local archive (M4-5, smoke v4) ------------------------------------------------
install_leg() {
  LEG="install"
  local root="$WORK/install" archive out prefix ver
  mkdir -p "$root/home" "$root/dist"
  prefix="$root/bin"
  if [[ -n "$INSTALL_ARCHIVE" ]]; then
    archive="$INSTALL_ARCHIVE"
    [[ -r "$archive" && -r "$archive.sha256" ]] || die "[$LEG] AMP_SMOKE_ARCHIVE $archive (or its .sha256) is not readable"
  else
    # The target triple from uname, so packing never runs rustc (rustup could fetch a toolchain).
    local target
    case "$(uname -s)/$(uname -m)" in
      Darwin/arm64) target=aarch64-apple-darwin ;;
      Darwin/x86_64) target=x86_64-apple-darwin ;;
      Linux/x86_64) target=x86_64-unknown-linux-gnu ;;
      Linux/aarch64 | Linux/arm64) target=aarch64-unknown-linux-gnu ;;
      *) die "[$LEG] no release target for $(uname -s)/$(uname -m)" ;;
    esac
    archive="$("$HARNESS_DIR/scripts/package.sh" --bin "$BIN" --target "$target" --out "$root/dist" | tail -n 1)" ||
      die "[$LEG] package.sh failed"
  fi
  log "== leg install: $(basename "$archive") into $prefix"
  # i ARGS...: install.sh with HOME, TMPDIR and the login shell under target/.
  i() {
    env HOME="$root/home" TMPDIR="$root" SHELL=/bin/sh "$HARNESS_DIR/install.sh" --archive "$archive" --prefix "$prefix" </dev/null
  }
  out="$(i 2>&1)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] install.sh --archive failed"
  }
  [[ -x "$prefix/amp-harness" ]] || die "[$LEG] no executable $prefix/amp-harness"
  grep -qF "$prefix" <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] install.sh did not tell how to put $prefix on PATH"
  }
  out="$(i 2>&1)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] reinstalling over the installed binary failed"
  }
  ver="$("$prefix/amp-harness" --version)" || die "[$LEG] the installed binary's --version failed"
  if [[ -z "$INSTALL_ARCHIVE" && "$ver" != "$("$BIN" --version)" ]]; then
    die "[$LEG] the installed --version differs from the binary under test"
  fi
  # The installed binary's doctor against compose, as a new user with only an env key.
  out="$(cd "$root" && env -u AMP_HARNESS_PROMPTS HOME="$root/home" XDG_CONFIG_HOME="$root/home/.config" \
    XDG_STATE_HOME="$root/home/.local/state" AMP_CREDENTIAL_STORE=file AMP_HARNESS_CEILING_DIR="$root" \
    AMP_MCP_URL="$COMPOSE_URL" OPENROUTER_API_KEY="$DUMMY_KEY" "$prefix/amp-harness" doctor --json </dev/null)" || {
    printf '%s\n' "$out" | head -c 4000 >&2
    die "[$LEG] the installed binary's doctor failed"
  }
  jq -e '.ok and .connection.connected and .tools.missing_contract == [] and .connection.instructions == true' \
    >/dev/null <<<"$out" || {
    printf '%s\n' "$out" | head -c 4000 >&2
    die "[$LEG] the installed binary's doctor is not ok"
  }
  log "[$LEG] install ok: $(head -n 1 <<<"$ver") installed twice into the prefix; doctor ok against $COMPOSE_URL"
  RESULTS+=("PASS install ($(basename "$archive"))")
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

  # #2163: connect --list reports the key's source (never the key); disconnect removes the item.
  rm -f "$trace"
  KARGS=(connect --list --json)
  out="$(k)" || die "[keychain] connect --list failed"
  expect "$out" 'map(select(.id == "openrouter"))[0].key == "keychain"' \
    "connect --list does not show the OpenRouter key in the keychain"
  grep -q "sk-or-v1-smoke-dummy" <<<"$out" && die "[keychain] connect --list printed the key"
  KARGS=(disconnect openrouter --json)
  out="$(k)" || die "[keychain] disconnect failed"
  expect "$out" '.key_removed == true' "disconnect did not remove the OpenRouter key"
  log "[keychain] connect --list and disconnect ok"
  RESULTS+=("PASS keychain")
}

# --- MCP servers, agents, skills, models (H16, #2246) ------------------------------------------------
# start_server NAME ARGS...: starts the smoke MCP server in the background (stdin a FIFO the script
# holds open, so the server ends with the script) and sets SERVER_URL to the URL it prints.
start_server() {
  local name="$1" fifo="$CFG_ROOT/$1.in" out="$CFG_ROOT/$1.url" i
  shift
  mkfifo "$fifo"
  env "${SERVER_ENV[@]+"${SERVER_ENV[@]}"}" "$MCP_SERVER" "$@" <"$fifo" >"$out" 2>"$CFG_ROOT/$name.err" &
  MCP_PIDS+=("$!")
  # Hold the write end open (fd 5 for http, 6 for sse); EOF on it stops the server.
  case "$name" in
    http) exec 5>"$fifo" ;;
    sse) exec 6>"$fifo" ;;
  esac
  for i in $(seq 1 50); do
    SERVER_URL="$(head -n 1 "$out" 2>/dev/null || true)"
    [[ "$SERVER_URL" == http://* ]] && return 0
    sleep 0.2
  done
  cat "$CFG_ROOT/$name.err" >&2
  die "[$LEG] the $name smoke MCP server printed no URL"
}

config_leg() {
  LEG="config" LEG_URL="$COMPOSE_URL" LEG_PROTO="auto" LEG_TOKEN="" LEG_CA="" LEG_WEB="$COMPOSE_WEB_URL"
  LEG_REST="$REST_URL" LEG_WEB_BASE="$COMPOSE_WEB_URL"
  [[ -x "$MCP_SERVER" ]] || die "[$LEG] $MCP_SERVER not found; run make -C v2/harness build (it builds the examples)"
  CFG_ROOT="$WORK/cfg"
  local home="$CFG_ROOT/home" project="$CFG_ROOT/project" out rc token
  mkdir -p "$home/.config/amp-harness" "$project"
  log "== leg config: MCP servers, agents, skills and models against $COMPOSE_URL"
  # A second faux worker model, for `models set`.
  cat >"$home/.config/amp-harness/config.toml" <<'TOML'
[providers.boss]
api = "faux"
[providers.boss.models."boss-1"]
[providers.wf]
api = "faux"
[providers.wf.models."wf-1"]
[providers.wf2]
api = "faux"
[providers.wf2.models."wf2-1"]
[roles.manager]
model = "boss/boss-1"
[roles.worker-backend]
model = "wf/wf-1"
TOML
  git -C "$project" init -q -b main . && printf 'config smoke repo\n' >"$project/README.md" &&
    git -C "$project" add README.md && git -C "$project" commit -q -m init ||
    die "[$LEG] could not make the config leg's git repo"
  # A bearer token for the http server: random, only ever in the environment.
  token="smoke-mcp-$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 24 || true)"
  # c ARGS...: the harness in this leg's HOME and project, against compose.
  c() {
    (cd "$project" && env -u AMP_HARNESS_PROMPTS HOME="$home" XDG_CONFIG_HOME="$home/.config" \
      XDG_STATE_HOME="$home/.local/state" AMP_CREDENTIAL_STORE=file AMP_HARNESS_CEILING_DIR="$CFG_ROOT" \
      AMP_MCP_URL="$COMPOSE_URL" AMP_WEB_URL="$COMPOSE_WEB_URL" SMOKE_MCP_TOKEN="$token" \
      "$BIN" "$@" </dev/null)
  }
  # no_token TEXT...: the bearer token is in none of the arguments' text nor in a file under HOME.
  no_token() {
    if printf '%s\n' "$@" | grep -qF "$token" || grep -rqF "$token" "$home" "$project" 2>/dev/null; then
      die "[$LEG] the MCP bearer token was printed or written to a file"
    fi
  }

  # 1. stdio, http (bearer from env) and legacy sse servers, each added and tested.
  out="$(c mcp add fs --json -- "$MCP_SERVER" stdio fs)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] mcp add fs (stdio) failed"
  }
  expect "$out" '.type == "stdio" and .test.ok and .test.tools == ["echo", "has_env"]' "mcp add fs: test not ok"
  SERVER_ENV=(SMOKE_MCP_TOKEN="$token")
  start_server http http web --bearer-env SMOKE_MCP_TOKEN
  SERVER_ENV=()
  local http_url="$SERVER_URL"
  out="$(c mcp add web "$http_url" --bearer-env SMOKE_MCP_TOKEN --json)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] mcp add web (http) failed"
  }
  expect "$out" '.type == "http" and .test.ok and .test.tools == ["echo", "has_env"]' "mcp add web: test not ok"
  start_server sse sse
  local sse_url="$SERVER_URL"
  out="$(c mcp add old "$sse_url" --transport sse --json)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] mcp add old (sse) failed"
  }
  expect "$out" '.type == "sse" and .test.ok and .test.tools == ["echo"] and .test.protocol == "2024-11-05"' \
    "mcp add old: test not ok (legacy SSE)"
  log "[$LEG] mcp add ok: stdio fs, http web (bearer from env), sse old ($sse_url)"

  # 2. import a Claude Code .mcp.json (project scope file -> the user config).
  jq -n --arg cmd "$MCP_SERVER" '{mcpServers: {imported: {command: $cmd, args: ["stdio", "imported"]}}}' \
    >"$project/.mcp.json"
  out="$(c mcp import --from claude --yes --json)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] mcp import --from claude failed"
  }
  expect "$out" '.imported == 1 and ([.servers[] | select(.name == "imported") | .action] == ["add"])' \
    "mcp import: expected one server added"
  out="$(c mcp list --health --json)" || die "[$LEG] mcp list failed"
  expect "$out" '[.[] | select(.status.state == "ready") | "\(.name)=\(.tools)"] | sort == ["fs=2", "imported=2", "old=1", "web=2"]' \
    "mcp list: expected fs, imported, old and web ready"
  log "[$LEG] mcp import ok: imported from .mcp.json; mcp list: 4 ready"

  # 3. an agent and a skill of the person's own; the agent falls back to worker-backend's model.
  out="$(c agents new smoke-helper --from blank --description "Smoke test helper: runs MCP tools." --json)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] agents new failed"
  }
  expect "$out" '.id == "smoke-helper" and .origin == "user" and .dispatchable and .model == "wf/wf-1" and (.model_source | startswith("fallback worker-backend"))' \
    "agents new: expected a dispatchable user agent on worker-backend's model (fallback)"
  c skills new smoke-skill --description "Smoke test skill." >/dev/null || die "[$LEG] skills new failed"
  c agents skills smoke-helper add smoke-skill >/dev/null || die "[$LEG] agents skills add failed"
  out="$(c skills list --json)" || die "[$LEG] skills list failed"
  expect "$out" '[.skills[] | select(.name == "smoke-skill" and .origin == "user")] | length == 1' "skills list: no smoke-skill"
  out="$(c agents show smoke-helper --json)" || die "[$LEG] agents show failed"
  expect "$out" '.skills == ["smoke-skill"]' "agents show: smoke-skill is not preloaded"
  log "[$LEG] agents/skills ok: smoke-helper (fallback worker-backend), smoke-skill preloaded"

  # 4. doctor: each server ready with auth and roles, the agent and skill, the fallback model.
  rc=0
  out="$(c doctor --json)" || rc=$?
  no_token "$out"
  # Doctor's own checks of this leg: mcp, agents, skills, fallback (the key check for openrouter
  # warns: the leg has no key; nothing may fail).
  expect "$out" '.ok and ([.checks[] | select(.status == "fail")] == [])' "doctor not ok (exit $rc)"
  expect "$out" '[.checks[] | select(.name | startswith("mcp:")) | "\(.name)=\(.status)"] | sort == ["mcp:fs=ok", "mcp:imported=ok", "mcp:old=ok", "mcp:web=ok"]' \
    "doctor: expected the four MCP servers ok"
  expect "$out" '(.checks[] | select(.name == "mcp:web") | .detail | contains("bearer token: env SMOKE_MCP_TOKEN (set)"))' \
    "doctor: mcp:web does not name its bearer variable"
  expect "$out" '(.checks[] | select(.name == "mcp:old") | .detail | contains("sse (user,") and contains("MCP 2024-11-05"))' \
    "doctor: mcp:old is not the legacy SSE server"
  expect "$out" '[.mcp.servers[] | select(.name == "fs") | .roles[] | select(.role == "smoke-helper") | .access] == ["all"]' \
    "doctor: smoke-helper should see every fs tool"
  expect "$out" '(.checks[] | select(.name == "agents") | .detail | contains("smoke-helper (user, wf/wf-1 via fallback worker-backend, dispatchable)"))' \
    "doctor: agents does not list smoke-helper with its fallback"
  expect "$out" '(.checks[] | select(.name == "skills") | .detail | contains("smoke-skill (user)"))' "doctor: skills does not list smoke-skill"
  expect "$out" '[.models.roles[] | select(.role == "smoke-helper") | "\(.provider)/\(.model) \(.fallback)"] == ["wf/wf-1 fallback worker-backend"]' \
    "doctor: models.roles has no smoke-helper on the fallback"
  expect "$out" '.models.fallback.setting == "worker-backend" and .models.fallback.model == "wf-1"' "doctor: no fallback model"
  log "[$LEG] doctor ok: $(jq -r '.checks[] | select(.name == "mcp") | .detail' <<<"$out"); fallback $(jq -r '.checks[] | select(.name == "fallback") | .detail' <<<"$out" | cut -c1-90)"
  # Without the bearer variable: a warning that names it (never its value).
  out="$(cd "$project" && env -u AMP_HARNESS_PROMPTS HOME="$home" XDG_CONFIG_HOME="$home/.config" \
    XDG_STATE_HOME="$home/.local/state" AMP_CREDENTIAL_STORE=file AMP_HARNESS_CEILING_DIR="$CFG_ROOT" \
    AMP_MCP_URL="$COMPOSE_URL" "$BIN" doctor --json </dev/null)" || true
  expect "$out" '(.checks[] | select(.name == "mcp:web") | .status == "warn" and (.detail | contains("SMOKE_MCP_TOKEN is not set")) and (.fix | length > 0))' \
    "doctor without the bearer variable: mcp:web should warn naming SMOKE_MCP_TOKEN"
  log "[$LEG] doctor ok without the bearer variable: mcp:web warns, naming SMOKE_MCP_TOKEN"

  # 5. the project, its tickets and an approved plan, for the chat and the dispatch.
  local code stamp t1
  code="SC$(LC_ALL=C tr -dc '[:upper:]' </dev/urandom | head -c 4 || true)"
  stamp="$(date +%Y%m%d-%H%M%S)"
  P_NAME="harness-smoke config $stamp"
  out="$(call amp_create_project "{\"name\":\"$P_NAME\",\"code\":\"$code\",\"description\":\"Throwaway project from v2/harness/scripts/smoke.sh (config leg); deleted by the run.\"}")"
  P_ID="$(jq -r '.id // .project.id // empty' <<<"$out")"
  [[ "$P_ID" =~ ^[0-9]+$ ]] || die "[$LEG] amp_create_project returned no id"
  out="$(call amp_create_epic "{\"project_id\":$P_ID,\"name\":\"config smoke epic\"}")"
  EPIC_ID="$(jq -r '.id // .epic.id // empty' <<<"$out")"
  out="$(call amp_create_story "{\"project_id\":$P_ID,\"epic_id\":$EPIC_ID,\"name\":\"config smoke story\"}")"
  STORY_ID="$(jq -r '.id // .story.id // empty' <<<"$out")"
  out="$(call amp_create_tasks "$(jq -nc --argjson p "$P_ID" --argjson s "$STORY_ID" '{project_id:$p, tasks:[
      {ref:"mcp", story_id:$s, name:"config smoke: call the MCP servers", description:"Throwaway; the smoke helper calls every MCP server.", acceptance_criteria:"None.", assigned_to:"amp:smoke-helper"}]}')")"
  read -r -a TASK_IDS <<<"$(jq -r '[.created[]?.id] | map(tostring) | join(" ")' <<<"$out")"
  t1="$(jq -r '.created[]? | select(.ref == "mcp") | .id' <<<"$out")"
  [[ "$t1" =~ ^[0-9]+$ ]] || die "[$LEG] amp_create_tasks: no id"
  approve_plan
  printf '{"project_id": %s, "project_name": "harness smoke config"}\n' "$P_ID" >"$project/.amp.json"

  # 6. the chat's /model roles says smoke-helper falls back (and names the fallback model).
  out="$(c --no-session --snapshot 160x50 --input "/model roles")" || {
    printf '%s\n' "$out" | tail -c 4000 >&2
    die "[$LEG] /model roles: the snapshot chat failed"
  }
  grep -E "smoke-helper +wf/wf-1 .*fallback: worker-backend's model" <<<"$out" >/dev/null || {
    printf '%s\n' "$out" >&2
    die "[$LEG] /model roles: smoke-helper does not show its fallback"
  }
  grep -qF "fallback: wf/wf-1 (worker-backend's model, default) for roles with no model of their own" <<<"$out" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] /model roles: no fallback line"
  }
  log "[$LEG] /model roles ok: smoke-helper on wf/wf-1, fallback: worker-backend's model"

  # 7. models set: smoke-helper gets a model of its own.
  out="$(c models set smoke-helper wf2/wf2-1 --json)" || {
    printf '%s\n' "$out" >&2
    die "[$LEG] models set failed"
  }
  expect "$out" '.action == "set" and .changed and .role.provider == "wf2" and .role.model == "wf2-1" and .role.fallback == null' \
    "models set: smoke-helper is not on wf2/wf2-1"
  out="$(c models list --json)" || die "[$LEG] models list failed"
  expect "$out" '[.roles[] | select(.role == "smoke-helper") | "\(.provider)/\(.model)"] == ["wf2/wf2-1"]' \
    "models list: smoke-helper is not on wf2/wf2-1"
  log "[$LEG] models set ok: smoke-helper on wf2/wf2-1 ([roles.smoke-helper])"

  # 8. the manager calls an external tool (the imported server) and dispatches smoke-helper, whose
  #    worker calls the stdio, http and sse servers' tools, then completes its ticket.
  jq -nc --argjson t "$t1" '{
    boss: [
      {tool_calls: [{name: "mcp__imported__echo", arguments: {text: "from the manager"}}]},
      {tool_calls: [{name: "amp_run_ticket", arguments: {task_id: $t, agent_id: "amp:smoke-helper"}}]},
      {text: "Worker report: {{last_tool_result}}"}],
    wf2: [
      {tool_calls: [{name: "mcp__fs__echo", arguments: {text: "worker stdio"}}]},
      {tool_calls: [{name: "mcp__web__echo", arguments: {text: "worker http"}}]},
      {tool_calls: [{name: "mcp__old__echo", arguments: {text: "worker sse"}}]},
      {tool_calls: [{name: "amp_add_task_comment", arguments: {task_id: $t, body: "smoke helper called fs, web and old", author: "amp-worker"}}]},
      {tool_calls: [{name: "amp_complete_task", arguments: {task_id: $t}}]},
      {text: "MCP tools called."}]}' >"$CFG_ROOT/mcp-dispatch.json"
  out="$(cd "$project" && env -u AMP_HARNESS_PROMPTS HOME="$home" XDG_CONFIG_HOME="$home/.config" \
    XDG_STATE_HOME="$home/.local/state" AMP_CREDENTIAL_STORE=file AMP_HARNESS_CEILING_DIR="$CFG_ROOT" \
    AMP_MCP_URL="$COMPOSE_URL" AMP_WEB_URL="$COMPOSE_WEB_URL" SMOKE_MCP_TOKEN="$token" \
    AMP_HARNESS_FAUX_SCRIPT="$CFG_ROOT/mcp-dispatch.json" "$BIN" run -p "Run ticket #$t1" --json --no-session </dev/null)" || {
    printf '%s\n' "$out" | tail -c 4000 >&2
    die "[$LEG] dispatch with MCP tools: run -p failed"
  }
  no_token "$out"
  jq -s -e '
      (.[0] | .type == "session" and ([.mcp_servers[]? | select(.status.state == "ready") | .name] | sort) == ["fs", "imported", "old", "web"]
        and (.tools | index("mcp__imported__echo")) != null) and
      ([.[] | select(.type == "tool_end" and .name == "mcp__imported__echo")][0]
        | .is_error == false and (.text | contains("imported: from the manager"))) and
      (.[-1] | .type == "result" and .outcome == "completed" and .tools_used == ["mcp__imported__echo", "amp_run_ticket"])' \
    >/dev/null <<<"$out" || {
    printf '%s\n' "$out" | tail -c 4000 >&2
    die "[$LEG] the manager did not call mcp__imported__echo, or its session lacks the MCP servers"
  }
  local report
  report="$(jq -s -c '[.[] | select(.type == "tool_end" and .name == "amp_run_ticket")][0]' <<<"$out")"
  jq -e --argjson t "$t1" '.is_error == false and (.text | fromjson
      | .task_id == $t and .outcome == "completed" and .agent_id == "amp:smoke-helper"
        and .model == "wf2/wf2-1" and .tool_calls == 5 and .tool_errors == 0)' >/dev/null <<<"$report" || {
    printf '%s\n' "$report" | head -c 4000 >&2
    die "[$LEG] the smoke-helper worker did not complete on wf2/wf2-1 with 5 tool calls and no errors"
  }
  out="$(runs_of "$t1")"
  jq -e '(.task.state == "completed") and ([.comments[]? | .body] | index("smoke helper called fs, web and old")) != null and
      ([.runs[]? | select(.outcome == "completed" and .model == "wf2/wf2-1")] | length) == 1' >/dev/null <<<"$out" || {
    printf '%s\n' "$out" | head -c 4000 >&2
    die "[$LEG] ticket $t1: expected completed with the worker's comment and a run on wf2/wf2-1"
  }
  log "[$LEG] dispatch ok: the manager called mcp__imported__echo; smoke-helper (wf2/wf2-1) called fs, web and old and completed #$t1"

  rm -f "$project/.amp.json"
  exec 5>&- 6>&-
  cleanup_leg || die "[$LEG] cleanup failed"
  LEG_REST=""
  RESULTS+=("PASS config (mcp stdio/http/sse/import, agents, skills, models, doctor, /model roles, dispatch with MCP tools)")
}

for leg in $LEGS; do
  case "$leg" in
    keychain) keychain_leg ;;
    compose)
      for proto in $PROTOCOLS; do run_leg compose "$COMPOSE_URL" "$proto" "" "" "$COMPOSE_EMAIL" "$COMPOSE_WEB_URL" "$REST_URL"; done ;;
    config) config_leg ;;
    firstrun) firstrun_leg ;;
    install) install_leg ;;
    cluster)
      if [[ -z "$CLUSTER_TOKEN" || -z "$CLUSTER_CA" ]]; then
        RESULTS+=("SKIP cluster (set AMP_TOKEN and AMP_CA_FILE to run it against $CLUSTER_URL)")
        continue
      fi
      [[ -r "$CLUSTER_CA" ]] || die "AMP_CA_FILE $CLUSTER_CA is not readable"
      for proto in $PROTOCOLS; do run_leg cluster "$CLUSTER_URL" "$proto" "$CLUSTER_TOKEN" "$CLUSTER_CA" "" "" ""; done ;;
    *) die "unknown leg '$leg' in AMP_SMOKE_LEGS" ;;
  esac
done
