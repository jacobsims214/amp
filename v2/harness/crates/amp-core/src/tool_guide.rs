//! Clearer built-in tools (#2363): sharper descriptions for `ls`, `read`, `find`, `grep` and
//! `bash`, and a precise error when a call's arguments belong to another tool.
//!
//! The user's first sessions on the default manager model (openrouter `xiaomi/mimo-v2.6-pro`) made
//! 7 schema errors in 21 calls: `ls {"path": {"command": "git status | head -20"}}` (bash's
//! arguments inside ls's `path`) and `ls {"path": {"project_id": 84, "owner": "any"}}`
//! (`amp_list_tasks`' arguments). rpi's error ("path: {...} is not of types null, string") names
//! the field but not the mistake, and the model sent the same call again three times.
//!
//! [`guide_tools`] wraps those five tools. The wrapper:
//! - appends one line to the description saying what the tool takes and which tool to use
//!   instead (ls/read/find/grep are not shells; bash is the shell; AMP tools are called by name);
//! - checks the raw arguments before rpi's schema validation ([`misrouted`]) and, when a
//!   string field holds an object, returns an error that names the tool the arguments were meant
//!   for and the exact call shape.
//!
//! Everything else (execution, labels, modes) is the inner tool's.

use std::sync::Arc;

use rpi_agent::{AgentError, AgentTool, AgentToolResult, ToolExecutionMode, ToolResultPartial};
use rpi_ai::types::Tool;
use rpi_harness::types::HarnessTool;
use serde_json::Value;
use tokio_util::sync::CancellationToken;

/// The extra description line per built-in tool.
fn extra_description(name: &str) -> Option<&'static str> {
    Some(match name {
        "ls" => {
            "Arguments: {\"path\": \"<directory>\"} (a plain string; omit it for the working \
             directory). It only lists files: it is not a shell. To run a command (git, pipes) \
             use the bash tool with {\"command\": \"...\"}; AMP tools (amp_*) are their own tools."
        }
        "read" => {
            "Arguments: {\"path\": \"<file>\"} (a plain string). To run a command use the bash \
             tool with {\"command\": \"...\"}."
        }
        "find" => {
            "Arguments: {\"pattern\": \"<glob>\", \"path\": \"<directory>\"} (plain strings). \
             It is not a shell: use the bash tool with {\"command\": \"...\"} for commands."
        }
        "grep" => {
            "Arguments: {\"pattern\": \"<regex>\", \"path\": \"<file or directory>\"} (plain \
             strings). It is not a shell: use the bash tool with {\"command\": \"...\"} for \
             commands."
        }
        "bash" => {
            "Arguments: {\"command\": \"<shell command>\"}. To list a directory the ls tool \
             ({\"path\": \"dir\"}) is simpler; AMP tools (amp_*) are called by their own names, \
             never through bash. Your role's tool policy may allow only some commands; a denied \
             command is reported as an error, so don't retry it unchanged."
        }
        _ => return None,
    })
}

/// When `args` for `tool` hold another tool's arguments, the error to return instead of rpi's
/// schema error. `None` when the arguments look like this tool's own (rpi validates the rest).
pub fn misrouted(tool: &str, args: &Value) -> Option<String> {
    let fields: &[&str] = match tool {
        "ls" | "read" => &["path"],
        "find" | "grep" => &["path", "pattern"],
        "bash" => &["command"],
        _ => return None,
    };
    let obj = args.as_object()?;
    for field in fields {
        let Some(v) = obj.get(*field) else { continue };
        if v.is_string() || v.is_null() {
            continue;
        }
        let inner = v.as_object();
        let has = |k: &str| inner.is_some_and(|o| o.contains_key(k));
        let shown = serde_json::to_string(v).unwrap_or_default();
        let shown: String = shown.chars().take(160).collect();
        let fix = if has("command") && tool != "bash" {
            let cmd = inner
                .and_then(|o| o.get("command"))
                .and_then(Value::as_str)
                .unwrap_or("...");
            let cmd: String = cmd.chars().take(120).collect();
            format!(
                "These are the bash tool's arguments. To run that command, call the bash tool: \
                 bash {{\"command\": {}}}.",
                serde_json::to_string(&cmd).unwrap_or_default()
            )
        } else if has("project_id") || has("task_id") || has("owner") || has("query") {
            "These look like an AMP tool's arguments (project_id, task_id, ...). Call that amp_* \
             tool by its own name with them, e.g. amp_list_tasks {\"project_id\": N}."
                .to_owned()
        } else if has("path") && tool != "bash" {
            format!("Pass the path itself as a string: {tool} {{\"{field}\": \"<path>\"}}.")
        } else {
            format!("`{field}` must be a plain string.")
        };
        let usage = match tool {
            "ls" => "ls {\"path\": \"<directory>\"}",
            "read" => "read {\"path\": \"<file>\"}",
            "find" => "find {\"pattern\": \"<glob>\", \"path\": \"<directory>\"}",
            "grep" => "grep {\"pattern\": \"<regex>\", \"path\": \"<file or directory>\"}",
            _ => "bash {\"command\": \"<shell command>\"}",
        };
        return Some(format!(
            "Wrong arguments for {tool}: `{field}` is {shown}, not a string. {fix} The {tool} \
             tool is called as {usage}. Don't send this call again."
        ));
    }
    None
}

struct Guided {
    inner: Arc<dyn AgentTool>,
    schema: Tool,
}

#[async_trait::async_trait]
impl AgentTool for Guided {
    fn schema(&self) -> &Tool {
        &self.schema
    }

    fn label(&self) -> &str {
        self.inner.label()
    }

    fn execution_mode(&self) -> ToolExecutionMode {
        self.inner.execution_mode()
    }

    fn prepare_arguments(&self, args: Value) -> Result<Value, AgentError> {
        if let Some(message) = misrouted(&self.schema.name, &args) {
            return Err(AgentError::Validation(message));
        }
        self.inner.prepare_arguments(args)
    }

    async fn execute(
        &self,
        tool_call_id: &str,
        params: Value,
        signal: CancellationToken,
        on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, AgentError> {
        self.inner
            .execute(tool_call_id, params, signal, on_update)
            .await
    }
}

/// Wraps the built-in tools that have an [`extra_description`]; the others are returned as they
/// are.
pub fn guide_tools(tools: Vec<HarnessTool>) -> Vec<HarnessTool> {
    tools
        .into_iter()
        .map(|t| {
            let schema = t.tool.schema();
            let Some(extra) = extra_description(&schema.name) else {
                return t;
            };
            let mut schema = schema.clone();
            schema.description = format!("{} {extra}", schema.description.trim_end());
            HarnessTool {
                tool: Arc::new(Guided {
                    inner: t.tool,
                    schema,
                }),
                replay: t.replay,
            }
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn bash_arguments_in_ls_point_at_bash() {
        // The user's session (#2363), verbatim shape.
        let m = misrouted(
            "ls",
            &json!({"path": {"command": "git status 2>&1 | head -20; pwd"}}),
        )
        .unwrap();
        assert!(m.contains("call the bash tool"), "{m}");
        assert!(
            m.contains("bash {\"command\": \"git status 2>&1 | head -20; pwd\"}"),
            "{m}"
        );
        assert!(m.contains("ls {\"path\": \"<directory>\"}"), "{m}");
    }

    #[test]
    fn amp_arguments_in_ls_point_at_the_amp_tool() {
        let m = misrouted("ls", &json!({"path": {"project_id": 84, "owner": "any"}})).unwrap();
        assert!(m.contains("amp_* tool by its own name"), "{m}");
    }

    #[test]
    fn nested_paths_and_other_shapes_get_the_usage() {
        let m = misrouted("read", &json!({"path": {"path": "README.md"}})).unwrap();
        assert!(m.contains("read {\"path\": \"<path>\"}"), "{m}");
        let m = misrouted("bash", &json!({"command": ["ls", "-la"]})).unwrap();
        assert!(m.contains("bash {\"command\": \"<shell command>\"}"), "{m}");
        let m = misrouted("grep", &json!({"pattern": {"x": 1}})).unwrap();
        assert!(m.contains("`pattern` must be a plain string"), "{m}");
    }

    #[test]
    fn well_formed_calls_and_other_tools_pass_through() {
        for (tool, args) in [
            ("ls", json!({})),
            ("ls", json!({"path": "src"})),
            ("ls", json!({"path": null})),
            ("read", json!({"path": "a.md", "offset": 3})),
            ("bash", json!({"command": "git status"})),
            ("grep", json!({"pattern": "x", "path": "."})),
            ("amp_list_tasks", json!({"path": {"command": "x"}})),
        ] {
            assert_eq!(misrouted(tool, &args), None, "{tool} {args}");
        }
    }

    #[test]
    fn descriptions_name_the_shape_and_the_other_tool() {
        assert!(extra_description("ls").unwrap().contains("not a shell"));
        assert!(
            extra_description("bash")
                .unwrap()
                .contains("never through bash")
        );
        assert_eq!(extra_description("edit"), None);
    }
}
