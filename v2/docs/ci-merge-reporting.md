# Reporting merges to AMP from CI

Agents finish tickets on branches; a person (or your merge bot) merges them. AMP shows finished,
unmerged work as **Waiting on a person** on Mission and in `amp_list_unmerged`. Report merges
from CI so that queue clears itself, and so a merged commit traces back to its tickets.

CI reports a merge with one call to the MCP tool `amp_report_merged`. It matches tickets by the
branch, PR URL or commit that their agent runs recorded, and marks each one merged with the merge
commit. Use an AMP agent account created for CI (Agents & tokens), not a person's token. The token
only needs read access to the project.

AMP API tokens (`amp_...`) are accepted **only on `/mcp`**. Other paths such as `/api/...` sit
behind the login proxy, which refuses a bearer token, so an old `POST /api/trace/merged` step from
CI fails. Use the MCP call below instead.

The call is a single stateless JSON-RPC `tools/call` POST to `/mcp` (MCP 2026-07-28), so it needs no
session or `initialize` handshake:

```yaml
# .github/workflows/amp-merged.yml
on:
  pull_request:
    types: [closed]
jobs:
  report:
    if: github.event.pull_request.merged == true
    runs-on: ubuntu-latest
    steps:
      - run: |
          body=$(jq -nc --arg branch "$BRANCH" --arg pr "$PR_URL" --arg sha "$MERGE_SHA" '{
            jsonrpc: "2.0", id: 1, method: "tools/call",
            params: {
              name: "amp_report_merged",
              arguments: {branch: $branch, pr_url: $pr, merge_commit: $sha},
              _meta: {
                "io.modelcontextprotocol/protocolVersion": "2026-07-28",
                "io.modelcontextprotocol/clientInfo": {name: "ci", version: "1"},
                "io.modelcontextprotocol/clientCapabilities": {}
              }
            }
          }')
          curl -fsS "$AMP_URL/mcp" \
            -H "Authorization: Bearer $AMP_TOKEN" \
            -H "Content-Type: application/json" \
            -H "Accept: application/json, text/event-stream" \
            -H "MCP-Protocol-Version: 2026-07-28" \
            -H "Mcp-Method: tools/call" -H "Mcp-Name: amp_report_merged" \
            -d "$body"
        env:
          AMP_URL: ${{ vars.AMP_URL }}
          AMP_TOKEN: ${{ secrets.AMP_CI_TOKEN }}
          BRANCH: ${{ github.event.pull_request.head.ref }}
          PR_URL: ${{ github.event.pull_request.html_url }}
          MERGE_SHA: ${{ github.event.pull_request.merge_commit_sha }}
```

The GitHub values go in through `env` and `jq` builds the JSON, so a branch name can't break the
quoting or inject shell.

Arguments: give at least one of `branch`, `pr_url` or `commit` to match on, and `merge_commit` to
record. The tool result's text is JSON, `{"merged_task_ids": [...], "count": N}`, listing the
tickets it marked. A refused call (for example, one with no match key) still returns HTTP 200, with
the error in the JSON-RPC body (an `error` object or a result with `isError: true`). `curl -f`
won't catch it, so check the body if CI should fail on it. A bad token gets HTTP 401.

Agents can report a merge themselves with
`amp_report_run(task_id, outcome="completed", merged=true, commit=<merge sha>)`.
