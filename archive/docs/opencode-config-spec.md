# OpenCode Configuration Reference

Based on OpenCode documentation from https://opencode.ai/docs/agents

## Config File Location

- **Global**: `~/.config/opencode/opencode.json`
- **Project**: `./opencode.json` (in project root)

## Root Schema

```json
{
  "$schema": "https://opencode.ai/config.json",
  "agent": { ... },
  "mcpServers": { ... },
  "permission": { ... },
  ...
}
```

## Agent Configuration

Agents are defined under the `agent` key:

```json
{
  "agent": {
    "agent-name": {
      "mode": "primary" | "subagent",
      "description": "What this agent does",
      "prompt": "System prompt or {file:./path/to/prompt.txt}",
      "model": "provider/model-id",
      "temperature": 0.0-1.0,
      "top_p": 0.0-1.0,
      "steps": 10,
      "disable": false,
      "hidden": false,
      "color": "#hex" | "primary" | "accent" | "success" | "warning" | "error" | "info",
      "permission": {
        "edit": "ask" | "allow" | "deny",
        "bash": "ask" | "allow" | "deny" | { "*": "ask", "git status": "allow" },
        "webfetch": "ask" | "allow" | "deny",
        "task": { "*": "deny", "subagent-*": "allow" }
      }
    }
  }
}
```

### Required Fields

- `description`: Brief description of agent's purpose

### Optional Fields

- `mode`: `"primary"`, `"subagent"`, or `"all"` (default: `"all"`)
- `prompt`: System prompt text or file reference `{file:./path.txt}`
- `model`: Model ID in format `provider/model-id`
- `temperature`: 0.0-1.0, controls creativity (default: model-specific, usually 0)
- `top_p`: 0.0-1.0, alternative to temperature for diversity
- `steps`: Max iterations before forced response (default: unlimited)
- `disable`: Set `true` to disable agent
- `hidden`: Set `true` to hide from `@` autocomplete (subagents only)
- `color`: Visual color in UI
- `permission`: Tool permissions (see below)

## MCP Servers

MCP servers are configured at root level:

```json
{
  "mcpServers": {
    "server-name": {
      "command": "command-to-run",
      "args": ["arg1", "arg2"],
      "env": { "KEY": "value" }
    },
    "http-server": {
      "url": "http://localhost:8000"
    }
  }
}
```

## Permissions

Permissions control what actions agents can take:

```json
{
  "permission": {
    "edit": "ask" | "allow" | "deny",
    "bash": "ask" | "allow" | "deny" | {
      "*": "ask",
      "git status": "allow",
      "grep *": "allow"
    },
    "webfetch": "ask" | "allow" | "deny",
    "task": {
      "*": "deny",
      "subagent-*": "allow"
    }
  }
}
```

Values:
- `"ask"`: Prompt for approval
- `"allow"`: Allow without approval
- `"deny"`: Disable the tool

Patterns (for bash/task):
- `"*"`: Wildcard matches all
- `"git *"`: Matches commands starting with "git "
- `"prefix-*"`: Matches agents starting with "prefix-"

**Rule evaluation**: Last matching rule wins.

## Agent Modes

- **primary**: Main agents user interacts with (switch with Tab key)
- **subagent**: Specialized agents invoked via `@mention` or Task tool
- **all**: Can be used as both (default)

## Built-in Agents

OpenCode includes these built-in agents:

- **build** (primary): Full access, default agent
- **plan** (primary): Read-only, restricted permissions
- **general** (subagent): Multi-step tasks, full tool access
- **explore** (subagent): Read-only codebase exploration
- **compaction** (primary, hidden): Context compaction
- **title** (primary, hidden): Session title generation
- **summary** (primary, hidden): Session summarization

## Markdown Agent Files

Agents can also be defined as markdown files:

**Location**:
- Global: `~/.config/opencode/agents/`
- Project: `.opencode/agents/`

**Format** (filename becomes agent name, e.g., `review.md`):

```markdown
---
description: Reviews code for quality
mode: subagent
model: anthropic/claude-sonnet-4-20250514
temperature: 0.1
permission:
  edit: deny
  bash: false
---

You are a code reviewer. Focus on security and best practices.
```

## Examples

### Basic Primary Agent

```json
{
  "$schema": "https://opencode.ai/config.json",
  "agent": {
    "build": {
      "mode": "primary",
      "description": "Full development agent",
      "prompt": "You are a helpful coding assistant.",
      "permission": {
        "edit": "allow",
        "bash": "allow"
      }
    }
  }
}
```

### Read-only Subagent

```json
{
  "agent": {
    "analyzer": {
      "mode": "subagent",
      "description": "Code analyzer without edits",
      "prompt": "Analyze code and suggest improvements.",
      "permission": {
        "edit": "deny",
        "bash": "ask"
      }
    }
  }
}
```

### With MCP Server

```json
{
  "$schema": "https://opencode.ai/config.json",
  "agent": {
    "manager": {
      "mode": "primary",
      "description": "Manager with MCP tools",
      "prompt": "Use available MCP tools to manage projects."
    }
  },
  "mcpServers": {
    "my-mcp": {
      "url": "http://localhost:3000"
    }
  }
}
```

### Restricted Bash Commands

```json
{
  "agent": {
    "safe-agent": {
      "mode": "primary",
      "permission": {
        "bash": {
          "*": "ask",
          "git status*": "allow",
          "git log*": "allow",
          "grep *": "allow"
        }
      }
    }
  }
}
```

## Deprecated Fields

- `tools`: Use `permission` instead
- `maxSteps`: Use `steps` instead

## Provider-Specific Options

Any additional fields are passed directly to the provider:

```json
{
  "agent": {
    "deep-thinker": {
      "model": "openai/gpt-5",
      "reasoningEffort": "high",
      "textVerbosity": "low"
    }
  }
}
```

## File References

Use `{file:./path}` syntax to load prompts from files:

```json
{
  "agent": {
    "review": {
      "prompt": "{file:./prompts/review.txt}"
    }
  }
}
```

Path is relative to the config file location.

## Resources

- Docs: https://opencode.ai/docs/agents
- GitHub: https://github.com/anomalyco/opencode
- Config Schema: https://opencode.ai/config.json
