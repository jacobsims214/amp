#!/usr/bin/env python3
"""Your own model and token usage in this Claude Code session, for amp_complete_task.

Claude Code gives the model no usage API (opencode's plugin has amp_session_usage), but it
writes every subagent's transcript to disk, and each assistant line carries message.model and
message.usage. A worker finds its own transcript by its dispatch ID: that ID is in the worker
prompt, so it appears in exactly one subagent transcript's first lines.

Usage:
  python3 session_usage.py --dispatch-id d_xxxxx     a dispatched worker (the normal case)
  python3 session_usage.py --transcript PATH.jsonl   any transcript, e.g. a main session

Prints one JSON object whose fields match amp_complete_task's; null when unknown.
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import sys
import time

# The dispatch ID is in the worker prompt, which is the transcript's first user line.
HEAD_BYTES = 64 * 1024
# Only look at transcripts touched recently: the worker is writing to its own right now.
RECENT_SECONDS = 7 * 24 * 3600


def projects_dir() -> str:
    base = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude")
    return os.path.join(base, "projects")


def find_transcript(dispatch_id: str) -> str | None:
    needle = dispatch_id.encode()
    cutoff = time.time() - RECENT_SECONDS
    candidates = glob.glob(os.path.join(projects_dir(), "*", "*", "subagents", "agent-*.jsonl"))
    candidates = [p for p in candidates if os.path.getmtime(p) >= cutoff]
    candidates.sort(key=os.path.getmtime, reverse=True)
    for path in candidates:
        with open(path, "rb") as f:
            # Match the opening prompt only: a worker that mentions another's dispatch ID
            # later on must not be mistaken for that worker.
            if needle in first_user_line(f.read(HEAD_BYTES)):
                return path
    return None


def first_user_line(head: bytes) -> bytes:
    for raw in head.splitlines():
        try:
            entry = json.loads(raw)
        except ValueError:
            continue
        if entry.get("type") == "user":
            return raw
    return b""


def summarize(path: str) -> dict:
    # A message is written once per content block, each line repeating its usage, and the
    # last line has the final output count: keep the last usage per message id.
    usage_by_id: dict[str, dict] = {}
    models: dict[str, int] = {}
    with open(path, encoding="utf-8") as f:
        for raw in f:
            try:
                entry = json.loads(raw)
            except ValueError:
                continue
            if entry.get("type") != "assistant":
                continue
            msg = entry.get("message") or {}
            usage = msg.get("usage")
            model = msg.get("model")
            if not usage or not model or model.startswith("<"):
                continue
            key = msg.get("id") or entry.get("requestId") or entry.get("uuid")
            if key not in usage_by_id:
                models[model] = models.get(model, 0) + 1
            usage_by_id[key] = usage

    if not usage_by_id:
        return {"transcript": path, "model": None, "input_tokens": None, "output_tokens": None,
                "cache_read_tokens": None, "cache_write_tokens": None, "messages": 0}

    def total(field: str) -> int:
        return sum(int(u.get(field) or 0) for u in usage_by_id.values())

    return {
        "transcript": path,
        "model": max(models, key=models.get),
        "input_tokens": total("input_tokens"),
        "output_tokens": total("output_tokens"),
        "cache_read_tokens": total("cache_read_input_tokens"),
        "cache_write_tokens": total("cache_creation_input_tokens"),
        "messages": len(usage_by_id),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--dispatch-id", help="your dispatch ID, from your worker prompt")
    group.add_argument("--transcript", help="path to a Claude Code transcript (.jsonl)")
    args = parser.parse_args()

    path = args.transcript or find_transcript(args.dispatch_id)
    if not path or not os.path.isfile(path):
        print(json.dumps({"error": "transcript not found; report the model only, never estimate tokens"}))
        return 1
    print(json.dumps(summarize(path)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
