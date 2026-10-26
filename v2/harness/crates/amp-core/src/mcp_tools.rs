//! AMP's MCP tools as native rpi agent tools (D2, D7): the `McpTool` → `AgentTool` adapter.
//!
//! Every tool in the server's `tools/list` becomes one [`McpTool`], named exactly as AMP names it
//! (`amp_get_task`, never `amp_amp_get_task`), with AMP's description and JSON input schema passed
//! straight through. All of them share one authenticated [`AmpMcpClient`].
//!
//! What the model sees:
//! - a successful call: the tool's text content (AMP returns its JSON as one text block);
//! - a result AMP flagged `isError`, or a call that did not complete (the 120 s per-call timeout,
//!   transport, auth): a tool error with the reason, never a hang;
//! - a cancelled run: the call is dropped (rmcp then sends `notifications/cancelled`).
//!
//! In all of these, AMP's web UI links (a relative `approve_url`, a refusal's `/project/N/plan`, a
//! page on the MCP origin) are pointed at the web UI first ([`crate::links`], #2205), so the model
//! never builds a link from the MCP URL.
//!
//! Tools AMP marks `readOnlyHint` are registered with [`ToolReplay::Safe`], so crash recovery may
//! re-run them; everything else stays `Never` (rpi's default).

use std::sync::Arc;

use amp_mcp::{
    AmpMcpClient, Catalog, McpError,
    rmcp::model::{CallToolResult, ContentBlock, JsonObject},
};
use rpi_agent::{AgentError, AgentTool, AgentToolResult, ToolResultPartial};
use rpi_ai::types::Tool;
use rpi_harness::types::{HarnessTool, ToolReplay};
use serde_json::Value;
use tokio_util::sync::CancellationToken;

use crate::{
    links::{web_links, web_links_json},
    tools::result_text,
};

/// One AMP tool, called through the shared MCP client.
pub struct McpTool {
    schema: Tool,
    label: String,
    read_only: bool,
    client: Arc<AmpMcpClient>,
}

impl std::fmt::Debug for McpTool {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("McpTool")
            .field("name", &self.schema.name)
            .field("read_only", &self.read_only)
            .finish_non_exhaustive()
    }
}

impl McpTool {
    /// Adapts one `tools/list` entry. The name and input schema are AMP's, unchanged; the
    /// description is AMP's minus its "Use for: ..." line ([`without_use_for`]).
    pub fn new(tool: &amp_mcp::rmcp::model::Tool, client: Arc<AmpMcpClient>) -> Self {
        let name = tool.name.to_string();
        let description = tool
            .description
            .as_deref()
            .map(without_use_for)
            .unwrap_or_else(|| name.clone());
        let label = tool
            .title
            .clone()
            .or_else(|| tool.annotations.as_ref().and_then(|a| a.title.clone()))
            .unwrap_or_else(|| name.clone());
        let read_only = tool
            .annotations
            .as_ref()
            .and_then(|a| a.read_only_hint)
            .unwrap_or(false);
        Self {
            schema: Tool {
                name,
                description,
                parameters: Value::Object((*tool.input_schema).clone()).into(),
                constrained_sampling: None,
            },
            label,
            read_only,
            client,
        }
    }

    pub fn name(&self) -> &str {
        &self.schema.name
    }

    /// AMP marked the tool `readOnlyHint`.
    pub fn is_read_only(&self) -> bool {
        self.read_only
    }

    /// The tool with its replay policy, for `AgentHarnessOptions::tools`.
    pub fn into_harness_tool(self) -> HarnessTool {
        let replay = if self.read_only {
            ToolReplay::Safe
        } else {
            ToolReplay::Never
        };
        HarnessTool::new(Arc::new(self)).with_replay(replay)
    }
}

/// Every tool in `catalog`, adapted, in server order.
pub fn tools_from_catalog(catalog: &Catalog, client: &Arc<AmpMcpClient>) -> Vec<HarnessTool> {
    catalog
        .tools()
        .iter()
        .map(|t| McpTool::new(t, Arc::clone(client)).into_harness_tool())
        .collect()
}

/// Fetches the catalog (or uses the cached one) and adapts every tool.
pub async fn amp_tools(client: &Arc<AmpMcpClient>) -> Result<Vec<HarnessTool>, McpError> {
    let catalog = client.catalog().await?;
    Ok(tools_from_catalog(&catalog, client))
}

/// The text the model reads for a result: every text block; a non-text block is named, not dropped
/// silently.
pub fn model_text(result: &CallToolResult) -> String {
    let text = result_text(result);
    let others = result
        .content
        .iter()
        .filter(|c| c.as_text().is_none())
        .count();
    match (text.trim().is_empty(), others) {
        (_, 0) => text,
        (true, n) => format!("[{n} non-text content {}]", blocks(n)),
        (false, n) => format!("{text}\n[{n} non-text content {}]", blocks(n)),
    }
}

fn blocks(n: usize) -> &'static str {
    if n == 1 { "block" } else { "blocks" }
}

fn arguments(params: Value) -> Result<Option<JsonObject>, AgentError> {
    match params {
        Value::Object(map) => Ok(Some(map)),
        Value::Null => Ok(None),
        other => Err(AgentError::Validation(format!(
            "arguments must be a JSON object, not {other}"
        ))),
    }
}

#[async_trait::async_trait]
impl AgentTool for McpTool {
    fn schema(&self) -> &Tool {
        &self.schema
    }

    fn label(&self) -> &str {
        &self.label
    }

    async fn execute(
        &self,
        _tool_call_id: &str,
        params: Value,
        signal: CancellationToken,
        _on_update: Arc<dyn Fn(ToolResultPartial) + Send + Sync>,
    ) -> Result<AgentToolResult, AgentError> {
        let args = arguments(params)?;
        let call = self.client.call_tool(&self.schema.name, args);
        let result = tokio::select! {
            biased;
            _ = signal.cancelled() => return Err(AgentError::Abort),
            r = call => r,
        };
        let session = self.client.session();
        let (web, mcp) = (session.web_base(), session.mcp_origin());
        let links = |s: &str| web_links(s, &web, &mcp).into_owned();
        let result =
            result.map_err(|e| AgentError::Tool(links(&format!("{}: {e}", self.schema.name))))?;
        let text = links(&model_text(&result));
        if result.is_error == Some(true) {
            let message = if text.trim().is_empty() {
                "the tool reported an error".to_owned()
            } else {
                text.trim().to_owned()
            };
            return Err(AgentError::Tool(message));
        }
        let mut out = AgentToolResult::text(text);
        if let Some(structured) = &result.structured_content {
            out.details = structured.clone();
            web_links_json(&mut out.details, &web, &mcp);
        }
        Ok(out)
    }
}

/// A text-only result, for tests and callers that build results by hand.
pub fn text_result(text: &str) -> CallToolResult {
    CallToolResult::success(vec![ContentBlock::text(text.to_owned())])
}

/// AMP's description without its trailing "Use for: <phrases>." line (#2367). Those phrases are
/// for clients that search a deferred tool list by keyword (Claude Code's tool search); the harness
/// puts every tool in the prompt, where they repeat the description in every request (~1k tokens
/// over AMP's tools). A description without one is returned as it is.
pub fn without_use_for(description: &str) -> String {
    match description.rfind("Use for:") {
        // Only a last line: a "Use for:" with more text after it is part of the description.
        Some(at) if at > 0 && !description[at..].contains('\n') => {
            description[..at].trim_end().to_owned()
        }
        _ => description.to_owned(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn use_for_lines_are_dropped_from_descriptions() {
        assert_eq!(
            without_use_for("List tasks. By default yours. Use for: my tickets, the board."),
            "List tasks. By default yours."
        );
        assert_eq!(
            without_use_for("Read a doc.\nUse for: read a doc by path."),
            "Read a doc."
        );
        assert_eq!(without_use_for("No trailer."), "No trailer.");
        assert_eq!(
            without_use_for("Use for: everything."),
            "Use for: everything."
        );
        assert_eq!(
            without_use_for("Desc. Use for: x.\nMore text after it."),
            "Desc. Use for: x.\nMore text after it."
        );
    }

    #[test]
    fn model_text_names_non_text_blocks() {
        assert_eq!(model_text(&text_result(r#"{"a":1}"#)), r#"{"a":1}"#);
        let image = ContentBlock::image("aGk=", "image/png");
        let r = CallToolResult::success(vec![image.clone()]);
        assert_eq!(model_text(&r), "[1 non-text content block]");
        let r = CallToolResult::success(vec![ContentBlock::text("x"), image]);
        assert_eq!(model_text(&r), "x\n[1 non-text content block]");
    }

    #[test]
    fn arguments_must_be_an_object() {
        assert!(arguments(Value::Null).unwrap().is_none());
        assert_eq!(
            arguments(serde_json::json!({"a": 1})).unwrap().unwrap()["a"],
            1
        );
        assert!(arguments(serde_json::json!([1])).is_err());
    }
}
