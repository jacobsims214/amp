---
description: {{description_yaml}}
mode: subagent
# The manager may dispatch this agent even though its id doesn't start with amp-.
dispatch: true
# Its model: [roles.{{model_role}}] in config.toml, else `model:` here (a configured
# provider/model), else `based_on: <agent id>`'s model, else [models] fallback (default:
# worker-backend's model).
# Skills whose instructions are preloaded into the prompt.
skills: []
# Tool rules (opencode's format): edit, bash, webfetch, websearch, task, or any tool name, each
# allow/deny or a map of pattern to allow/deny. Evaluated in order, the last match wins, so put
# "*" first. No rule means allow.
permission:
  todowrite: deny
  question: deny
---

# {{title}}

{{description}}
