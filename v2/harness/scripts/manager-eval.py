#!/usr/bin/env python3
"""Manager eval (#2363): scripted, headless runs of the AMP manager against a local AMP.

Each (model, scenario) pair runs `amp-harness run -p ... --json` once, in a throwaway HOME and a
throwaway git repo under --out, against a throwaway AMP project named "QA 2363 ...". The script
reads the JSON event stream, times it, checks what the manager did in AMP, archives the project,
and writes results.json plus a markdown table (results.md).

Scenarios:
  init    new empty git folder (no commits, no .amp.json): "set up a new project here, it's a Go
          API" -> amp_create_project, .amp.json written here, architecture/project-overview.md.
  plan    greenfield repo linked to an empty project (plan approval on): the user's real
          "plan out this project" prompt -> epics/stories/tickets with dependencies, amp_check_plan,
          no dispatch, the approve link in the answer.
  status  a seeded board: "what's on the board / where are we" -> read-only tools only.
  run     a seeded, approval-off project with one small docs ticket: "run the ready tickets" ->
          amp_run_ticket on it, ticket completed.

Measured per run: wall time, turns, tool calls, tool errors, schema errors, policy denials,
time to the first tool call, silent time (a model turn with no text or tool call yet; this is what
a person sees as "Thinking"), tokens and cost.

Keys: the provider key must already be in the environment (e.g. OPENROUTER_API_KEY). This script
never prints, logs or writes it. Run it from a subshell that sources your dev keys:

  ( source ~/.config/amp-harness/dev-keys.env
    python3 -I v2/harness/scripts/manager-eval.py --models mimo-high,sonnet --scenarios all )

Models are named presets (see MODELS below) or `label=provider/model@thinking` for anything else.
The AMP must be a compose AMP with auth off (every call acts as a global admin) and its REST
listener (--rest) reachable: approval-off projects are set up as the person over REST.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import shutil
import signal
import string
import subprocess
import sys
import threading
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
HARNESS = HERE.parent

# Providers the presets need besides the shipped ones. `orm` is OpenRouter's Anthropic-compatible
# Messages endpoint, so Claude models get prompt caching (OpenRouter's chat endpoint doesn't cache
# them without cache_control).
EXTRA_PROVIDERS = """
[providers.orm]
api = "anthropic-messages"
base_url = "https://openrouter.ai/api"
key_env = "OPENROUTER_API_KEY"
header_timeout_secs = 180
stream_idle_timeout_secs = 300

[providers.orm.models."anthropic/claude-sonnet-5.5"]
name = "Claude Sonnet 5.5 (OpenRouter)"
reasoning = true
context_window = 1000000
max_tokens = 64000
cost = { input = 2.0, output = 10.0, cache_read = 0.2, cache_write = 2.5 }
thinking_map = { xhigh = "xhigh", max = "max" }
unsupported_thinking = ["off"]
compat = { force_adaptive_thinking = true, supports_temperature = false }

[providers.orm.models."anthropic/claude-haiku-5.5"]
name = "Claude Haiku 5.5 (OpenRouter)"
reasoning = true
context_window = 1000000
max_tokens = 64000
cost = { input = 0.1, output = 0.5, cache_read = 0.01, cache_write = 0.125 }
thinking_map = { xhigh = "xhigh", max = "max" }
unsupported_thinking = ["off"]
compat = { force_adaptive_thinking = true, supports_temperature = false }

[providers.openrouter.models."openai/gpt-6.1-sol"]
name = "GPT-6.1 Sol"
reasoning = true
context_window = 400000
max_tokens = 64000
cost = { input = 2.0, output = 10.0, cache_read = 0.2 }

[providers.openrouter.models."google/gemini-3.8-flash"]
name = "Gemini 3.8 Flash"
reasoning = true
context_window = 1048576
max_tokens = 64000
cost = { input = 0.825, output = 4.125, cache_read = 0.08 }
"""

# label -> [roles.manager] body. None = the shipped default (whatever the binary ships).
MODELS: dict[str, str | None] = {
    "default": None,
    "mimo-high": 'model = "openrouter/xiaomi/mimo-v2.6-pro"\nthinking = "high"\n'
    "extra_body = { reasoning = { enabled = true } }\n",
    "mimo-off": 'model = "openrouter/xiaomi/mimo-v2.6-pro"\nthinking = "low"\n'
    "extra_body = { reasoning = { enabled = false } }\n",
    "glm-low": 'model = "openrouter/z-ai/glm-5.3"\nthinking = "low"\n',
    "sonnet": 'model = "orm/anthropic/claude-sonnet-5.5"\nthinking = "low"\n',
    "haiku": 'model = "orm/anthropic/claude-haiku-5.5"\nthinking = "low"\n',
    "gpt-sol": 'model = "openrouter/openai/gpt-6.1-sol"\nthinking = "low"\n',
    "gemini-flash": 'model = "openrouter/google/gemini-3.8-flash"\nthinking = "low"\n',
}

SCENARIOS = ["init", "plan", "status", "run"]

PLAN_PROMPT = (
    "Ok i want you to plan out this project its all brand new right now for a demo api service. "
    "We are going to build a go based api and mcp service (Official packages and latest versions "
    "only) for a simple feed service. we use nats jet stream and we have this whole thing developed "
    "within a docker compose stack, you SHOULD NOT run things outside of docker compose. A GO API "
    "lets us build feeds for users and post messages to the feeds that an app service or AI agent "
    "can listen to, like a generic incoming event bus (got an email, a text, a webhook from another "
    "service). We want a quick POC built and tested and a light weight admin UI to see and admin "
    "the feeds. Postgres for the db, NATS JetStream in the middle."
)

MUTATING = {
    "amp_create_tasks", "amp_create_epic", "amp_create_story", "amp_update_task", "amp_update_epic",
    "amp_update_story", "amp_add_task_comment", "amp_dispatch_task", "amp_run_ticket",
    "amp_complete_task", "amp_set_task_state", "amp_claim_task", "amp_assign_task",
    "amp_block_task", "amp_delete_task", "amp_set_task_dependencies", "amp_kb_write",
    "amp_create_project", "amp_update_project_settings", "amp_handover_tasks",
    "amp_answer_question", "write", "edit",
}


def code(n: int = 5) -> str:
    return "Q" + "".join(random.choice(string.ascii_uppercase) for _ in range(n - 1))


class Env:
    def __init__(self, args, home: Path):
        self.bin = args.bin
        self.url = args.url
        self.rest = args.rest.rstrip("/")
        self.home = home
        self.env = {
            k: v for k, v in os.environ.items()
            if k not in ("AMP_MCP_URL", "AMP_TOKEN", "AMP_HARNESS_PROFILE", "XDG_CONFIG_HOME")
        }
        self.env.update(
            HOME=str(home),
            XDG_CONFIG_HOME=str(home / ".config"),
            XDG_STATE_HOME=str(home / ".state"),
            AMP_CREDENTIAL_STORE="file",
            AMP_HARNESS_KEYCHAIN_PROMPTS="never",
            AMP_HARNESS_CEILING_DIR=str(home.parent),
            AMP_MCP_URL=self.url,
            NO_COLOR="1",
        )

    def mcp(self, tool: str, args: dict) -> dict:
        p = subprocess.run(
            [self.bin, "mcp", "call", tool, json.dumps(args)],
            env=self.env, capture_output=True, text=True, timeout=60,
        )
        if p.returncode != 0:
            raise RuntimeError(f"{tool}: {p.stderr.strip()[:400]}")
        try:
            return json.loads(p.stdout)
        except json.JSONDecodeError:
            return {"text": p.stdout}

    def rest_call(self, method: str, path: str, body: dict) -> dict:
        req = urllib.request.Request(
            self.rest + path, method=method, data=json.dumps(body).encode(),
            headers={"content-type": "application/json"},
        )
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.loads(r.read() or b"{}")


def git(repo: Path, *a: str) -> None:
    subprocess.run(["git", "-C", str(repo), *a], check=True, capture_output=True,
                   env={**os.environ, "GIT_AUTHOR_NAME": "qa", "GIT_AUTHOR_EMAIL": "qa@x",
                        "GIT_COMMITTER_NAME": "qa", "GIT_COMMITTER_EMAIL": "qa@x"})


def marker(repo: Path, pid: int, name: str, c: str) -> None:
    (repo / ".amp.json").write_text(json.dumps(
        {"project_id": pid, "project_name": name, "project_code": c}, indent=2) + "\n")


def seed_board(e: Env, pid: int) -> list[int]:
    epic = e.mcp("amp_create_epic", {"project_id": pid, "name": "Feed API POC"})
    eid = epic.get("id") or epic.get("epic", {}).get("id")
    story = e.mcp("amp_create_story", {"project_id": pid, "epic_id": eid, "name": "Foundation"})
    sid = story.get("id") or story.get("story", {}).get("id")
    out = e.mcp("amp_create_tasks", {"project_id": pid, "tasks": [
        {"ref": "a", "story_id": sid, "name": "Write NOTES.md", "assigned_to": "amp:amp-worker-docs",
         "description": "Create NOTES.md at the repo root containing exactly one line: "
                        "hello from QA 2363. Commit it on your branch.",
         "acceptance_criteria": "NOTES.md exists with that line, committed."},
        # A person's step, so "run the ready tickets" stops after ticket a.
        {"ref": "b", "story_id": sid, "name": "Merge wave 1", "performer": "human",
         "human_reason": "review", "description": "Review and merge the NOTES.md branch.",
         "acceptance_criteria": "merged", "depends_on": ["a"]},
        {"ref": "c", "story_id": sid, "name": "Review foundation", "assigned_to": "amp:amp-reviewer",
         "description": "Review wave 1.", "acceptance_criteria": "review done", "depends_on": ["b"]},
    ]})
    ids = [t.get("id") for t in out.get("tasks", out.get("created", []))]
    return [i for i in ids if i]


def setup(scn: str, e: Env, repo: Path, tag: str) -> dict:
    """Returns {project_id?, prompt, ...} for the scenario."""
    repo.mkdir(parents=True)
    git(repo, "init", "-q", "-b", "main")
    if scn == "init":
        c = code(5)
        name = f"QA 2363 {tag} init"
        return {"prompt": f"Set up a new project here. It's a Go REST API for a small todo-list demo, "
                          f"Postgres, built and tested only inside docker compose. Call the project "
                          f"\"{name}\" with code {c}.", "expect_name": name}
    c = code(5)
    name = f"QA 2363 {tag} {scn}"
    p = e.mcp("amp_create_project", {"name": name, "code": c})
    pid = p["id"]
    marker(repo, pid, name, c)
    ctx = {"project_id": pid}
    if scn == "plan":
        ctx["prompt"] = PLAN_PROMPT
    elif scn == "status":
        seed_board(e, pid)
        ctx["prompt"] = "What's on the board? Where are we?"
    elif scn == "run":
        (repo / "README.md").write_text("# QA 2363 run scenario\n")
        (repo / ".gitignore").write_text(".worktrees/\n")
        git(repo, "add", "-A")
        git(repo, "commit", "-q", "-m", "init")
        e.rest_call("PATCH", f"/api/projects/{pid}", {"require_plan_approval": False})
        ids = seed_board(e, pid)
        ctx["seed_ids"] = ids
        ctx["prompt"] = "Run the ready tickets."
    return ctx


def run_one(args, label: str, manager: str | None, scn: str, out: Path) -> dict:
    tag = f"{label}-{random.randint(100, 999)}"
    base = out / label / scn
    if base.exists():
        shutil.rmtree(base)
    home = base / "home"
    cfg = home / ".config" / "amp-harness"
    cfg.mkdir(parents=True)
    toml = f'[server]\nurl = "{args.url}"\nweb_url = "{args.web}"\n' + EXTRA_PROVIDERS
    if manager:
        toml += "\n[roles.manager]\n" + manager
    # Workers stay on the shipped cheap models; the run scenario uses one docs worker.
    (cfg / "config.toml").write_text(toml)
    e = Env(args, home)
    repo = base / "repo"
    res: dict = {"model": label, "scenario": scn}
    try:
        ctx = setup(scn, e, repo, tag)
    except Exception as ex:  # noqa: BLE001
        res.update(outcome="setup-failed", error=str(ex)[:300])
        return res
    before = {p["id"] for p in projects(e)} if scn == "init" else set()

    cmd = [args.bin, "run", "--json", "--cwd", str(repo), "-p", ctx["prompt"]]
    t0 = time.monotonic()
    proc = subprocess.Popen(cmd, env=e.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                            text=True, start_new_session=True)
    events: list[tuple[float, dict]] = []
    err_lines: list[str] = []
    threading.Thread(target=lambda: err_lines.extend(proc.stderr), daemon=True).start()
    timer = threading.Timer(args.timeout, lambda: os.killpg(proc.pid, signal.SIGTERM))
    timer.start()
    for line in proc.stdout:
        try:
            events.append((time.monotonic() - t0, json.loads(line)))
        except json.JSONDecodeError:
            pass
    proc.wait()
    timed_out = not timer.is_alive() and proc.returncode != 0
    timer.cancel()
    wall = time.monotonic() - t0
    (base / "events.jsonl").write_text("".join(json.dumps({"t": round(t, 2), **ev}) + "\n"
                                               for t, ev in events))
    (base / "stderr.txt").write_text("".join(err_lines)[-20000:])

    res.update(metrics(events, wall))
    res["timed_out"] = timed_out
    sess = next((ev for _, ev in events if ev.get("type") == "session"), {})
    res["model_id"] = f'{sess.get("provider")}/{sess.get("model")}@{sess.get("thinking")}'
    res["list_cost_usd"] = list_cost(sess.get("model"), res)
    try:
        res["checks"] = check(scn, e, repo, ctx, res, before)
    except Exception as ex:  # noqa: BLE001
        res["checks"] = {"check-error": str(ex)[:200]}
    res["pass"] = all(v is True for v in res["checks"].values())
    cleanup(e, ctx, repo, before, scn)
    return res


def metrics(events, wall: float) -> dict:
    tools, errors, schema, denied, turns = [], 0, 0, 0, 0
    first_tool = None
    silent_total, silent_max, turn_t, seen_visible = 0.0, 0.0, None, False
    result = {}
    for t, ev in events:
        ty = ev.get("type")
        if ty == "turn_start":
            turns += 1
            turn_t, seen_visible = t, False
        elif ty in ("text_delta", "tool_start", "assistant") and turn_t is not None and not seen_visible:
            gap = t - turn_t
            silent_total += gap
            silent_max = max(silent_max, gap)
            seen_visible = True
        if ty == "tool_start":
            tools.append(ev.get("name"))
            if first_tool is None:
                first_tool = t
        elif ty == "tool_end" and ev.get("is_error"):
            errors += 1
            txt = ev.get("text", "")
            if "schema error" in txt or "Validation failed" in txt:
                schema += 1
            if "Denied by" in txt:
                denied += 1
        elif ty == "result":
            result = ev
    # A turn still silent when the run ended (aborted / timed out) counts too.
    if turn_t is not None and not seen_visible:
        gap = wall - turn_t
        silent_total += gap
        silent_max = max(silent_max, gap)
    usage = result.get("usage", {}) or {}
    cw = sum((ev.get("usage") or {}).get("cache_write_tokens", 0)
             for _, ev in events if ev.get("type") == "assistant")
    return {
        "outcome": result.get("outcome", "no-result"),
        "wall_s": round(wall, 1),
        "turns": turns,
        "tool_calls": len(tools),
        "tool_errors": errors,
        "schema_errors": schema,
        "denied": denied,
        "first_tool_s": round(first_tool, 1) if first_tool is not None else None,
        "silent_total_s": round(silent_total, 1),
        "silent_max_s": round(silent_max, 1),
        "input_tokens": usage.get("input_tokens", 0),
        "output_tokens": usage.get("output_tokens", 0),
        "cache_read_tokens": usage.get("cache_read_tokens", 0),
        "cache_write_tokens": cw,
        "cost_usd": round(usage.get("cost_usd", 0.0), 4),
        "tools": tools,
        "final_text": result.get("text") or "",
    }


PRICES: dict[str, dict] = {}


def load_prices() -> None:
    """OpenRouter's public price list (no key needed), so costs compare across models even where
    the harness's own price table is stale or lacks cache writes. Best effort."""
    try:
        with urllib.request.urlopen("https://openrouter.ai/api/v1/models", timeout=20) as r:
            for m in json.loads(r.read())["data"]:
                p = m.get("pricing", {})
                PRICES[m["id"]] = {k: float(p.get(k) or 0) for k in
                                   ("prompt", "completion", "input_cache_read", "input_cache_write")}
    except Exception:  # noqa: BLE001
        pass


def list_cost(model: str | None, r: dict) -> float | None:
    p = PRICES.get(model or "")
    if not p:
        return None
    return round(r["input_tokens"] * p["prompt"] + r["output_tokens"] * p["completion"]
                 + r["cache_read_tokens"] * p["input_cache_read"]
                 + r["cache_write_tokens"] * p["input_cache_write"], 4)


def projects(e: Env) -> list[dict]:
    r = e.mcp("amp_list_projects", {})
    return r if isinstance(r, list) else r.get("projects", [])


def check(scn, e: Env, repo: Path, ctx: dict, res: dict, before: set) -> dict:
    tools = res["tools"]
    c = {"no_schema_errors": res["schema_errors"] == 0, "no_denials": res["denied"] == 0,
         "completed": res["outcome"] == "completed"}
    if scn == "init":
        new = [p for p in projects(e) if p["id"] not in before]
        mine = [p for p in new if p.get("name") == ctx["expect_name"]]
        # Only this run's project: other runs (in parallel) create projects too.
        ctx["created"] = [p["id"] for p in mine]
        c["project_created"] = bool(mine)
        m = repo / ".amp.json"
        pid = mine[0]["id"] if mine else None
        c["marker_written"] = m.exists() and pid is not None and \
            json.loads(m.read_text()).get("project_id") == pid
        if pid:
            try:
                doc = e.mcp("amp_kb_get", {"project_id": pid, "path": "architecture/project-overview.md"})
                c["kb_overview"] = bool(doc.get("content"))
            except RuntimeError:
                c["kb_overview"] = False
        else:
            c["kb_overview"] = False
    elif scn == "plan":
        pid = ctx["project_id"]
        plan = e.mcp("amp_get_plan", {"project_id": pid})
        tl = e.mcp("amp_list_tasks", {"project_id": pid, "owner": "any"})
        n_tickets = plan.get("tickets", 0)
        c["epics_stories"] = bool(plan.get("epics")) and any(ep.get("stories") for ep in plan["epics"])
        c["tickets>=4"] = n_tickets >= 4
        c["dependencies"] = any(t.get("waits_on") or t.get("dependency_ids")
                                for t in iter_tasks(tl))
        c["check_plan_called"] = "amp_check_plan" in tools
        c["no_dispatch"] = not ({"amp_run_ticket", "amp_dispatch_task"} & set(tools))
        c["approve_link_given"] = "/plan" in res["final_text"]
        res["tickets_created"] = n_tickets
    elif scn == "status":
        c["read_only"] = not (MUTATING & set(tools))
        c["answered"] = len(res["final_text"]) > 40
    elif scn == "run":
        ids = ctx.get("seed_ids", [])
        c["run_ticket_called"] = "amp_run_ticket" in tools
        if ids:
            t = e.mcp("amp_get_task", {"task_id": ids[0]})
            st = (t.get("task") or t).get("state")
            c["first_ticket_completed"] = st in ("completed", "done")
    return c


def iter_tasks(tl):
    if isinstance(tl, dict):
        for k, v in tl.items():
            if isinstance(v, list):
                for x in v:
                    if isinstance(x, dict) and "id" in x:
                        yield x
            elif isinstance(v, dict):
                yield from iter_tasks(v)


def cleanup(e: Env, ctx: dict, repo: Path, before: set, scn: str) -> None:
    ids = []
    if ctx.get("project_id"):
        ids.append(ctx["project_id"])
    ids += ctx.get("created", [])
    for pid in ids:
        try:
            e.mcp("amp_archive_project", {"project_id": pid})
        except Exception:  # noqa: BLE001
            print(f"manager-eval: could not archive project {pid}", file=sys.stderr)
    # Worker worktrees hold their own target dirs only for code tickets; drop them all.
    shutil.rmtree(repo / ".worktrees", ignore_errors=True)


def table(rows: list[dict]) -> str:
    cols = ["model", "scenario", "pass", "outcome", "wall_s", "first_tool_s", "silent_max_s",
            "silent_total_s", "turns", "tool_calls", "tool_errors", "schema_errors", "denied",
            "cost_usd", "list_cost_usd"]
    out = ["| " + " | ".join(cols) + " | failed checks |", "|" + "---|" * (len(cols) + 1)]
    for r in rows:
        failed = ", ".join(k for k, v in (r.get("checks") or {}).items() if v is not True)
        out.append("| " + " | ".join(str(r.get(k, "")) for k in cols) + f" | {failed} |")
    return "\n".join(out)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bin", default=str(HARNESS / "target/debug/amp-harness"), type=lambda p: str(Path(p).resolve()))
    ap.add_argument("--url", default=os.environ.get("AMP_EVAL_URL", "http://localhost:18000/mcp"))
    ap.add_argument("--rest", default=os.environ.get("AMP_EVAL_REST_URL", "http://localhost:3001"))
    ap.add_argument("--web", default=os.environ.get("AMP_EVAL_WEB_URL", "http://localhost:5173"))
    ap.add_argument("--models", default="default", help="comma list of presets or label=provider/model@thinking")
    ap.add_argument("--scenarios", default="all")
    ap.add_argument("--timeout", type=int, default=480, help="seconds per run")
    ap.add_argument("--out", default=str(HARNESS / "target/manager-eval" / time.strftime("%Y%m%d-%H%M%S")))
    ap.add_argument("--budget", type=float, default=3.0, help="stop starting runs past this total USD")
    ap.add_argument("--report", nargs="+", metavar="DIR",
                    help="no runs: one table from earlier --out dirs (recomputes list prices)")
    args = ap.parse_args()

    if args.report:
        load_prices()
        rows = []
        for d in args.report:
            for r in json.loads((Path(d) / "results.json").read_text()):
                ev = Path(d) / r["model"] / r["scenario"] / "events.jsonl"
                lines = [json.loads(x) for x in ev.read_text().splitlines()] if ev.exists() else []
                r["cache_write_tokens"] = sum((x.get("usage") or {}).get("cache_write_tokens", 0)
                                              for x in lines if x.get("type") == "assistant")
                sess = next((x for x in lines if x.get("type") == "session"), {})
                r["list_cost_usd"] = list_cost(sess.get("model"), r)
                r["model"] = f'{r["model"]} ({Path(d).name})'
                rows.append(r)
        print(table(rows))
        total = sum(r.get("list_cost_usd") or r.get("cost_usd") or 0 for r in rows)
        print(f"\nTotal list cost: ${total:.2f}")
        return 0

    if not os.environ.get("OPENROUTER_API_KEY") and not os.environ.get("ANTHROPIC_API_KEY"):
        print("manager-eval: no provider key in the environment (source your dev keys first)", file=sys.stderr)
        return 2
    scns = SCENARIOS if args.scenarios == "all" else args.scenarios.split(",")
    models: list[tuple[str, str | None]] = []
    for m in args.models.split(","):
        if "=" in m:
            label, spec = m.split("=", 1)
            ref, _, th = spec.partition("@")
            models.append((label, f'model = "{ref}"\nthinking = "{th or "low"}"\n'))
        else:
            models.append((m, MODELS[m]))
    out = Path(args.out).resolve()
    load_prices()
    out.mkdir(parents=True, exist_ok=True)
    rows, spent = [], 0.0
    for label, manager in models:
        for scn in scns:
            if spent >= args.budget:
                print(f"manager-eval: budget ${args.budget} reached, skipping {label}/{scn}", file=sys.stderr)
                continue
            print(f"manager-eval: {label} / {scn} ...", file=sys.stderr, flush=True)
            r = run_one(args, label, manager, scn, out)
            spent += r.get("cost_usd", 0) or 0
            rows.append(r)
            print(f"  -> pass={r.get('pass')} outcome={r.get('outcome')} wall={r.get('wall_s')}s "
                  f"silent_max={r.get('silent_max_s')}s errors={r.get('tool_errors')} "
                  f"cost=${r.get('cost_usd')}", file=sys.stderr, flush=True)
            (out / "results.json").write_text(json.dumps(rows, indent=2))
    (out / "results.md").write_text(table(rows) + f"\n\nTotal cost: ${spent:.2f}\n")
    print(table(rows))
    print(f"\nTotal cost: ${spent:.2f}  (results in {out})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
