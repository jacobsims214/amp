#!/usr/bin/env python3
"""What an opencode session (and the subagents it started) actually did.

    opencode-trace.py <session-id>        a session and its subagents
    opencode-trace.py --latest            the most recent top-level session

Prints each subagent's timeline: every model turn and tool call, with its
duration, tokens and cost, and flags tool calls that never finished (a hung
call looks exactly like an agent that's "still thinking") and anything slower
than SLOW seconds. Reads opencode's local database; changes nothing.
"""
import json
import os
import sqlite3
import sys
import time
from pathlib import Path

SLOW = 120  # seconds


def db_path():
    base = Path(os.environ.get("XDG_DATA_HOME") or Path.home() / ".local/share")
    return base / "opencode" / "opencode.db"


def hms(ms):
    return time.strftime("%H:%M:%S", time.localtime(ms / 1000)) if ms else "--:--:--"


def secs(a, b):
    return (b - a) / 1000 if a and b else None


def tool_summary(c):
    state = c.get("state") if isinstance(c.get("state"), dict) else {}
    inp = c.get("input") or state.get("input") or {}
    if isinstance(inp, dict) and "code" in inp:  # opencode's code-mode execute
        first = next((l.strip() for l in inp["code"].splitlines() if l.strip().startswith("await")), inp["code"].strip())
        return first[:110]
    if isinstance(inp, dict):
        for key in ("command", "filePath", "path", "url", "query", "description", "pattern"):
            if key in inp:
                return f"{key}={str(inp[key])[:100]}"
    return json.dumps(inp)[:110]


def trace(con, sid, depth=0):
    row = con.execute("select title, cost, tokens_input, time_created from session_v2 where id=?", (sid,)).fetchone()
    if not row:
        print(f"no session {sid}")
        return
    title, cost, tin, created = row
    pad = "  " * depth
    print(f"{pad}■ {sid}  {title or ''}  (cost ${cost or 0:.4f}, {tin or 0} input tokens)")
    msgs = con.execute("select seq, type, data from session_message where session_id=? order by seq", (sid,)).fetchall()
    now = time.time() * 1000
    first = last = None
    for seq, typ, data in msgs:
        d = json.loads(data)
        t = d.get("time", {})
        start, done = t.get("created"), t.get("completed")
        first = first or start
        last = done or last
        if typ != "assistant":
            continue
        tools = [c for c in d.get("content", []) if c.get("type") == "tool"]
        think = secs(start, t.get("streamed")) or 0
        dur = secs(start, done)
        tokens = d.get("tokens") or {}
        flag = ""
        if not done:
            stuck = (now - start) / 1000
            flag = f"  ⚠ NEVER FINISHED (running {stuck/60:.0f} min)"
        elif dur and dur > SLOW:
            flag = f"  ⚠ slow ({dur:.0f}s)"
        label = ", ".join(f"{c.get('name')}: {tool_summary(c)}" for c in tools) or "text"
        out = tokens.get("output", 0) + tokens.get("reasoning", 0)
        print(f"{pad}  {hms(start)}  {dur if dur is not None else 0:6.1f}s  model {think:5.1f}s  out {out:>6}  {label}{flag}")
    if first and last:
        print(f"{pad}  total {secs(first, last)/60:.1f} min over {sum(1 for m in msgs if m[1]=='assistant')} model turns")
    for (child,) in con.execute("select id from session_v2 where parent_id=? order by time_created", (sid,)):
        trace(con, child, depth + 1)


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    con = sqlite3.connect(f"file:{db_path()}?mode=ro", uri=True)
    sid = sys.argv[1]
    if sid == "--latest":
        sid = con.execute("select id from session_v2 where parent_id is null order by time_created desc limit 1").fetchone()[0]
    trace(con, sid)


if __name__ == "__main__":
    main()
