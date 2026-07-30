#!/usr/bin/env python3
"""Convert the AMP v2 opencode config into a Claude Code plugin.

Reads:
  v2/.opencode/agent/*.md      opencode agent definitions
  v2/.opencode/skills/*/       opencode skills
  v2/opencode.json             provider + mcp config

Writes (fully regenerated, safe to delete and rebuild):
  v2/plugins/amp/.claude-plugin/plugin.json
  v2/plugins/amp/.mcp.json
  v2/plugins/amp/agents/*.md
  v2/plugins/amp/commands/*.md
  v2/plugins/amp/skills/*/

The two configs describe the same system but disagree on vocabulary, so most of
this file is the translation table between them. See MAPPING NOTES below.

Usage: opencode_to_claude.py <v2-dir> <plugin-out-dir>
"""

from __future__ import annotations

import json
import re
import shutil
import sys
from pathlib import Path

# --------------------------------------------------------------------------
# MAPPING NOTES
#
# opencode                          Claude Code
# --------------------------------  ------------------------------------------
# mode: primary                     no equivalent — a "primary" agent is just
#                                   the main session, so it becomes a slash
#                                   command that adopts the protocol in-session
# mode: subagent                    agents/<name>.md
# hidden: true                      dropped (no equivalent)
# temperature                       dropped (no equivalent)
# model: openrouter/<vendor>/<m>    model tier — see MODEL_MAP
# permission: <k>: deny            disallowedTools: [...] — opencode is
#                                   deny-based, so an allowlist would be the
#                                   wrong shape (and would strip Skill and the
#                                   amp MCP surface nobody thought to list)
# permission with path globs        commands keep them as Edit(.claude/**) /
#                                   Bash(git status:*); plugin AGENTS cannot —
#                                   a `tools` entry drops its (...) specifier
#                                   and permissionMode is ignored for plugin
#                                   agents, so those become prose + a pointer
#                                   at .claude/settings.json
# steps: N                          maxTurns: N
# "load skill X first" in prose     skills: [X] (preloaded at spawn)
# skill("x")                        the Skill tool / **x** skill
# --------------------------------------------------------------------------

PLUGIN_NAME = "amp"
PLUGIN_VERSION = "2.0.0"
MCP_SERVER_NAME = "amp"

# openrouter model -> Claude Code model tier. Cheap/small models map to haiku,
# coding models to sonnet, the planner to opus.
MODEL_MAP = {
    "openrouter/deepseek/deepseek-v4-flash": "opus",
    "openrouter/qwen/qwen3-coder-next": "sonnet",
    "openrouter/qwen/qwen3.6-35b-a3b": "sonnet",
    "openrouter/qwen/qwen3.5-9b": "haiku",
}
DEFAULT_MODEL = "inherit"

COLOR_MAP = {
    "amp-manager": "blue",
    "amp-researcher": "cyan",
    "amp-reviewer": "yellow",
    "amp-kb-curator": "cyan",
    "amp-github-researcher": "cyan",
    "amp-github-reviewer": "yellow",
    "amp-worker-backend": "green",
    "amp-worker-frontend": "magenta",
    "amp-worker-docs": "green",
}

# Claude Code tools blocked by each opencode `permission: <key>: deny`.
# Context7 is the MCP form of web access, so it goes wherever WebFetch goes.
DENY_TOOLS = {
    "edit": ["Edit", "Write", "NotebookEdit"],
    "bash": ["Bash", "BashOutput", "KillShell"],
    "webfetch": [
        "WebFetch",
        "WebSearch",
        "mcp__context7__resolve-library-id",
        "mcp__context7__get-library-docs",
    ],
    "task": ["Task"],
    "todowrite": ["TodoWrite"],
}

# Agents whose opencode file is frontmatter-only — they leaned entirely on a
# skill for behaviour. Names the skill to point the synthesised prompt at.
BODY_FALLBACK_SKILLS = {
    "amp-github-reviewer": "github-pr-review",
    "amp-github-researcher": "github-research",
}

SKILL_EXCLUDES = {"__pycache__", ".DS_Store"}
SKILL_EXCLUDE_SUFFIXES = {".bak", ".pyc"}


# --------------------------------------------------------------------------
# frontmatter
# --------------------------------------------------------------------------

# Tolerates a closing `---` that sits at EOF with no trailing newline, which is
# how at least one of the opencode agent files is written.
FRONTMATTER_RE = re.compile(r"\A---\n(.*?)\n---[ \t]*(?:\n|\Z)", re.DOTALL)


def split_frontmatter(text: str) -> tuple[str, str]:
    """Return (frontmatter_yaml, body). Empty frontmatter if the file has none."""
    match = FRONTMATTER_RE.match(text)
    if not match:
        return "", text
    return match.group(1), text[match.end():]


def parse_frontmatter(fm: str) -> dict:
    """Minimal YAML subset parser: scalars, and dicts nested up to two levels.

    Hand-rolled so the build has no PyYAML dependency. It covers exactly the
    shapes the opencode agent files use and nothing more.
    """
    root: dict = {}
    stack: list[tuple[int, dict]] = [(-1, root)]
    for raw in fm.splitlines():
        if not raw.strip() or raw.lstrip().startswith("#"):
            continue
        indent = len(raw) - len(raw.lstrip())
        line = raw.strip()
        if ":" not in line:
            continue
        key, _, value = line.partition(":")
        key = key.strip().strip('"')
        value = value.strip()

        while stack and indent <= stack[-1][0]:
            stack.pop()
        parent = stack[-1][1] if stack else root

        if value == "":
            child: dict = {}
            parent[key] = child
            stack.append((indent, child))
        else:
            parent[key] = _scalar(value)
    return root


def _scalar(value: str):
    if value.lower() in ("true", "false"):
        return value.lower() == "true"
    if re.fullmatch(r"-?\d+", value):
        return int(value)
    if re.fullmatch(r"-?\d*\.\d+", value):
        return float(value)
    return value.strip('"').strip("'")


def dump_frontmatter(fields: dict) -> str:
    out = ["---"]
    for key, value in fields.items():
        if value is None or value == "" or value == []:
            continue
        if isinstance(value, list):
            out.append(f"{key}: [{', '.join(json.dumps(v) for v in value)}]")
        else:
            out.append(f"{key}: {value}")
    out.append("---")
    return "\n".join(out) + "\n"


# --------------------------------------------------------------------------
# body rewriting
# --------------------------------------------------------------------------

# Applied in order — the .opencode path rules must run before the bare-word rule.
BODY_RULES: list[tuple[str, str]] = [
    # skill loading
    (r'`?skill\("([a-z0-9-]+)"\)`?', r"the **\1** skill"),
    (r"`skill\(([^)]*)\)`", r"the Skill tool"),
    # paths
    (r'[`"]?\.opencode/agent/\*\.md[`"]?', "`${CLAUDE_PLUGIN_ROOT}/agents/*.md`"),
    (r'[`"]?\.opencode/skills/\*[`"]?', "`${CLAUDE_PLUGIN_ROOT}/skills/*`"),
    (r'[`"]?\.opencode/\*\*[`"]?', "`.claude/**`"),
    # Agents/skills the manager mints itself land in the project, not the plugin.
    (r"\.opencode/agent/", ".claude/agents/"),
    (r"\.opencode/skills/", ".claude/skills/"),
    (r'"?`?opencode\.json`?"?', "`.claude/settings.json`"),
    (r"mkdir -p \.opencode/\*", "mkdir -p .claude/*"),
    # tool vocabulary
    (r"`bash date`", "`date` via `Bash`"),
    (r"`edit`/write", "`Edit`/`Write`"),
    (r"`edit`", "`Edit`"),
    (r"`task` tool", "`Task` tool"),
    (r"`webfetch`", "`WebFetch`"),
    (r"`todowrite`", "`TodoWrite`"),
    (r"`bash`", "`Bash`"),
    (r"`read`", "`Read`"),
    (r"`glob`", "`Glob`"),
    (r"`grep`", "`Grep`"),
    (r"\bweb fetch\b", "WebFetch"),
    (r"\bwebfetch\b", "WebFetch"),
    # nested-delegation rationale differs by host but the rule is identical
    (
        r"opencode's\s*\n?\s*`?subagent_depth`? setting structurally prevents nested delegation",
        "Claude Code structurally prevents nested delegation (subagents cannot spawn subagents)",
    ),
    # anything left
    (r"\bopencode\b", "Claude Code"),
]

# opencode skill exports append a machine-generated footer; drop it.
FOOTER_RE = re.compile(
    r"\n*Base directory for this skill:.*?(?:<skill_files>.*?</skill_files>)?\s*$",
    re.DOTALL,
)


def rewrite_text(text: str) -> str:
    for pattern, replacement in BODY_RULES:
        text = re.sub(pattern, replacement, text)
    return text


def rewrite_body(body: str) -> str:
    return rewrite_text(FOOTER_RE.sub("\n", body)).strip() + "\n"


# --------------------------------------------------------------------------
# tools
# --------------------------------------------------------------------------

def allows(permission: dict, key: str) -> bool:
    """opencode permission value is 'allow'/'deny' or a glob->verdict dict."""
    value = permission.get(key)
    if value is None:
        return False
    if isinstance(value, dict):
        return any(v == "allow" for v in value.values())
    return value == "allow"


def denies(permission: dict, key: str) -> bool:
    """True when the key is a blanket deny (not a scoped allow, not absent)."""
    return permission.get(key) == "deny"


def scoped(permission: dict, key: str) -> bool:
    """True when the permission was path/command-scoped rather than blanket."""
    return isinstance(permission.get(key), dict)


def disallowed_for(permission: dict) -> list[str]:
    """Translate opencode denies into a Claude Code `disallowedTools` list.

    opencode's permission block is deny-based: it names the capabilities an
    agent must not have and leaves everything else alone. `disallowedTools` has
    exactly those semantics, so it is the honest mapping — and unlike a `tools`
    allowlist it does not silently strip tools nobody thought to enumerate
    (Skill, TodoWrite, the amp MCP surface), or freeze the list against tools
    added to Claude Code later.
    """
    tools: list[str] = []
    for key, blocked in DENY_TOOLS.items():
        if denies(permission, key):
            tools.extend(blocked)
    return tools


def scoped_allow_rules(permission: dict) -> list[str]:
    """Translate opencode's scoped globs into Claude Code rule syntax.

    `Edit(.claude/**)` / `Bash(git status:*)`. Only slash commands honour these
    specifiers — see the note in convert_agent() for why agents cannot.
    """
    rules: list[str] = []
    for tool, key in (("Edit", "edit"), ("Write", "edit"), ("Bash", "bash")):
        globs = permission.get(key)
        if not isinstance(globs, dict):
            continue
        for glob, verdict in globs.items():
            if verdict != "allow" or glob == "*":
                continue
            # rewrite_text maps .opencode paths to .claude ones but formats them
            # as markdown code spans; rule syntax wants the bare path.
            spec = rewrite_text(glob).strip().strip("`")
            if key == "bash":
                # opencode writes `git status*`; Claude Code prefix rules want
                # `git status:*`. A glob already ending in `/` keeps its slash.
                spec = re.sub(r"(?<![:/])\s*\*$", ":*", spec)
            rules.append(f"{tool}({spec})")
    return sorted(set(rules))


# The agent prompts open with a "Load the **x** skill ..." instruction naming
# the protocol skills they always need. Preloading those at spawn saves the
# agent a round trip; the optional stack skills stay on-demand because they are
# written as bullets rather than as a Load instruction.
PRELOAD_RE = re.compile(r"^Load the \*\*[a-z0-9-]+\*\* skill.*?(?=[—.]|\n\n)", re.MULTILINE | re.DOTALL)
SKILL_NAME_RE = re.compile(r"\*\*([a-z0-9-]+)\*\* skill")


def preloaded_skills(name: str, body: str) -> list[str]:
    """Skills to inject at spawn via the `skills:` frontmatter field."""
    if name in BODY_FALLBACK_SKILLS:
        return [BODY_FALLBACK_SKILLS[name], "amp-execution"]
    match = PRELOAD_RE.search(body)
    if not match:
        return []
    return list(dict.fromkeys(SKILL_NAME_RE.findall(match.group(0))))


# --------------------------------------------------------------------------
# conversion
# --------------------------------------------------------------------------

def convert_agent(path: Path) -> tuple[str, dict, str]:
    """Return (mode, claude_frontmatter, body) for one opencode agent file."""
    fm_text, body = split_frontmatter(path.read_text())
    fm = parse_frontmatter(fm_text)
    name = path.stem
    permission = fm.get("permission", {}) or {}

    # A silently unparsed header would produce a plausible-looking but wrong
    # agent (no description, no permissions), so fail the build instead.
    if not fm.get("description"):
        raise ValueError(f"{path.name}: no description in frontmatter — did the header parse?")

    fields = {
        "name": name,
        "description": rewrite_text(fm.get("description")),
        "model": MODEL_MAP.get(str(fm.get("model", "")), DEFAULT_MODEL),
        "color": COLOR_MAP.get(name),
    }
    disallowed = disallowed_for(permission)
    if disallowed:
        fields["disallowedTools"] = disallowed
    if isinstance(fm.get("steps"), int):
        fields["maxTurns"] = fm["steps"]

    body = rewrite_body(body)
    preload = preloaded_skills(name, body)
    if preload:
        fields["skills"] = preload
    if not body.strip():
        # Some opencode agents are frontmatter-only and got all their behaviour
        # from a skill. Claude Code needs a system prompt, so synthesise one.
        body = (
            f"# {name}\n\n"
            f"{fields['description']}\n\n"
            "You are dispatched by the AMP manager to do exactly one assigned task.\n\n"
            f"Load the **{BODY_FALLBACK_SKILLS.get(name, 'amp-execution')}** skill first — it "
            "defines the protocol for the work you were dispatched to do. Then follow the "
            "**amp-execution** skill for the ticket lifecycle: reading the ticket, logging "
            "progress as comments, writing findings to the AMP knowledge base, and completing "
            "the task. The **amp-index** skill lists everything else available.\n\n"
            "You cannot dispatch other subagents. Finish the task you were given.\n"
        )
    mode = str(fm.get("mode", "subagent"))
    # Commands express scoped rules in frontmatter, so only subagents — which
    # cannot — need the rule restated in the prompt.
    if mode != "primary" and (scoped(permission, "edit") or scoped(permission, "bash")):
        # Plugin agents get no scoped enforcement at all: `tools` entries drop
        # their `(...)` specifier (only the Agent tool reads one), and
        # `permissionMode` is explicitly ignored for plugin agents. State the
        # rule in the prompt and point at the settings file that can enforce it.
        rules = ", ".join(f"`{r}`" for r in scoped_allow_rules(permission))
        body += (
            "\n## Path restrictions\n\n"
            "opencode enforced these as `permission` globs. Claude Code has no per-agent "
            "equivalent, so treat them as hard rules you impose on yourself: your only "
            f"allowed write and shell operations are {rules}. Everything else is project "
            "work and belongs in a dispatched task.\n\n"
            "To enforce this rather than trust it, put the same rules in "
            "`.claude/settings.json` under `permissions.allow` with a matching "
            "`permissions.deny`.\n"
        )
    return mode, fields, body


def convert_skill(src: Path, dest: Path) -> None:
    dest.mkdir(parents=True, exist_ok=True)
    for item in sorted(src.rglob("*")):
        if any(part in SKILL_EXCLUDES for part in item.relative_to(src).parts):
            continue
        if item.suffix in SKILL_EXCLUDE_SUFFIXES:
            continue
        target = dest / item.relative_to(src)
        if item.is_dir():
            target.mkdir(parents=True, exist_ok=True)
        elif item.name == "SKILL.md":
            fm_text, body = split_frontmatter(item.read_text())
            fm = parse_frontmatter(fm_text)
            # In a plugin every skill is invokable as /amp:<name>, so the
            # opencode-only userInvokable flag has nothing to say.
            fields = {
                "name": fm.get("name", item.parent.name),
                "description": rewrite_text(fm.get("description", "")),
            }
            target.write_text(dump_frontmatter(fields) + "\n" + rewrite_body(body))
        elif item.suffix == ".md":
            target.write_text(rewrite_body(item.read_text()))
        else:
            shutil.copy2(item, target)


def convert_mcp(opencode_json: Path) -> dict:
    """opencode `mcp` block -> Claude Code `.mcp.json`."""
    cfg = json.loads(re.sub(r",(\s*[}\]])", r"\1", opencode_json.read_text()))
    servers: dict = {}
    for name, entry in cfg.get("mcp", {}).items():
        if entry.get("enabled") is False:
            continue
        if entry.get("type") == "remote" or "url" in entry:
            url = entry["url"]
            if name == MCP_SERVER_NAME:
                # Let operators point at a local server without editing the file.
                url = "${AMP_MCP_URL:-%s}" % url
            servers[name] = {"type": "http", "url": url}
        else:
            command = entry.get("command", [])
            servers[name] = {"command": command[0], "args": command[1:]}
    return {"mcpServers": servers}


def apply_overrides(overrides: Path, out: Path) -> None:
    """Copy hand-written replacements over the generated tree.

    A few sources document opencode's own schema (agent frontmatter fields,
    permission globs, model ids). Search-and-replace produces nonsense for
    those, so they are maintained by hand here and stamped on last.
    """
    if not overrides.is_dir():
        return
    for item in sorted(overrides.rglob("*")):
        if not item.is_file() or item.name == ".DS_Store":
            continue
        target = out / item.relative_to(overrides)
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(item, target)
        print(f"  override: {item.relative_to(overrides)}")


def manager_command(fields: dict, body: str, permission: dict) -> str:
    """Turn the `mode: primary` agent into a slash command for the main session.

    Commands are the one place Claude Code honours scoped rule syntax, so the
    manager's opencode permission globs survive the trip intact here — unlike
    the subagents, where they can only be prose.
    """
    allowed = scoped_allow_rules(permission)
    if allows(permission, "task"):
        allowed.append("Task")
    allowed.append(f"mcp__{MCP_SERVER_NAME}")
    header = dump_frontmatter({
        "description": fields["description"],
        "argument-hint": "[what you want planned]",
        "allowed-tools": ", ".join(allowed),
        "disallowed-tools": ", ".join(disallowed_for(permission)),
    })
    return (
        header
        + "\n"
        + "Adopt the AMP manager role below for the rest of this session. It replaces "
        + "your default working style: you plan and delegate, you do not implement.\n\n"
        + "The `allowed-tools` and `disallowed-tools` above carry over opencode's permission "
        + "block, so the web and planning-tool denials are enforced for real. The scoped write "
        + "and shell rules are a grant list, not a cap — nothing stops you reaching past them. "
        + "Treat them as a hard boundary anyway; the delegation model only works if you keep "
        + "your hands off the work.\n\n"
        + "If the user gave an argument, treat it as the work to plan: $ARGUMENTS\n\n"
        + "---\n"
        + body
    )


# --------------------------------------------------------------------------

def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    v2 = Path(sys.argv[1]).resolve()
    out = Path(sys.argv[2]).resolve()
    opencode = v2 / ".opencode"

    for required in (opencode / "agent", opencode / "skills", v2 / "opencode.json"):
        if not required.exists():
            print(f"missing source: {required}", file=sys.stderr)
            return 1

    if out.exists():
        shutil.rmtree(out)
    (out / ".claude-plugin").mkdir(parents=True)
    (out / "agents").mkdir()
    (out / "commands").mkdir()
    (out / "skills").mkdir()

    (out / ".claude-plugin" / "plugin.json").write_text(json.dumps({
        "name": PLUGIN_NAME,
        "version": PLUGIN_VERSION,
        "description": (
            "AMP v2 for Claude Code — a manager/worker/reviewer agent team that plans "
            "work onto the AMP board over MCP, dispatches specialists in review-gated "
            "waves, and accumulates project knowledge in the AMP knowledge base."
        ),
        "author": {"name": "AMP", "url": "https://amp.simstech.cloud"},
        "keywords": ["amp", "project-management", "agents", "mcp", "knowledge-base"],
    }, indent=2) + "\n")

    (out / ".mcp.json").write_text(json.dumps(convert_mcp(v2 / "opencode.json"), indent=2) + "\n")
    print("  .mcp.json written")

    for path in sorted((opencode / "agent").glob("*.md")):
        permission = parse_frontmatter(split_frontmatter(path.read_text())[0]).get("permission", {})
        mode, fields, body = convert_agent(path)
        if mode == "primary":
            command = manager_command(fields, body, permission or {})
            (out / "commands" / "manager.md").write_text(command)
            print(f"  command: /{PLUGIN_NAME}:manager  (from {path.name}, scoped rules kept)")
        else:
            (out / "agents" / path.name).write_text(dump_frontmatter(fields) + "\n" + body)
            denied = fields.get("disallowedTools") or []
            scope = f"-{len(denied)} tools" if denied else "unrestricted"
            turns = f", maxTurns {fields['maxTurns']}" if "maxTurns" in fields else ""
            print(f"  agent:   {fields['name']}  [{fields['model']}, {scope}{turns}]")

    for src in sorted((opencode / "skills").iterdir()):
        if not src.is_dir() or not (src / "SKILL.md").is_file():
            continue
        convert_skill(src, out / "skills" / src.name)
        print(f"  skill:   {src.name}")

    apply_overrides(Path(__file__).parent / "overrides", out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
