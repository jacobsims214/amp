//! Calling AMP tools from harness code (not the model): the tool contract (D11) and JSON results.

use std::fmt;

use amp_mcp::{
    AmpMcpClient, Catalog, McpError,
    rmcp::model::{CallToolResult, JsonObject},
};
use serde::de::DeserializeOwned;
use serde_json::Value;

/// The AMP tools the harness hard-depends on (D11). Everything else is registered from
/// `tools/list`. `doctor` reports any of these the server does not list.
pub const CONTRACT_TOOLS: &[&str] = &[
    "amp_whoami",
    "amp_get_project",
    "amp_list_projects",
    "amp_list_tasks",
    "amp_get_task",
    "amp_get_plan",
    "amp_check_plan",
    "amp_dispatch_task",
    "amp_claim_task",
    "amp_complete_task",
    "amp_block_task",
    "amp_report_run",
    "amp_answer_question",
    "amp_list_artifacts",
    "amp_create_project",
    "amp_kb_write",
];

/// The contract tools missing from `catalog`, in contract order.
pub fn missing_contract_tools(catalog: &Catalog) -> Vec<&'static str> {
    CONTRACT_TOOLS
        .iter()
        .copied()
        .filter(|name| catalog.get(name).is_none())
        .collect()
}

/// A tool call that did not produce the JSON the caller wanted.
#[derive(Debug)]
pub enum ToolCallError {
    /// The call did not complete (transport, timeout, auth, or a JSON-RPC error from the server).
    Mcp(McpError),
    /// The tool ran and reported an error (`isError`); the text is the tool's message.
    Tool { tool: String, message: String },
    /// The tool answered with something that is not the expected JSON.
    BadResult { tool: String, reason: String },
}

impl fmt::Display for ToolCallError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Mcp(e) => e.fmt(f),
            Self::Tool { tool, message } => write!(f, "{tool}: {message}"),
            Self::BadResult { tool, reason } => write!(f, "{tool}: unexpected result: {reason}"),
        }
    }
}

impl std::error::Error for ToolCallError {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        match self {
            Self::Mcp(e) => Some(e),
            _ => None,
        }
    }
}

impl From<McpError> for ToolCallError {
    fn from(e: McpError) -> Self {
        Self::Mcp(e)
    }
}

impl ToolCallError {
    pub fn is_auth_required(&self) -> bool {
        matches!(self, Self::Mcp(e) if e.is_auth_required())
    }
}

/// All text blocks of a result, joined with newlines.
pub fn result_text(result: &CallToolResult) -> String {
    result
        .content
        .iter()
        .filter_map(|c| c.as_text().map(|t| t.text.as_str()))
        .collect::<Vec<_>>()
        .join("\n")
}

/// A result's JSON: `structuredContent` when present, else the text content parsed as JSON (AMP
/// returns its JSON as one text block). `None` when the text is not JSON.
pub fn result_json(result: &CallToolResult) -> Option<Value> {
    if let Some(structured) = &result.structured_content {
        return Some(structured.clone());
    }
    serde_json::from_str(result_text(result).trim()).ok()
}

/// Calls `tool` and returns its JSON result. A result flagged `isError` becomes
/// [`ToolCallError::Tool`].
pub async fn call_json(
    client: &AmpMcpClient,
    tool: &str,
    arguments: Value,
) -> Result<Value, ToolCallError> {
    let arguments = match arguments {
        Value::Object(map) => Some(map),
        Value::Null => None,
        other => {
            return Err(ToolCallError::BadResult {
                tool: tool.to_owned(),
                reason: format!("arguments must be a JSON object, not {other}"),
            });
        }
    };
    let result = client.call_tool(tool, arguments).await?;
    if result.is_error == Some(true) {
        let message = result_text(&result);
        return Err(ToolCallError::Tool {
            tool: tool.to_owned(),
            message: if message.trim().is_empty() {
                "the tool reported an error".to_owned()
            } else {
                message.trim().to_owned()
            },
        });
    }
    result_json(&result).ok_or_else(|| ToolCallError::BadResult {
        tool: tool.to_owned(),
        reason: "no JSON in the result".to_owned(),
    })
}

/// [`call_json`] deserialized into `T`.
pub async fn call_as<T: DeserializeOwned>(
    client: &AmpMcpClient,
    tool: &str,
    arguments: Value,
) -> Result<T, ToolCallError> {
    let value = call_json(client, tool, arguments).await?;
    serde_json::from_value(value).map_err(|e| ToolCallError::BadResult {
        tool: tool.to_owned(),
        reason: e.to_string(),
    })
}

/// Parses a JSON object given on the command line (`mcp call`). Empty input means `{}`.
pub fn parse_arguments(raw: &str) -> Result<Option<JsonObject>, String> {
    if raw.trim().is_empty() {
        return Ok(None);
    }
    match serde_json::from_str::<Value>(raw) {
        Ok(Value::Object(map)) => Ok(Some(map)),
        Ok(other) => Err(format!("arguments must be a JSON object, not {other}")),
        Err(e) => Err(format!("arguments are not valid JSON: {e}")),
    }
}

#[cfg(test)]
mod tests {
    use amp_mcp::rmcp::model::ContentBlock;

    use super::*;

    #[test]
    fn json_from_text_or_structured_content() {
        let r = CallToolResult::success(vec![ContentBlock::text(r#"{"a": 1}"#)]);
        assert_eq!(result_json(&r), Some(serde_json::json!({"a": 1})));
        let r = CallToolResult::success(vec![ContentBlock::text("plain words")]);
        assert_eq!(result_json(&r), None);
        assert_eq!(result_text(&r), "plain words");
        let mut r = CallToolResult::success(vec![ContentBlock::text("ignored")]);
        r.structured_content = Some(serde_json::json!({"b": 2}));
        assert_eq!(result_json(&r), Some(serde_json::json!({"b": 2})));
    }

    #[test]
    fn arguments_must_be_an_object() {
        assert_eq!(parse_arguments("").unwrap(), None);
        assert_eq!(parse_arguments(r#"{"x": 1}"#).unwrap().unwrap()["x"], 1);
        assert!(parse_arguments("[1]").is_err());
        assert!(parse_arguments("{").is_err());
    }

    #[test]
    fn contract_has_no_duplicates() {
        let mut names = CONTRACT_TOOLS.to_vec();
        names.sort_unstable();
        names.dedup();
        assert_eq!(names.len(), CONTRACT_TOOLS.len());
    }
}
