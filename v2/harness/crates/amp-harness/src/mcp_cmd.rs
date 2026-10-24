//! `amp-harness mcp …`: MCP servers (#2243, [`crate::mcp_crud`]): `list`, `get`, `add`,
//! `add-json`, `import` (#2244, [`crate::mcp_import`]), `edit`, `remove`, `enable`, `disable`,
//! `test`, `login`, `logout`, `secret`, and trust for repo servers (`trust` / `untrust`, #2241,
//! [`crate::mcp_trust_cmd`]).
//!
//! `mcp call <tool> [json]` and `mcp tools` talk to AMP's own MCP server by default (scripts such as
//! the M1-5 smoke test use them to create and clean up throwaway data); `--server <name>` points
//! them at a configured server instead. Output: the tool's JSON result, pretty-printed (or its text
//! when it is not JSON); `--raw` prints the whole MCP `CallToolResult`. A result flagged `isError`
//! goes to stderr with exit code 1.

use std::{io::Read, time::Duration};

use amp_core::tools::{parse_arguments, result_json, result_text};
use clap::{Args, Subcommand};

use crate::{
    conn::{CliError, ConnArgs, connect_cli, print_json},
    mcp_crud,
};

#[derive(Subcommand)]
pub enum McpCommand {
    /// List MCP servers (user and repo) with their last known health; starts nothing. `--health`
    /// connects to every enabled server now, in parallel, with a short timeout.
    #[command(visible_alias = "ls")]
    List(mcp_crud::ListArgs),
    /// Show one server: its config, where each secret comes from (never the value), trust, login
    /// and health.
    #[command(visible_alias = "show")]
    Get(mcp_crud::GetArgs),
    /// Add a server. stdio: `mcp add <name> -- <command> [args…]`; http/sse: `mcp add <name>
    /// <url>` or `--url URL [--transport http|sse]`. --header/--env for plain values,
    /// --secret-header/--secret-env for secrets (asked hidden, kept in the OS keychain). `--scope
    /// repo` writes <repo>/.amp/harness/config.toml and trusts it for you.
    Add(Box<mcp_crud::AddArgs>),
    /// Add a server from Claude Code / Cursor / VS Code JSON (`-` reads it from stdin).
    AddJson(mcp_crud::AddJsonArgs),
    /// Import servers from Claude Code, opencode, VS Code, Cursor or Claude Desktop (`--from`), with
    /// secrets moved to the OS keychain. `--dry-run` shows the TOML first. Never automatic.
    Import(Box<crate::mcp_import::ImportArgs>),
    /// Edit a server's table in $EDITOR (checked before it is saved; reopened on an error), or set
    /// its roles with --roles.
    Edit(mcp_crud::EditArgs),
    /// Remove a server, with its keychain secrets, its OAuth login and its repo trust.
    #[command(visible_alias = "rm")]
    Remove(mcp_crud::RemoveArgs),
    /// Turn a server on (in your config.toml, also for a repo server).
    Enable(mcp_crud::NameArgs),
    /// Turn a server off for you (a repo server is switched off in your config.toml, not the
    /// repo's).
    Disable(mcp_crud::NameArgs),
    /// Connect to a server now: status, timing, its tools, and its stderr when it fails.
    Test(mcp_crud::TestArgs),
    /// Log in to an http/sse server with OAuth in the browser (kept like the AMP login).
    Login(mcp_crud::LoginArgs),
    /// Forget a server's OAuth login.
    Logout(mcp_crud::NameArgs),
    /// Store (or replace) a server's secret values in the OS keychain, asked hidden.
    Secret(mcp_crud::SecretArgs),
    /// Trust MCP servers from this repo's .amp/harness/config.toml as configured now (recorded
    /// outside the repo; a change asks again). No names: list every server and its trust.
    Trust(crate::mcp_trust_cmd::TrustArgs),
    /// Forget trusted repo MCP servers.
    Untrust(crate::mcp_trust_cmd::UntrustArgs),
    /// Call one tool with JSON arguments and print its result (AMP's server, or --server NAME).
    Call(CallArgs),
    /// List the tools a server offers (AMP's, or --server NAME).
    Tools(ToolsArgs),
}

#[derive(Args)]
pub struct CallArgs {
    #[command(flatten)]
    pub conn: ConnArgs,
    /// A configured MCP server (`mcp list`) instead of AMP's.
    #[arg(long, value_name = "NAME")]
    pub server: Option<String>,
    /// Tool name, e.g. amp_whoami (or the server's own name for the tool with --server).
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
    /// A configured MCP server (`mcp list`) instead of AMP's.
    #[arg(long, value_name = "NAME")]
    pub server: Option<String>,
    /// Print JSON (name, title, description per tool).
    #[arg(long)]
    pub json: bool,
}

pub async fn run(command: McpCommand) -> Result<(), CliError> {
    match command {
        McpCommand::List(args) => mcp_crud::list(args).await,
        McpCommand::Get(args) => mcp_crud::get(args).await,
        McpCommand::Add(args) => mcp_crud::add(*args).await,
        McpCommand::AddJson(args) => mcp_crud::add_json(args).await,
        McpCommand::Import(args) => crate::mcp_import::import(*args).await,
        McpCommand::Edit(args) => mcp_crud::edit(args).await,
        McpCommand::Remove(args) => mcp_crud::remove(args).await,
        McpCommand::Enable(args) => mcp_crud::set_enabled(args, true).await,
        McpCommand::Disable(args) => mcp_crud::set_enabled(args, false).await,
        McpCommand::Test(args) => mcp_crud::test(args).await,
        McpCommand::Login(args) => mcp_crud::login(args).await,
        McpCommand::Logout(args) => mcp_crud::logout(args).await,
        McpCommand::Secret(args) => mcp_crud::secret(args).await,
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
    let timeout = Duration::from_secs(args.timeout);
    let result = match &args.server {
        Some(name) => {
            let (hub, client, _) = mcp_crud::connect_external(name).await?;
            let result = client
                .call_tool_with_timeout(&args.tool, arguments, timeout)
                .await;
            drop(client);
            hub.shutdown().await;
            result.map_err(|e| CliError::new(e.to_string()))?
        }
        None => {
            let settings = args.conn.settings()?;
            let client = connect_cli(&settings).await?;
            let result = client
                .call_tool_with_timeout(&args.tool, arguments, timeout)
                .await;
            client.close().await;
            result?
        }
    };
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
    if let Some(name) = &args.server {
        let (hub, client, tools) = mcp_crud::connect_external(name).await?;
        drop(client);
        hub.shutdown().await;
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
        return Ok(());
    }
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
