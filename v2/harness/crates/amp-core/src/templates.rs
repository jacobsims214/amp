//! Templates for new agents and skills (`amp-harness agents new` / `skills new`, #2234), and
//! comment-preserving edits of a file's YAML frontmatter.
//!
//! The agent templates carry the permission blocks of the shipped role they are named after
//! (`worker` = amp-worker-backend, `reviewer` = amp-reviewer, `researcher` = amp-researcher), its
//! preloaded skills, `based_on:` that role (so the new agent runs on its model until it gets a
//! `[roles.<id>]` of its own) and `dispatch: true` (so the manager may run it whatever its id).
//! `blank` has no `based_on`: it runs on `[models] fallback`.

use serde_json::Value;

use crate::{assets::parse_frontmatter, models::normalize_role};

/// The agent templates, by `--from` name: (name, the shipped role it copies, text).
pub const AGENT_TEMPLATES: &[(&str, Option<&str>, &str)] = &[
    (
        "worker",
        Some("amp-worker-backend"),
        include_str!("../templates/agents/worker.md"),
    ),
    (
        "reviewer",
        Some("amp-reviewer"),
        include_str!("../templates/agents/reviewer.md"),
    ),
    (
        "researcher",
        Some("amp-researcher"),
        include_str!("../templates/agents/researcher.md"),
    ),
    ("blank", None, include_str!("../templates/agents/blank.md")),
];

/// The default `--from` for `agents new`.
pub const DEFAULT_AGENT_TEMPLATE: &str = "worker";

/// The skill template (`skills new`).
pub const SKILL_TEMPLATE: &str = include_str!("../templates/skills/basic/SKILL.md");

/// The names `--from` accepts besides an existing agent id.
pub fn agent_template_names() -> Vec<&'static str> {
    AGENT_TEMPLATES.iter().map(|(n, _, _)| *n).collect()
}

/// The template called `name`, if there is one.
pub fn agent_template(name: &str) -> Option<&'static str> {
    AGENT_TEMPLATES
        .iter()
        .find(|(n, _, _)| *n == name)
        .map(|(_, _, text)| *text)
}

/// A YAML scalar for `text`: a JSON string, which YAML reads as a double-quoted scalar.
pub fn yaml_string(text: &str) -> String {
    Value::String(text.to_owned()).to_string()
}

/// A YAML flow list of strings (`["a", "b"]`).
pub fn yaml_list(items: &[String]) -> String {
    Value::Array(items.iter().cloned().map(Value::String).collect()).to_string()
}

/// `amp-sql-expert` -> `Amp Sql Expert`, for a template's heading.
fn title_of(id: &str) -> String {
    id.split('-')
        .filter(|w| !w.is_empty())
        .map(|w| {
            let mut c = w.chars();
            match c.next() {
                Some(first) => first.to_uppercase().chain(c).collect(),
                None => String::new(),
            }
        })
        .collect::<Vec<String>>()
        .join(" ")
}

fn render(template: &str, pairs: &[(&str, &str)]) -> String {
    let mut out = template.to_owned();
    for (key, value) in pairs {
        out = out.replace(&format!("{{{{{key}}}}}"), value);
    }
    out
}

/// The agent file for `id` from template `template` (one of [`AGENT_TEMPLATES`]).
pub fn render_agent(template: &str, id: &str, description: &str) -> Option<String> {
    let text = agent_template(template)?;
    Some(render(
        text,
        &[
            ("description_yaml", &yaml_string(description)),
            ("description", description),
            ("title", &title_of(id)),
            ("model_role", normalize_role(id)),
            ("id", id),
        ],
    ))
}

/// A new `SKILL.md` for skill `name`.
pub fn render_skill(name: &str, description: &str) -> String {
    render(
        SKILL_TEMPLATE,
        &[
            ("description_yaml", &yaml_string(description)),
            ("description", description),
            ("title", &title_of(name)),
            ("name", name),
        ],
    )
}

/// Sets (or, with `value: None`, removes) the top-level frontmatter key `key` of a markdown file,
/// line by line so comments and every other key stay as written. `value` is YAML (use
/// [`yaml_string`] / [`yaml_list`]). A key that isn't there is added before the closing `---`; a
/// file without frontmatter gets one. The result must parse and hold exactly `value` at `key`, or
/// this is an error and the caller writes nothing.
pub fn set_frontmatter_key(text: &str, key: &str, value: Option<&str>) -> Result<String, String> {
    let text = text.replace("\r\n", "\n");
    let (fm_lines, body) = match text.strip_prefix("---\n") {
        Some(rest) => {
            let (yaml, body) = if let Some(body) = rest.strip_prefix("---") {
                ("", body)
            } else {
                match rest.find("\n---") {
                    Some(end) => (&rest[..end], &rest[end + 4..]),
                    None => return Err("frontmatter has no closing `---`".into()),
                }
            };
            (
                yaml.lines().map(str::to_owned).collect::<Vec<_>>(),
                body.to_owned(),
            )
        }
        None => (Vec::new(), format!("\n\n{}", text.trim_start())),
    };
    let mut lines = fm_lines;
    let prefix = format!("{key}:");
    let start = lines
        .iter()
        .position(|l| l == &prefix || l.starts_with(&format!("{prefix} ")));
    let new_line = value.map(|v| format!("{key}: {v}"));
    match start {
        Some(start) => {
            // The key's value continues on indented lines and on `- ` items (a block list may sit
            // at column 0).
            let mut end = start + 1;
            while end < lines.len() {
                let l = &lines[end];
                if l.starts_with(' ') || l.starts_with('\t') || l.starts_with('-') {
                    end += 1;
                } else {
                    break;
                }
            }
            lines.drain(start..end);
            if let Some(line) = new_line {
                lines.insert(start, line);
            }
        }
        None => {
            if let Some(line) = new_line {
                lines.push(line);
            }
        }
    }
    let out = format!("---\n{}\n---{}", lines.join("\n"), body);
    let (fm, _) = parse_frontmatter(&out)?;
    let want = match value {
        Some(v) => {
            Some(yaml_serde::from_str::<Value>(v).map_err(|e| format!("`{key}`: not YAML: {e}"))?)
        }
        None => None,
    };
    if fm.get(key) != want.as_ref() {
        return Err(format!(
            "could not set `{key}` in the frontmatter without changing its meaning; edit the file by hand"
        ));
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn templates_render_and_parse() {
        for name in agent_template_names() {
            let text = render_agent(name, "sql-expert", "Writes SQL: \"fast\" ones").unwrap();
            let (fm, body) = parse_frontmatter(&text).unwrap();
            assert_eq!(fm["description"], "Writes SQL: \"fast\" ones", "{name}");
            assert_eq!(fm["mode"], "subagent", "{name}");
            assert_eq!(fm["dispatch"], true, "{name}");
            assert!(fm["permission"].is_object(), "{name}");
            assert!(body.starts_with("# Sql Expert"), "{name}: {body}");
            assert!(!text.contains("{{"), "{name}: {text}");
        }
        let (fm, _) = parse_frontmatter(&render_skill("my-skill", "Does: things")).unwrap();
        assert_eq!(fm["name"], "my-skill");
        assert_eq!(fm["description"], "Does: things");
    }

    #[test]
    fn frontmatter_keys_are_set_in_place() {
        let text =
            "---\n# keep me\ndescription: x\nskills:\n  - a\n  - b\nmode: subagent\n---\n\nBody\n";
        let out = set_frontmatter_key(text, "skills", Some(&yaml_list(&["c".into()]))).unwrap();
        assert_eq!(
            out,
            "---\n# keep me\ndescription: x\nskills: [\"c\"]\nmode: subagent\n---\n\nBody\n"
        );
        let out = set_frontmatter_key(&out, "dispatch", Some("true")).unwrap();
        assert!(out.contains("mode: subagent\ndispatch: true\n---"), "{out}");
        let out = set_frontmatter_key(&out, "skills", None).unwrap();
        assert!(!out.contains("skills"), "{out}");
        // Column-0 block lists are one value.
        let text = "---\nskills:\n- a\n- b\nmode: x\n---\nB";
        let out = set_frontmatter_key(text, "skills", Some("[]")).unwrap();
        assert_eq!(out, "---\nskills: []\nmode: x\n---\nB");
        // No frontmatter: one is added.
        let out = set_frontmatter_key("Body", "description", Some("\"d\"")).unwrap();
        let (fm, body) = parse_frontmatter(&out).unwrap();
        assert_eq!(fm["description"], "d");
        assert_eq!(body, "Body");
        assert!(set_frontmatter_key("---\na: 1\n", "a", Some("2")).is_err());
    }
}
