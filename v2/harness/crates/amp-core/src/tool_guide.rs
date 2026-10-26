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
//! Every built-in tool's parameter schema is also compacted ([`compact_schema`], #2367): rpi's
//! schemars output carries `$schema`, `title`, Rust-facing descriptions ("Mirrors TS
//! `ReadToolInput`") and `"default": null` on every optional field, all sent with every request
//! and none of it telling the model anything.
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

/// A built-in tool's parameter schema without what only Rust needs: `$schema`, the top-level
/// `title` and `description` (the tool's own description says it), `title` in definitions, and
/// `"default": null` (an optional field's absence already means that). Validation is unchanged:
/// none of these keywords constrains a value.
pub fn compact_schema(schema: &Value) -> Value {
    fn strip(v: &mut Value, top: bool) {
        match v {
            Value::Object(map) => {
                map.remove("$schema");
                map.remove("title");
                if top {
                    map.remove("description");
                }
                if map.get("default").is_some_and(Value::is_null) {
                    map.remove("default");
                }
                for (key, child) in map.iter_mut() {
                    // A property named "title" or "default" is a field, not a keyword.
                    if key == "properties" || key == "definitions" || key == "$defs" {
                        if let Value::Object(fields) = child {
                            for field in fields.values_mut() {
                                strip(field, false);
                            }
                        }
                    } else {
                        strip(child, false);
                    }
                }
            }
            Value::Array(items) => items.iter_mut().for_each(|i| strip(i, false)),
            _ => {}
        }
    }
    let mut out = schema.clone();
    strip(&mut out, true);
    if let Some(defs) = out.get_mut("definitions").and_then(Value::as_object_mut) {
        for def in defs.values_mut() {
            if let Some(d) = def.as_object_mut() {
                // "JSON-schema-facing edit shape (camelCase to match the TS wire contract)."
                d.remove("description");
            }
        }
    }
    out
}

/// Wraps every built-in tool: a compacted schema ([`compact_schema`]) and, where there is one,
/// the [`extra_description`].
pub fn guide_tools(tools: Vec<HarnessTool>) -> Vec<HarnessTool> {
    tools
        .into_iter()
        .map(|t| {
            let mut schema = t.tool.schema().clone();
            if let Some(extra) = extra_description(&schema.name) {
                schema.description = format!("{} {extra}", schema.description.trim_end());
            }
            schema.parameters = rpi_ai::types::Schema(compact_schema(&schema.parameters.0));
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
    fn compact_schema_drops_rust_noise_and_keeps_constraints() {
        let raw = json!({
            "$schema": "http://json-schema.org/draft-07/schema#",
            "title": "EditInput",
            "description": "Input for the edit tool. Mirrors TS `EditToolInput`.",
            "type": "object",
            "required": ["edits", "path"],
            "properties": {
                "path": {"description": "Path to the file.", "type": "string"},
                "title": {"description": "A field named title.", "type": "string", "default": null},
                "limit": {"default": null, "type": ["integer", "null"], "minimum": 0.0},
                "edits": {"type": "array", "items": {"$ref": "#/definitions/E"}}
            },
            "definitions": {"E": {
                "description": "JSON-schema-facing edit shape.",
                "type": "object",
                "required": ["oldText"],
                "properties": {"oldText": {"description": "Exact text.", "type": "string"}}
            }}
        });
        let out = compact_schema(&raw);
        assert_eq!(
            out,
            json!({
                "type": "object",
                "required": ["edits", "path"],
                "properties": {
                    "path": {"description": "Path to the file.", "type": "string"},
                    "title": {"description": "A field named title.", "type": "string"},
                    "limit": {"type": ["integer", "null"], "minimum": 0.0},
                    "edits": {"type": "array", "items": {"$ref": "#/definitions/E"}}
                },
                "definitions": {"E": {
                    "type": "object",
                    "required": ["oldText"],
                    "properties": {"oldText": {"description": "Exact text.", "type": "string"}}
                }}
            })
        );
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
