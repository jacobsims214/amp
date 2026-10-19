---
title: Merging and CI — closing the loop from ticket to main
tags: [merge, ci, pull-request, unmerged, github]
---

# Merging and CI

A completed ticket isn't shipped until its work is merged. AMP tracks the gap.

- **Waiting to merge**: `amp_list_unmerged(project_id)` and the panel in **Mission** list
  completed tickets whose runs pushed work that hasn't been marked merged.
- **Marking merged**: the **Merged** button in Mission; or your agent records
  `amp_report_run(task_id, outcome="completed", merged=true, commit=<merge sha>)` when you merge or
  tell it to; or CI reports it automatically.
- **From CI**: after a merge, CI calls the MCP tool `amp_report_merged` with the branch, PR URL
  or commit (`branch`, `pr_url`, `commit`) plus the `merge_commit`. Every ticket whose runs produced
  that work is marked merged, and the tool returns `merged_task_ids`. It is one stateless JSON-RPC
  POST to `/mcp`:

  ```sh
  curl -fsS "$AMP_URL/mcp" -H "Authorization: Bearer $AMP_TOKEN" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: tools/call' -H 'Mcp-Name: amp_report_merged' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"amp_report_merged",
         "arguments":{"branch":"feat/my-branch","merge_commit":"<merge sha>"},
         "_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28",
                  "io.modelcontextprotocol/clientInfo":{"name":"ci","version":"1"},
                  "io.modelcontextprotocol/clientCapabilities":{}}}}'
  ```

  AMP API tokens work only on `/mcp`, so don't call AMP's REST paths from CI with one. Use a token
  from an agent account made for CI. Read access to the project is enough.

Workers put the ticket ref in their commit messages and record branch, commit, PR and pushed in
their run, which is what lets merges, and later traces, find the ticket.
