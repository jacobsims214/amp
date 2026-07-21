---
description: GitHub PR reviewer — uses gh CLI, jq, delta, git to fetch PR diffs, do full in-depth code reviews, post line-level review comments, approve or request changes. Dispatched by the manager to review a specific PR.
mode: subagent
hidden: true
model: amazon-bedrock/us.anthropic.claude-sonnet-5
steps: 20
permission:
  edit: deny
  bash: allow
  webfetch: deny
---