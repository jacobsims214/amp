#!/usr/bin/env bash
# seed-sample-receipt.sh <task_id>: give a ticket of a local AMP (make dev,
# auth off) a verification receipt with a real video, end to end (#126).
#
# It sets the ticket's project to Inform, makes a 3 s ffmpeg testsrc clip,
# dispatches the ticket, opens its receipt (amp_open_receipt), declares the
# clip (amp_add_evidence), uploads it to the one-time URL, and completes the
# ticket with a visual check citing it (amp_complete_task). The clip then
# plays in the ticket's drawer, and seeking returns 206 Partial Content.
#
# The ticket must be ready (backlog) and an agent ticket. Needs curl,
# python3 and ffmpeg.
#
#   AMP_URL       REST, default http://localhost:3001
#   AMP_MCP_URL   MCP, default http://localhost:8000/mcp
set -euo pipefail

TASK_ID="${1:-}"
if [[ ! "$TASK_ID" =~ ^[0-9]+$ ]]; then
  echo "usage: $0 <task_id>" >&2
  exit 2
fi
AMP_URL="${AMP_URL:-http://localhost:3001}"
AMP_MCP_URL="${AMP_MCP_URL:-http://localhost:8000/mcp}"
for bin in curl python3 ffmpeg; do
  command -v "$bin" >/dev/null || { echo "$0 needs $bin" >&2; exit 1; }
done

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# 1. The project, set to Inform (REST counts as the web UI with auth off).
project_id="$(curl -fsS "$AMP_URL/api/tasks/$TASK_ID" | python3 -c 'import json,sys; d=json.load(sys.stdin); print((d.get("task") or d)["project_id"])')"
curl -fsS -X PATCH -H 'Content-Type: application/json' -d '{"receipts_mode":"inform"}' \
  "$AMP_URL/api/projects/$project_id" >/dev/null
echo "project $project_id: receipts set to Inform"

# 2. A 3 s clip (90 frames).
ffmpeg -loglevel error -y -f lavfi -i testsrc=duration=3:size=640x360:rate=30 \
  -pix_fmt yuv420p -movflags +faststart "$work/video.mp4"
bytes="$(wc -c <"$work/video.mp4" | tr -d ' ')"
sha="$(python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$work/video.mp4")"
echo "clip: $bytes bytes, sha256 $sha"

# 3. MCP calls (the initialize / session protocol), one small client.
mcp() { # mcp <tool> <json args> -> the tool's JSON result on stdout
  python3 - "$AMP_MCP_URL" "$1" "$2" "$work/session" <<'PY'
import json, os, sys, urllib.request
url, tool, args, sess_file = sys.argv[1], sys.argv[2], json.loads(sys.argv[3]), sys.argv[4]
def post(body, session=None):
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Accept", "application/json, text/event-stream")
    if session:
        req.add_header("Mcp-Session-Id", session)
    with urllib.request.urlopen(req) as r:
        raw = r.read().decode()
        sid = r.headers.get("Mcp-Session-Id")
    msgs = []
    for line in raw.splitlines():
        if line.startswith("data:"):
            msgs.append(json.loads(line[5:]))
    if not msgs and raw.strip():
        msgs.append(json.loads(raw))
    return (msgs[-1] if msgs else None), sid
session = open(sess_file).read().strip() if os.path.exists(sess_file) else None
if not session:
    _, session = post({"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": {
        "protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "seed-sample-receipt", "version": "1"}}})
    post({"jsonrpc": "2.0", "method": "notifications/initialized"}, session)
    open(sess_file, "w").write(session or "")
resp, _ = post({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": tool, "arguments": args}}, session)
if resp.get("error"):
    sys.exit(f"{tool}: {resp['error'].get('message')}")
res = resp["result"]
text = "".join(c.get("text", "") for c in res.get("content", []))
if res.get("isError"):
    sys.exit(f"{tool}: {text}")
print(text)
PY
}
field() { # field <a.0.b>: a value from the JSON on stdin
  python3 -c '
import json, sys
v = json.load(sys.stdin)
for k in sys.argv[1].split("."):
    v = v[int(k)] if isinstance(v, list) else v[k]
print(v)' "$1"
}

dispatch="$(mcp amp_dispatch_task "{\"task_id\": $TASK_ID, \"agent_id\": \"amp-worker\"}" | field dispatch_id)"
receipt="$(mcp amp_open_receipt "{\"task_id\": $TASK_ID, \"dispatch_id\": \"$dispatch\", \"summary\": \"Sample receipt with a 3 s test clip.\"}")"
receipt_id="$(echo "$receipt" | field receipt_id)"
if [[ "$receipt_id" == "0" ]]; then
  echo "the project's receipts setting is still Off" >&2
  exit 1
fi
tree="$(printf 'sample-%s' "$TASK_ID" | python3 -c 'import hashlib,sys; print(hashlib.sha1(sys.stdin.buffer.read()).hexdigest())')"
added="$(mcp amp_add_evidence "{\"receipt_id\": $receipt_id, \"dispatch_id\": \"$dispatch\", \"tree_sha\": \"$tree\",
  \"files\": [{\"name\": \"video.mp4\", \"bytes\": $bytes, \"sha256\": \"$sha\", \"duration_ms\": 3000, \"frames\": 90}]}")"
url="$(echo "$added" | field uploads.0.url)"
key="$(echo "$added" | field uploads.0.key)"
[[ "$url" == /* ]] && url="$AMP_URL$url"
curl -fsS -T "$work/video.mp4" "$url" >/dev/null
echo "uploaded $key"

mcp amp_complete_task "{\"task_id\": $TASK_ID, \"dispatch_id\": \"$dispatch\", \"receipt_id\": $receipt_id, \"tree_sha\": \"$tree\",
  \"checks\": [{\"criterion_ref\": \"1\", \"kind\": \"visual\", \"verdict\": \"pass\", \"evidence_key\": \"$key\",
  \"note\": \"sample clip\"}]}" >/dev/null
echo "ticket $TASK_ID completed with receipt $receipt_id"
echo "check a seek: curl -s -o /dev/null -w '%{http_code}\\n' -H 'Range: bytes=0-99' $AMP_URL/api/receipts/$key   # 206"
