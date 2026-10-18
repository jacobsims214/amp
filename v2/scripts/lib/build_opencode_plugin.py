"""Build AMP's opencode plugin (v2/plugins/opencode) from v2/.opencode and
v2/opencode.json — the same sources the Claude Code plugin is built from.

    python3 build_opencode_plugin.py <v2 dir> <out dir>
"""
import json
import re
import shutil
import sys
from pathlib import Path

import yaml

# opencode's permission keys in agent frontmatter → the v2 permission actions
# opencode resolves them to.
ACTION = {"bash": "shell", "task": "subagent"}


def front(text):
    m = re.match(r"^---\n(.*?)\n---\n?(.*)$", text, re.S)
    if not m:
        return {}, text
    return yaml.safe_load(m.group(1)) or {}, m.group(2)


def permissions(perm):
    out = []
    for key, value in (perm or {}).items():
        action = ACTION.get(key, key)
        if isinstance(value, dict):
            # Same order as the file: opencode builds the rules in this order.
            for resource, effect in value.items():
                out.append({"action": action, "resource": str(resource), "effect": effect})
        else:
            out.append({"action": action, "resource": "*", "effect": value})
    return out


def model_ref(fm, raw):
    spec = fm.get("model")
    if not spec or spec == "inherit":
        return None
    provider, _, model_id = spec.partition("/")
    ref = {"providerID": provider, "id": model_id}
    # Read the variant as written: YAML 1.1 turns `on`/`off` into booleans.
    m = re.search(r"^variant:\s*(\S+)\s*$", raw, re.M)
    if m:
        ref["variant"] = m.group(1).strip("'\"")
    return ref


def main(v2, out):
    v2, out = Path(v2), Path(out)
    cfg = json.loads((v2 / "opencode.json").read_text())
    if out.exists():
        shutil.rmtree(out)
    out.mkdir(parents=True)

    agents = []
    for f in sorted((v2 / ".opencode/agent").glob("*.md")):
        text = f.read_text()
        fm, body = front(text)
        agents.append({
            "id": f.stem, "name": f.stem, "description": fm.get("description", ""),
            "mode": fm.get("mode", "subagent"), "hidden": bool(fm.get("hidden", False)),
            "color": fm.get("color"), "steps": fm.get("steps"),
            "model": model_ref(fm, text.split("\n---", 1)[0]), "permissions": permissions(fm.get("permission")),
            "system": body.strip(), "default": f.stem == cfg.get("default_agent"),
        })
    (out / "agents.json").write_text(json.dumps(agents, indent=1))

    skills = []
    for d in sorted(p for p in (v2 / ".opencode/skills").iterdir() if p.is_dir()):
        src = d / "SKILL.md"
        if not src.exists():
            continue
        fm, body = front(src.read_text())
        shutil.copytree(d, out / "skills" / d.name)
        (out / "skills" / d.name / "SKILL.body.md").write_text(body.strip() + "\n")
        skills.append({"id": d.name, "name": fm.get("name", d.name), "description": fm.get("description", ""),
                       "path": f"skills/{d.name}/SKILL.md", "contentFile": f"skills/{d.name}/SKILL.body.md"})
    (out / "skills.json").write_text(json.dumps(skills, indent=1))

    models = {}
    for mid, m in (cfg.get("providers", {}).get("openrouter", {}).get("models") or {}).items():
        caps = {k: v for k, v in (m.get("capabilities") or {}).items() if k in ("tools", "input", "output")}
        models[mid] = {"capabilities": caps, "variants": m.get("variants", [])}
    (out / "models.json").write_text(json.dumps(models, indent=1))

    shutil.copy(v2 / "scripts/lib/opencode_plugin/server.js", out / "server.js")
    version = json.loads((v2 / "plugins/amp/.claude-plugin/plugin.json").read_text())["version"]
    (out / "package.json").write_text(json.dumps({
        "name": "amp-opencode", "version": version, "type": "module",
        "description": "AMP for opencode: the manager/worker/reviewer agent team, skills, MCP servers and model settings in one plugin.",
        "main": "server.js", "exports": {".": "./server.js"},
        "files": ["server.js", "agents.json", "skills.json", "models.json", "skills", "README.md"],
        "peerDependencies": {"@opencode/plugin": ">=2.0.0"},
        "dependencies": {"zod": "^4.1.8"},
        "license": "Apache-2.0",
    }, indent=2) + "\n")
    print(f"  agents: {len(agents)}  skills: {len(skills)}  models: {len(models)}")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
