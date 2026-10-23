//! Hidden debug commands: `amp-harness mcp call <tool> [json]` and `mcp tools`; and trust for
//! repo MCP servers, `mcp trust` / `mcp untrust` (#2241, [`crate::mcp_trust_cmd`]).
//!
//! `mcp call` is what scripts (the M1-5 smoke test) use to create and clean up throwaway data.
//! Output: the tool's JSON result, pretty-printed (or its text when it is not JSON); `--raw` prints
//! the whole MCP `CallToolResult`. A result flagged `isError` goes to stderr with exit code 1.

use std::{io::Read, time::Duration};

use amp_core::tools::{parse_arguments, result_json, result_text};
use clap::{Args, Subcommand};

use crate::conn::{CliError, ConnArgs, connect_cli, print_json};

#[derive(Subcommand)]
pub enum McpCommand {
    /// Call one AMP tool with JSON arguments and print its result.
    Call(CallArgs),
    /// List the tools the server offers.
    Tools(ToolsArgs),
    /// Trust MCP servers from this repo's .amp/harness/config.toml as configured now (recorded
    /// outside the repo; a change asks again). No names: list every server and its trust.
    Trust(crate::mcp_trust_cmd::TrustArgs),
    /// Forget trusted repo MCP servers.
    Untrust(crate::mcp_trust_cmd::UntrustArgs),
}

#[derive(Args)]
pub struct CallArgs {
    #[command(flatten)]
    pub conn: ConnArgs,
    /// Tool name, e.g. amp_whoami.
    pub tool: String,
    /// Arguments as a JSON object (default `{}`); `-` reads them from stdin.
    pub arguments: Option<String>,
    /// Print the whole MCP CallToolResult instead of the tool's JSON.
    #[arg(long)]
    pub raw: bool,
    /// Per-call timeout in seconds.
    #[arg(long, default_value_t = amp_mcp::DEFAULT_CALL_TIMEOUT.as_secs())]
    pub timeout: u64,
}

#[derive(Args)]
pub struct ToolsArgs {
    #[command(flatten)]
    pub conn: ConnArgs,
    /// Print JSON (name, title, description per tool).
    #[arg(long)]
    pub json: bool,
}

pub async fn run(command: McpCommand) -> Result<(), CliError> {
    match command {
        McpCommand::Call(args) => call(args).await,
        McpCommand::Tools(args) => tools(args).await,
        McpCommand::Trust(args) => crate::mcp_trust_cmd::trust(args),
        McpCommand::Untrust(args) => crate::mcp_trust_cmd::untrust(args),
    }
}

async fn call(args: CallArgs) -> Result<(), CliError> {
    let raw_args = match args.arguments.as_deref() {
        Some("-") => {
            let mut buf = String::new();
            std::io::stdin()
                .read_to_string(&mut buf)
                .map_err(|e| CliError::new(format!("read arguments from stdin: {e}")))?;
            buf
        }
        Some(text) => text.to_owned(),
        None => String::new(),
    };
    let arguments = parse_arguments(&raw_args).map_err(CliError::usage)?;
    let settings = args.conn.settings()?;
    let client = connect_cli(&settings).await?;
    let result = client
        .call_tool_with_timeout(&args.tool, arguments, Duration::from_secs(args.timeout))
        .await;
    client.close().await;
    let result = result?;
    let failed = result.is_error == Some(true);
    if args.raw {
        print_json(&result)?;
    } else if failed {
        eprintln!("{}: {}", args.tool, result_text(&result).trim());
    } else if let Some(value) = result_json(&result) {
        print_json(&value)?;
    } else {
        println!("{}", result_text(&result));
    }
    if failed {
        return Err(CliError::silent(1));
    }
    Ok(())
}

#[derive(serde::Serialize)]
struct ToolInfo {
    name: String,
    title: Option<String>,
    description: Option<String>,
}

async fn tools(args: ToolsArgs) -> Result<(), CliError> {
    let settings = args.conn.settings()?;
    let client = connect_cli(&settings).await?;
    let catalog = client.catalog().await;
    client.close().await;
    let catalog = catalog?;
    let mut tools: Vec<ToolInfo> = catalog
        .tools()
        .iter()
        .map(|t| ToolInfo {
            name: t.name.to_string(),
            title: t.title.clone(),
            description: t.description.as_ref().map(|d| d.to_string()),
        })
        .collect();
    tools.sort_by(|a, b| a.name.cmp(&b.name));
    if args.json {
        return print_json(&tools);
    }
    for t in &tools {
        match &t.title {
            Some(title) => println!("{}  ({title})", t.name),
            None => println!("{}", t.name),
        }
    }
    eprintln!("{} tools", tools.len());
    Ok(())
}
