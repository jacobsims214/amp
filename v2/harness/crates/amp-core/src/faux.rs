//! Scripted model turns for the faux provider, loaded from a JSON file, so the binary can run
//! offline and deterministically (`run -p` tests, the smoke test, M2-8).
//!
//! `AMP_HARNESS_FAUX_SCRIPT=<file>` loads the file into every `api = "faux"` provider the session
//! has. The file is either an array of steps (every faux provider gets a copy) or an object mapping
//! provider id to its steps. One step is one model response:
//!
//! ```json
//! [
//!   {"tool_calls": [{"name": "amp_whoami", "arguments": {}}]},
//!   {"text": "You are {{last_tool_result}}"}
//! ]
//! ```
//!
//! - `text`: assistant text. With `tool_calls` it comes before the calls.
//! - `tool_calls`: `[{name, arguments}]`; the response stops for tool use. `arguments` defaults to
//!   `{}`; `id` defaults to a generated one.
//! - `thinking`: an optional thinking block before everything else.
//!
//! Placeholders in `text`, filled at call time from what the model was sent:
//! - `{{last_tool_result}}`: the text of the newest tool result, without the clock stamp;
//! - `{{last_user_message}}`: the text of the newest user message, without the clock stamp (AMP
//!   #2837: what the turn was really sent, e.g. the worker summaries the chat puts on top);
//! - `{{system_prompt}}`: the whole system prompt;
//! - `{{current_time}}`: the newest clock stamp the request carried (#2250,
//!   [`crate::request_time`]), or `none`;
//! - `{{context:KEY}}`: the value of the system prompt's `<amp_context>` line `KEY: …` (#2383,
//!   e.g. `{{context:project_id}}`), or `none`.
//!
//! `AMP_HARNESS_FAUX_TOKENS_PER_SECOND=<n>` streams every faux response at about n tokens a second
//! (4 characters a token) instead of all at once, so a pty test can watch a long reply or tool
//! call arrive (#2384).
//!
//! When the script runs out, the faux provider answers with an error ("No more faux responses
//! queued"), so a script that is too short fails the run instead of hanging it.

use std::{collections::BTreeMap, path::Path};

use rpi_ai::{
    providers::faux::{FauxBlock, FauxStep, faux_assistant_message},
    types::{Content, Context, Message, StopReason},
};
use serde::Deserialize;
use serde_json::Value;

use crate::providers::ProviderSet;

/// The environment variable naming the script file.
pub const ENV_FAUX_SCRIPT: &str = "AMP_HARNESS_FAUX_SCRIPT";

/// The environment variable that slows the faux providers' streaming down (#2384).
pub const ENV_FAUX_TOKENS_PER_SECOND: &str = "AMP_HARNESS_FAUX_TOKENS_PER_SECOND";

/// The faux providers' streaming speed from [`ENV_FAUX_TOKENS_PER_SECOND`]: `None` (all at once)
/// unless it is a positive number.
pub fn tokens_per_second(env: &dyn Fn(&str) -> Option<String>) -> Option<f64> {
    env(ENV_FAUX_TOKENS_PER_SECOND)
        .and_then(|v| v.trim().parse::<f64>().ok())
        .filter(|tps| tps.is_finite() && *tps > 0.0)
}

/// One scripted response.
#[derive(Debug, Clone, Default, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct StepSpec {
    #[serde(default)]
    pub thinking: Option<String>,
    #[serde(default)]
    pub text: Option<String>,
    #[serde(default)]
    pub tool_calls: Vec<ToolCallSpec>,
}

#[derive(Debug, Clone, PartialEq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ToolCallSpec {
    pub name: String,
    #[serde(default = "empty_object")]
    pub arguments: Value,
    #[serde(default)]
    pub id: Option<String>,
}

fn empty_object() -> Value {
    Value::Object(Default::default())
}

/// A parsed script file.
#[derive(Debug, Clone, PartialEq)]
pub enum ScriptFile {
    /// Every faux provider gets these steps.
    All(Vec<StepSpec>),
    /// Steps per provider id.
    PerProvider(BTreeMap<String, Vec<StepSpec>>),
}

#[derive(Deserialize)]
#[serde(untagged)]
enum RawScript {
    All(Vec<StepSpec>),
    PerProvider(BTreeMap<String, Vec<StepSpec>>),
}

/// A script that could not be loaded or applied.
#[derive(Debug, thiserror::Error)]
#[error("faux script {path}: {message}")]
pub struct FauxScriptError {
    pub path: String,
    pub message: String,
}

impl ScriptFile {
    pub fn parse(text: &str, origin: &str) -> Result<Self, FauxScriptError> {
        let raw: RawScript = serde_json::from_str(text).map_err(|e| FauxScriptError {
            path: origin.to_owned(),
            message: format!(
                "expected an array of steps or an object of provider id -> steps ({e})"
            ),
        })?;
        Ok(match raw {
            RawScript::All(steps) => Self::All(steps),
            RawScript::PerProvider(map) => Self::PerProvider(map),
        })
    }

    pub fn load(path: &Path) -> Result<Self, FauxScriptError> {
        let text = std::fs::read_to_string(path).map_err(|e| FauxScriptError {
            path: path.display().to_string(),
            message: e.to_string(),
        })?;
        Self::parse(&text, &path.display().to_string())
    }

    /// Loads the file named by [`ENV_FAUX_SCRIPT`], if it is set.
    pub fn from_env(env: &dyn Fn(&str) -> Option<String>) -> Result<Option<Self>, FauxScriptError> {
        match env(ENV_FAUX_SCRIPT).filter(|v| !v.trim().is_empty()) {
            Some(path) => Self::load(Path::new(&path)).map(Some),
            None => Ok(None),
        }
    }

    /// Queues the steps on the faux providers in `set`. Returns the provider ids scripted. Naming a
    /// provider that is not a faux provider in `set` is an error.
    pub fn apply(&self, set: &ProviderSet) -> Result<Vec<String>, String> {
        let mut done = Vec::new();
        match self {
            Self::All(steps) => {
                for id in set.faux_ids() {
                    if let Some(script) = set.faux_script(&id) {
                        script.set_responses(steps.iter().map(StepSpec::to_step).collect());
                        done.push(id);
                    }
                }
            }
            Self::PerProvider(map) => {
                for (id, steps) in map {
                    let script = set
                        .faux_script(id)
                        .ok_or_else(|| format!("`{id}` is not an available faux provider"))?;
                    script.set_responses(steps.iter().map(StepSpec::to_step).collect());
                    done.push(id.clone());
                }
            }
        }
        Ok(done)
    }
}

impl StepSpec {
    /// The step as a faux response. Text with placeholders becomes a factory evaluated per call.
    pub fn to_step(&self) -> FauxStep {
        let spec = self.clone();
        let stop = if spec.tool_calls.is_empty() {
            StopReason::Stop
        } else {
            StopReason::ToolUse
        };
        let has_placeholder = spec.text.as_deref().is_some_and(|t| {
            [
                "{{last_tool_result}}",
                "{{last_user_message}}",
                "{{system_prompt}}",
                "{{current_time}}",
                "{{context:",
            ]
            .iter()
            .any(|p| t.contains(p))
        });
        if !has_placeholder {
            return FauxStep::message(faux_assistant_message(spec.blocks(None), stop));
        }
        FauxStep::factory(move |ctx: &Context| faux_assistant_message(spec.blocks(Some(ctx)), stop))
    }

    fn blocks(&self, ctx: Option<&Context>) -> Vec<FauxBlock> {
        let mut blocks = Vec::new();
        if let Some(t) = &self.thinking {
            blocks.push(FauxBlock::thinking(t.clone()));
        }
        if let Some(t) = &self.text {
            blocks.push(FauxBlock::text(match ctx {
                Some(ctx) => fill(t, ctx),
                None => t.clone(),
            }));
        }
        for call in &self.tool_calls {
            let mut block = FauxBlock::tool_call(call.name.clone(), call.arguments.clone());
            if let (Some(id), FauxBlock::ToolCall { id: slot, .. }) = (&call.id, &mut block) {
                *slot = id.clone();
            }
            blocks.push(block);
        }
        blocks
    }
}

fn texts(content: &[Content]) -> String {
    content
        .iter()
        .filter_map(|c| match c {
            Content::Text(t) => Some(t.text.as_str()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("\n")
}

fn fill(template: &str, ctx: &Context) -> String {
    let last_tool_result = ctx
        .messages
        .iter()
        .rev()
        .find_map(|m| match m {
            Message::ToolResult(r) => Some(crate::request_time::strip(&texts(&r.content))),
            _ => None,
        })
        .unwrap_or_default();
    let last_user_message = ctx
        .messages
        .iter()
        .rev()
        .find_map(|m| match m {
            Message::User(u) => Some(crate::request_time::strip(&match &u.content {
                rpi_ai::types::UserContent::Text(t) => t.clone(),
                rpi_ai::types::UserContent::Blocks(b) => texts(b),
            })),
            _ => None,
        })
        .unwrap_or_default();
    let current_time = ctx
        .messages
        .iter()
        .rev()
        .find_map(|m| {
            let text = match m {
                Message::ToolResult(r) => texts(&r.content),
                Message::User(u) => match &u.content {
                    rpi_ai::types::UserContent::Text(t) => t.clone(),
                    rpi_ai::types::UserContent::Blocks(b) => texts(b),
                },
                Message::Assistant(_) => return None,
            };
            crate::request_time::last_stamp(&text).map(str::to_owned)
        })
        .unwrap_or_else(|| "none".to_owned());
    let system = ctx.system_prompt.as_deref().unwrap_or_default();
    let filled = template
        .replace("{{last_tool_result}}", &last_tool_result)
        .replace("{{last_user_message}}", &last_user_message)
        .replace("{{current_time}}", &current_time);
    fill_context(&filled, system).replace("{{system_prompt}}", system)
}

/// Fills each `{{context:KEY}}` in `text` with the value of the `KEY: …` line in `system`'s
/// `<amp_context>` block, or `none`.
fn fill_context(text: &str, system: &str) -> String {
    const OPEN: &str = "{{context:";
    let block = system
        .find("<amp_context>")
        .map(|at| {
            let rest = &system[at..];
            &rest[..rest.find("</amp_context>").unwrap_or(rest.len())]
        })
        .unwrap_or_default();
    let mut out = String::new();
    let mut rest = text;
    while let Some(at) = rest.find(OPEN) {
        let Some(len) = rest[at + OPEN.len()..].find("}}") else {
            break;
        };
        let key = &rest[at + OPEN.len()..at + OPEN.len() + len];
        out.push_str(&rest[..at]);
        let prefix = format!("{key}: ");
        out.push_str(
            block
                .lines()
                .find_map(|l| l.strip_prefix(&prefix))
                .unwrap_or("none"),
        );
        rest = &rest[at + OPEN.len() + len + 2..];
    }
    out.push_str(rest);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tokens_per_second_needs_a_positive_number() {
        let with = |v: &'static str| {
            move |k: &str| (k == ENV_FAUX_TOKENS_PER_SECOND).then(|| v.to_owned())
        };
        assert_eq!(tokens_per_second(&with("400")), Some(400.0));
        assert_eq!(tokens_per_second(&with(" 2.5 ")), Some(2.5));
        for bad in ["", "0", "-1", "fast", "NaN", "inf"] {
            assert_eq!(tokens_per_second(&with(bad)), None, "{bad}");
        }
        assert_eq!(tokens_per_second(&|_| None), None);
    }

    #[test]
    fn parses_both_shapes_and_rejects_unknown_keys() {
        let all = ScriptFile::parse(
            r#"[{"tool_calls":[{"name":"amp_whoami"}]},{"text":"done"}]"#,
            "t",
        )
        .unwrap();
        let ScriptFile::All(steps) = &all else {
            panic!("{all:?}")
        };
        assert_eq!(steps[0].tool_calls[0].arguments, serde_json::json!({}));
        assert_eq!(steps[1].text.as_deref(), Some("done"));

        let per = ScriptFile::parse(r#"{"faux":[{"text":"x"}]}"#, "t").unwrap();
        assert!(matches!(per, ScriptFile::PerProvider(m) if m["faux"].len() == 1));

        let e = ScriptFile::parse(r#"[{"txt":"typo"}]"#, "s.json").unwrap_err();
        assert!(e.to_string().starts_with("faux script s.json:"), "{e}");
    }

    #[test]
    fn placeholders_are_filled_from_the_context() {
        let step = StepSpec {
            text: Some("got {{last_tool_result}} / {{system_prompt}}".into()),
            ..Default::default()
        };
        let FauxStep::Factory(f) = step.to_step() else {
            panic!("expected a factory")
        };
        let ctx = Context {
            system_prompt: Some("SYS".into()),
            messages: vec![Message::ToolResult(Box::new(
                rpi_ai::types::ToolResultMessage {
                    role: rpi_ai::types::ToolResultRole,
                    tool_call_id: "c1".into(),
                    tool_name: "amp_whoami".into(),
                    content: vec![Content::text("{\"user_id\":7}")],
                    details: None,
                    usage: None,
                    added_tool_names: Vec::new(),
                    is_error: false,
                    timestamp: 0,
                },
            ))],
            tools: Vec::new(),
        };
        let m = f(&ctx);
        assert_eq!(
            crate::events::assistant_text(&m),
            "got {\"user_id\":7} / SYS"
        );
    }

    #[test]
    fn context_placeholders_read_the_amp_context_lines() {
        let system = "role prompt\nproject_id: 1 (outside)\n<amp_context>\nrole: manager\n\
                      project_id: 159\nproject_name: Go API\n</amp_context>\nafter";
        assert_eq!(
            fill_context(
                "#{{context:project_id}} {{context:project_name}} {{context:nope}} {{context:x",
                system
            ),
            "#159 Go API none {{context:x"
        );
        assert_eq!(fill_context("{{context:project_id}}", "no block"), "none");
    }

    #[test]
    fn tool_call_steps_stop_for_tool_use_and_keep_ids() {
        let step = StepSpec {
            text: Some("calling".into()),
            tool_calls: vec![ToolCallSpec {
                name: "amp_whoami".into(),
                arguments: serde_json::json!({}),
                id: Some("call-1".into()),
            }],
            ..Default::default()
        };
        let FauxStep::Message(m) = step.to_step() else {
            panic!("expected a message")
        };
        assert_eq!(m.stop_reason, StopReason::ToolUse);
        assert!(matches!(&m.content[1], Content::ToolCall(tc) if tc.id == "call-1"));
    }
}
