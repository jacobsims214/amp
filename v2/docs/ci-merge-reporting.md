# Reporting merges to AMP from CI

Agents finish tickets on branches; a person (or your merge bot) merges them. AMP shows finished,
unmerged work as **Waiting on a person** on Mission and in `amp_list_unmerged`. Report merges
from CI so that queue clears itself, and so a merged commit traces back to its tickets.

The call matches tickets by the branch (or PR URL, or commit) their agent runs recorded, and marks
each merged with the merge commit. The token only needs read access to the project, so use an
AMP agent account created for CI (Agents & tokens), not a person's token.

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
          curl -fsS -X POST "$AMP_URL/api/trace/merged" \
            -H "Authorization: Bearer $AMP_TOKEN" -H "Content-Type: application/json" \
            -d "{\"branch\": \"${{ github.event.pull_request.head.ref }}\",
                 \"pr\": \"${{ github.event.pull_request.html_url }}\",
                 \"merge_commit\": \"${{ github.event.pull_request.merge_commit_sha }}\"}"
        env:
          AMP_URL: ${{ vars.AMP_URL }}
          AMP_TOKEN: ${{ secrets.AMP_CI_TOKEN }}
```

The response lists the ticket IDs it marked (`merged_task_ids`). Agents can report a merge
themselves with `amp_report_run(task_id, outcome="completed", merged=true, commit=<merge sha>)`.
