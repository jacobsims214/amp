"""Generate the AMP harness's embedded agent and skill assets (v2/harness/assets) from the same
sources as the opencode and Claude Code plugins: v2/.opencode/agent/*.md and v2/.opencode/skills/*/.

    python3 build_harness_assets.py <v2 dir> <out dir> [--overrides FILE]

Design: project 2 KB decisions/amp-harness-architecture.md, D5. The output is committed and embedded
in the binary (amp-core, include_dir); CI regenerates it and fails on drift.

Host glue (opencode's `task` tool, the `amp_amp_` prefix note, `subagent_depth`, `.opencode/**`
edits, amp-agent-builder) is rewritten by the rules in harness_overrides.yaml. Every rule must
match exactly as many times as it says (default once), so a change to the source that a rule no
longer fits fails the build instead of shipping opencode wording silently. After the rules, the
`banned` patterns must not appear in any generated markdown.
"""
import hashlib
import json
import re
import shutil
import sys
from pathlib import Path

import yaml

sys.dont_write_bytecode = True  # no __pycache__ next to the scripts
sys.path.insert(0, str(Path(__file__).resolve().parent))
from build_opencode_plugin import front  # noqa: E402  (reuse the plugin's frontmatter parsing)

SKIP_NAMES = {"__pycache__", ".DS_Store"}


class RuleError(Exception):
    pass


def split_raw(text):
    """(frontmatter text including fences, body) — the frontmatter is kept as written."""
    m = re.match(r"^(---\n.*?\n---\n?)(.*)$", text, re.S)
    if not m:
        return "", text
    return m.group(1), m.group(2)


def paragraphs(body):
    return re.split(r"\n[ \t]*\n", body)


def apply_body_rule(text, rule, where):
    want = rule.get("count", 1)
    if "paragraph" in rule:
        paras = paragraphs(text)
        hits = [i for i, p in enumerate(paras) if rule["paragraph"] in p]
        if len(hits) != want:
            raise RuleError(f"{where}: paragraph {rule['paragraph']!r} matched {len(hits)} paragraphs, want {want}")
        out = []
        for i, p in enumerate(paras):
            if i in hits:
                if rule["with"].strip():
                    out.append(rule["with"].strip("\n"))
            else:
                out.append(p)
        return "\n\n".join(out)
    if "find" in rule:
        n = text.count(rule["find"])
        if n != want:
            raise RuleError(f"{where}: find {rule['find']!r} matched {n} times, want {want}")
        return text.replace(rule["find"], rule["replace"])
    if "regex" in rule:
        new, n = re.subn(rule["regex"], rule["replace"], text)
        if "count" in rule and n != want:
            raise RuleError(f"{where}: regex {rule['regex']!r} matched {n} times, want {want}")
        return new
    raise RuleError(f"{where}: rule needs paragraph, find or regex: {rule}")


def apply_frontmatter_rules(fm, spec, where):
    for line in spec.get("remove_lines", []):
        lines = fm.split("\n")
        hits = [i for i, l in enumerate(lines) if l == line]
        if len(hits) != 1:
            raise RuleError(f"{where}: frontmatter line {line!r} matched {len(hits)} times, want 1")
        del lines[hits[0]]
        fm = "\n".join(lines)
    return fm


def rewrite(text, rel, overrides, used):
    """Apply the per-file rules for `rel` (path relative to .opencode), then the global rules."""
    spec = overrides.get("files", {}).get(rel)
    fm, body = split_raw(text)
    if spec:
        used.add(rel)
        fm = apply_frontmatter_rules(fm, spec.get("frontmatter", {}), rel)
        for rule in spec.get("body", []):
            body = apply_body_rule(body, rule, rel)
    for rule in overrides.get("global", []):
        body = apply_body_rule(body, rule, rel)  # global regexes may match any number of times
    for pattern in overrides.get("banned", []):
        m = re.search(pattern, body)
        if m:
            line = body[: m.start()].count("\n") + 1
            raise RuleError(f"{rel}: banned host glue {pattern!r} left at body line {line}: {m.group(0)!r}; add a rule to harness_overrides.yaml")
    return fm + body.rstrip("\n") + "\n"


def iter_files(root):
    for p in sorted(root.rglob("*")):
        if p.is_file() and not (set(p.relative_to(root).parts) & SKIP_NAMES):
            yield p


def main(argv):
    if len(argv) < 2:
        sys.exit(__doc__)
    v2, out = Path(argv[0]).resolve(), Path(argv[1]).resolve()
    ov_path = Path(argv[3]) if len(argv) > 3 and argv[2] == "--overrides" else Path(__file__).resolve().parent / "harness_overrides.yaml"
    overrides = yaml.safe_load(ov_path.read_text()) or {}
    src = v2 / ".opencode"
    exclude = set(overrides.get("exclude_skills", []))
    used = set()

    if out.exists():
        shutil.rmtree(out)
    (out / "agents").mkdir(parents=True)
    (out / "skills").mkdir()

    digest = hashlib.sha256()

    def feed(label, data):
        digest.update(label.encode() + b"\0" + data + b"\0")

    feed("overrides", ov_path.read_bytes())

    agents = []
    for f in sorted((src / "agent").glob("*.md")):
        text = f.read_text()
        feed(f"agent/{f.name}", text.encode())
        generated = rewrite(text, f"agent/{f.name}", overrides, used)
        (out / "agents" / f.name).write_text(generated)
        fm, _ = front(generated)
        agents.append({"id": f.stem, "mode": fm.get("mode", "subagent"), "description": fm.get("description", "")})

    skills = []
    for d in sorted(p for p in (src / "skills").iterdir() if p.is_dir()):
        if d.name in exclude or not (d / "SKILL.md").exists():
            continue
        files = []
        for f in iter_files(d):
            rel = f.relative_to(d).as_posix()
            dest = out / "skills" / d.name / rel
            dest.parent.mkdir(parents=True, exist_ok=True)
            feed(f"skills/{d.name}/{rel}", f.read_bytes())
            if f.suffix == ".md":
                dest.write_text(rewrite(f.read_text(), f"skills/{d.name}/{rel}", overrides, used))
            else:
                shutil.copy2(f, dest)
                dest.chmod(f.stat().st_mode & 0o777)
            files.append(rel)
        fm, _ = front((out / "skills" / d.name / "SKILL.md").read_text())
        skills.append({"name": fm.get("name", d.name), "description": fm.get("description", ""), "files": files})

    unused = set(overrides.get("files", {})) - used
    if unused:
        raise RuleError(f"rules for files that were not generated: {sorted(unused)}")

    cfg_text = (v2 / "opencode.json").read_text()
    default_agent = json.loads(re.sub(r",(\s*[}\]])", r"\1", cfg_text)).get("default_agent")
    manifest = {
        "generated_by": "v2/scripts/build-harness-assets.sh — do not edit; edit v2/.opencode or v2/scripts/lib/harness_overrides.yaml",
        "source": "v2/.opencode",
        "source_sha256": digest.hexdigest(),
        "default_agent": default_agent,
        "excluded_skills": sorted(exclude),
        "agents": agents,
        "skills": skills,
    }
    (out / "manifest.json").write_text(json.dumps(manifest, indent=1, ensure_ascii=False) + "\n")
    print(f"  agents: {len(agents)}  skills: {len(skills)}  source_sha256: {digest.hexdigest()[:12]}")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except RuleError as e:
        sys.exit(f"build-harness-assets: {e}")
