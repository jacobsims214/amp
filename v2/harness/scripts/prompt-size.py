#!/usr/bin/env python3
"""What a role's prompt costs (#2367): the system prompt by section and the tool schemas by tool.

Runs `amp-harness run --print-system-prompt --print-tools` (no model call) for a role and prints
characters and estimated tokens (chars / 4) for each part:

  role prompt, <amp_server_instructions>, <mcp_server_instructions>, <amp_context>,
  <preloaded_skills> (per skill), <project_context>, <available_skills>, and the tools
  (built-in, AMP, extra), largest tools first.

The estimate is rough; the eval's first-turn usage (input + cache write + cache read tokens) is
the real number. Use the same environment as `amp-harness run` (an AMP to connect to, and the
role's provider key in the environment from a subshell; this script never prints it):

  ( source ~/.config/amp-harness/dev-keys.env
    python3 -I v2/harness/scripts/prompt-size.py --cwd /path/to/repo [--role amp-manager] [--json] )
"""

from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path

HARNESS = Path(__file__).resolve().parent.parent
TAGS = [
    "amp_server_instructions",
    "mcp_server_instructions",
    "amp_context",
    "preloaded_skills",
    "project_context",
    "available_skills",
]
BUILTIN = {"read", "bash", "edit", "write", "grep", "find", "ls"}


def tok(n: int) -> int:
    return round(n / 4)


def sections(prompt: str) -> list[tuple[str, int]]:
    """(name, chars) per part, in prompt order. The role prompt is whatever no tag claims."""
    spans = []
    for tag in TAGS:
        for m in re.finditer(rf"<{tag}[ >].*?</{tag}>", prompt, re.S):
            spans.append((m.start(), m.end(), tag, m.group(0)))
    spans.sort()
    out, claimed = [], 0
    for _, _, tag, text in spans:
        claimed += len(text)
        if tag == "preloaded_skills":
            for s in re.finditer(r'<skill name="([^"]+)".*?</skill>', text, re.S):
                out.append((f"preloaded skill {s.group(1)}", len(s.group(0))))
        else:
            out.append((f"<{tag}>", len(text)))
    out.insert(0, ("role prompt (and glue)", len(prompt) - claimed))
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--bin", default=str(HARNESS / "target/debug/amp-harness"))
    ap.add_argument("--role", default=None)
    ap.add_argument("--cwd", default=".")
    ap.add_argument("--json", action="store_true", help="print the numbers as JSON")
    ap.add_argument("--top", type=int, default=15, help="largest tools to list")
    args = ap.parse_args()

    cmd = [args.bin, "run", "--print-system-prompt", "--print-tools", "--cwd", args.cwd]
    if args.role:
        cmd += ["--role", args.role]
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=120)
    if p.returncode != 0:
        print(p.stderr.strip()[-2000:], file=sys.stderr)
        return p.returncode
    # The tools are the last line (one JSON array); the prompt is everything before it.
    head, _, last = p.stdout.rstrip("\n").rpartition("\n")
    tools = json.loads(last)
    prompt = head

    parts = sections(prompt)
    rows = []
    for t in tools:
        size = len(json.dumps({"name": t["name"], "description": t["description"],
                               "parameters": t["parameters"]}, separators=(",", ":")))
        kind = "built-in" if t["name"] in BUILTIN else ("AMP" if t["name"].startswith("amp_") else "other")
        rows.append((t["name"], kind, size, len(t["description"] or "")))
    by_kind: dict[str, list] = {}
    for r in rows:
        by_kind.setdefault(r[1], []).append(r)

    total_prompt = len(prompt)
    total_tools = sum(r[2] for r in rows)
    if args.json:
        print(json.dumps({
            "system_prompt_chars": total_prompt,
            "sections": dict(parts),
            "tools": {k: {"count": len(v), "chars": sum(r[2] for r in v)} for k, v in by_kind.items()},
            "tool_chars": total_tools,
            "per_tool": {r[0]: r[2] for r in rows},
            "est_tokens": tok(total_prompt + total_tools),
        }, indent=2))
        return 0

    print(f"{'part':44} {'chars':>8} {'~tokens':>8}")
    for name, n in parts:
        print(f"{name:44} {n:8} {tok(n):8}")
    print(f"{'= system prompt':44} {total_prompt:8} {tok(total_prompt):8}")
    for kind, rs in sorted(by_kind.items()):
        n = sum(r[2] for r in rs)
        print(f"{f'tools: {kind} ({len(rs)})':44} {n:8} {tok(n):8}")
    print(f"{'= tools':44} {total_tools:8} {tok(total_tools):8}")
    print(f"{'== total':44} {total_prompt + total_tools:8} {tok(total_prompt + total_tools):8}")
    print(f"\nlargest tools (schema chars, description chars):")
    for name, kind, size, d in sorted(rows, key=lambda r: -r[2])[: args.top]:
        print(f"  {name:40} {size:6} {d:6}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
