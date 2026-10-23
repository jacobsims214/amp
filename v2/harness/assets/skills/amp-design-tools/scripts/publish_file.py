#!/usr/bin/env python3
"""Publish a file as an AMP artifact (or a new version of one) without the
model re-typing its contents: the file is read here and sent to AMP's
amp_publish_artifact over MCP.

  publish_file.py <file> --task 222 [--title "Settings mockup"] [--kind html] [--note "..."]
  publish_file.py <file> --artifact 1 [--note "what changed"]

Needs AMP_MCP_URL (e.g. https://amp.example.com/mcp) and AMP_TOKEN (an AMP API
token). Without a token, publish with the amp_publish_artifact tool instead.
Prints the artifact's id, version and url.
"""
import argparse
import base64
import json
import os
import sys
import urllib.request
from pathlib import Path

KINDS = {".html": "html", ".htm": "html", ".svg": "svg", ".png": "image", ".jpg": "image", ".jpeg": "image",
         ".webp": "image", ".gif": "image", ".md": "markdown", ".pdf": "pdf", ".txt": "text"}
PROTOCOL = "2026-07-28"


def call(url, token, tool, arguments):
    meta = {"io.modelcontextprotocol/protocolVersion": PROTOCOL,
            "io.modelcontextprotocol/clientInfo": {"name": "amp-publish-file", "version": "1"},
            "io.modelcontextprotocol/clientCapabilities": {}}
    body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                       "params": {"name": tool, "arguments": arguments, "_meta": meta}}).encode()
    req = urllib.request.Request(url, data=body, headers={
        "Content-Type": "application/json", "Accept": "application/json, text/event-stream",
        "Authorization": f"Bearer {token}", "MCP-Protocol-Version": PROTOCOL, "Mcp-Method": "tools/call", "Mcp-Name": tool})
    with urllib.request.urlopen(req, timeout=120) as r:
        raw = r.read().decode()
    for line in raw.splitlines():  # SSE or plain JSON
        if line.startswith("data: "):
            raw = line[6:]
    msg = json.loads(raw)
    if "error" in msg:
        sys.exit(f"AMP: {msg['error'].get('message')}")
    res = msg["result"]
    text = res["content"][0]["text"]
    if res.get("isError"):
        sys.exit(f"AMP: {text}")
    return json.loads(text)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("--task", type=int)
    ap.add_argument("--project", type=int)
    ap.add_argument("--artifact", type=int)
    ap.add_argument("--title")
    ap.add_argument("--kind")
    ap.add_argument("--note", default="")
    a = ap.parse_args()
    url, token = os.environ.get("AMP_MCP_URL"), os.environ.get("AMP_TOKEN")
    if not url or not token:
        sys.exit("AMP_MCP_URL and AMP_TOKEN aren't set: publish with the amp_publish_artifact tool instead.")
    path = Path(a.file)
    kind = a.kind or KINDS.get(path.suffix.lower())
    if not kind:
        sys.exit(f"can't tell the kind of {path.name}: pass --kind")
    args = {"note": a.note}
    if kind in ("image", "pdf"):
        args["content_base64"] = base64.b64encode(path.read_bytes()).decode()
    else:
        args["content"] = path.read_text()
    if a.artifact:
        args["artifact_id"] = a.artifact
        if a.title:
            args["title"] = a.title
    else:
        if not (a.task or a.project) or not a.title:
            sys.exit("a new artifact needs --task (or --project) and --title")
        args.update({"title": a.title, "kind": kind})
        args["task_id" if a.task else "project_id"] = a.task or a.project
    out = call(url, token, "amp_publish_artifact", args)
    art = out["artifact"]
    print(f"artifact #{art['id']} v{art['current_version']} ({art['status']}) {art.get('url', '')}")


if __name__ == "__main__":
    main()
