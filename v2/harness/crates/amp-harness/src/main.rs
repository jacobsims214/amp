//! `amp-harness`: the AMP agent harness binary (D1).
//!
//! - M1-3: `login` / `logout` (OAuth with the keychain credential store, D3).
//! - M1-4: `doctor`, `whoami`, `tickets list|get` (each with `--json`) and the hidden `mcp call`.
//! - M2-3: `config show` (providers and the provider/model each role resolves to).
//! - M2-4: `run -p "..." [--json]`: a headless agent run (the manager by default).
//! - M2-7: no subcommand opens the chat TUI with the manager (`amp_tui`).
//! - M3-3: hidden `debug render-chrome` prints the chat's AMP header and status line.
//!
//! - M4-1: `init` and the first run of the chat (server, login, project, provider key).
//! - #2163: `connect [provider]` / `connect --list` / `disconnect <provider>`: model provider keys
//!   and custom providers without shell commands (the chat's `/connect` does the same).
//! - #2164: `server` / `server set <url>` / `server use <name>` and the chat's `/server`.
//! - #2178: connection failures read as one plain sentence with a next step (`--verbose` adds the
//!   cause chain); the chat opens disconnected and retries instead of exiting.
//! - #2232: `--config FILE` / `--config-dir DIR` (and `AMP_HARNESS_CONFIG[_DIR]`) are global and
//!   apply to every setting and command ([`amp_mcp::config_paths`]).
//! - #2234: `agents list|show|new|edit|remove|reset|validate|skills` and
//!   `skills list|show|new|import|edit|remove|reset|validate`, at `--user` or `--repo` scope.
//! - #2235: `models list|set|reset|profile` (per-role models, user or repo file) and
//!   `config check` (every layer's problems, naming file and key).
//! - #2275: `setup` (= `init`) with `--preset FILE|URL` (an org preset), the first run's "your own
//!   OpenAI-compatible endpoint" with models mapped per role, and `connect --role ROLE=MODEL`.

mod agents_cmd;
mod chat;
mod config_cmd;
mod conn;
mod connect;
mod debug;
mod doctor;
mod mcp_cmd;
mod mcp_trust_cmd;
mod models_cmd;
mod run;
mod server_cmd;
mod session;
mod setup;
mod skills_cmd;
mod tickets;
mod whoami;

use std::{ffi::OsString, path::PathBuf, process::ExitCode, time::Duration};

use amp_mcp::{
    CredentialBackend, McpError, Overrides, Settings,
    oauth::{self, LoginOptions, LoginPrompt},
};
use clap::{Args, CommandFactory, Parser, Subcommand};

/// Harness version, the exact upstream versions it was built against, and the commit of the
/// embedded agent/skill assets (D11).
const LONG_VERSION: &str = concat!(
    env!("CARGO_PKG_VERSION"),
    "\nrpi-ai ",
    env!("AMP_PIN_RPI_AI"),
    "\nrpi-agent ",
    env!("AMP_PIN_RPI_AGENT"),
    "\nrpi-tools ",
    env!("AMP_PIN_RPI_TOOLS"),
    "\nrpi-harness ",
    env!("AMP_PIN_RPI_HARNESS"),
    "\nrpi-tui ",
    env!("AMP_PIN_RPI_TUI"),
    "\nrmcp ",
    env!("AMP_PIN_RMCP"),
    "\nassets ",
    env!("AMP_ASSETS_COMMIT"),
);

#[derive(Parser)]
#[command(
    name = "amp-harness",
    about = "AMP agent harness",
    version = env!("CARGO_PKG_VERSION"),
    long_version = LONG_VERSION,
    args_conflicts_with_subcommands = true,
)]
struct Cli {
    #[command(subcommand)]
    command: Option<Command>,
    /// With no subcommand: chat with the manager in the terminal.
    #[command(flatten)]
    chat: chat::ChatArgs,
    /// Show the full cause chain of connection errors (as does a non-empty RUST_LOG).
    #[arg(long, global = true)]
    verbose: bool,
    /// Read and write this config.toml instead of <config dir>/config.toml, for every setting and
    /// every command (env AMP_HARNESS_CONFIG).
    #[arg(long, global = true, value_name = "FILE")]
    config: Option<PathBuf>,
    /// Use this directory instead of ~/.config/amp-harness for the whole user layer: config.toml,
    /// agents/, skills/, AGENTS.md and credentials.json (env AMP_HARNESS_CONFIG_DIR).
    #[arg(long, global = true, value_name = "DIR")]
    config_dir: Option<PathBuf>,
    /// Let every MCP server from this repo's .amp/harness/config.toml start without being trusted
    /// (CI; env AMP_HARNESS_TRUST_REPO_MCP=1). Nothing is recorded.
    #[arg(long, global = true)]
    trust_repo_mcp: bool,
}

#[derive(Subcommand)]
enum Command {
    /// Print the harness version and the rpi/rmcp pins.
    Version,
    /// Log in to an AMP server in the browser (OAuth) and keep the login in the OS keychain.
    Login(LoginArgs),
    /// Forget the stored login for an AMP server.
    Logout(ServerArgs),
    /// Check this machine end to end: AMP connection, CA trust, login, tool contract, project,
    /// provider keys, the models each role pins, and versions. Failing checks say how to fix them.
    Doctor(doctor::DoctorArgs),
    /// Show who AMP thinks you are.
    Whoami(JsonArgs),
    /// List and read tickets.
    #[command(subcommand)]
    Tickets(tickets::TicketsCommand),
    /// Run an agent headless on one prompt (the manager by default).
    Run(run::RunArgs),
    /// Providers, models and the model each role runs on.
    #[command(subcommand)]
    Config(config_cmd::ConfigCommand),
    /// The model each role runs on: `models list`, `models set <role> <provider/model>`,
    /// `models reset <role>`, `models profile <name>` (--user, the default, or --repo).
    #[command(subcommand)]
    Models(models_cmd::ModelsCommand),
    /// Add a model provider: its key (asked hidden, checked, kept in the OS keychain) or a custom
    /// OpenAI-compatible endpoint. `connect --list` shows every provider and where its key comes from.
    Connect(Box<connect::ConnectArgs>),
    /// Remove a provider's key from the OS keychain (and a custom provider from config.toml).
    Disconnect(connect::DisconnectArgs),
    /// Show, save and switch AMP servers (`server`, `server set <url>`, `server use <name>`),
    /// without editing config.toml by hand.
    Server(server_cmd::ServerArgs),
    /// Agents: list, show, create (from a template or a copy), edit in $EDITOR, remove, reset, and
    /// the skills each preloads. `--user` (default) or `--repo` picks the layer a change goes to.
    #[command(subcommand)]
    Agents(agents_cmd::AgentsCommand),
    /// Skills: list, show, create, import (Claude Code / opencode skill dirs), edit, remove, reset.
    #[command(subcommand)]
    Skills(skills_cmd::SkillsCommand),
    /// Set up this machine and folder: AMP server, login, project (.amp.json), model provider
    /// (OpenRouter, Anthropic, OpenAI or your own OpenAI-compatible endpoint, mapped per role).
    /// `--preset FILE|URL` applies an org preset first. Asks for whatever the flags leave open
    /// when run in a terminal; `--yes` never asks. Also `amp-harness setup`.
    #[command(visible_alias = "setup")]
    Init(setup::InitArgs),
    /// Debug: call AMP tools directly.
    #[command(subcommand, hide = true)]
    Mcp(mcp_cmd::McpCommand),
    /// Debug: render checks (`render-chrome`).
    #[command(subcommand, hide = true)]
    Debug(debug::DebugCommand),
}

#[derive(Args)]
struct JsonArgs {
    #[command(flatten)]
    conn: conn::ConnArgs,
    /// Print JSON.
    #[arg(long)]
    json: bool,
}

#[derive(Args)]
struct ServerArgs {
    /// MCP server URL (default: AMP_MCP_URL, then config.toml, then hosted AMP).
    #[arg(long)]
    url: Option<String>,
    /// Extra root certificate (PEM), e.g. the docker-desktop cluster's amp-local-ca.crt.
    #[arg(long)]
    ca_file: Option<PathBuf>,
}

#[derive(Args)]
struct LoginArgs {
    #[command(flatten)]
    server: ServerArgs,
    /// Print the login URL instead of opening a browser.
    #[arg(long)]
    no_browser: bool,
    /// Seconds to wait for the browser to finish.
    #[arg(long, default_value_t = oauth::DEFAULT_LOGIN_TIMEOUT.as_secs())]
    timeout: u64,
}

impl ServerArgs {
    fn settings(&self) -> Result<Settings, McpError> {
        Settings::resolve(&Overrides {
            url: self.url.clone(),
            ca_file: self.ca_file.clone(),
            ..Default::default()
        })
    }
}

#[tokio::main]
async fn main() -> ExitCode {
    let cli = parse_cli(std::env::args_os().collect());
    conn::set_verbose(cli.verbose);
    // #2232: one set of config paths for every reader and writer in this process.
    amp_mcp::config_paths::install(amp_mcp::ConfigPaths::resolve(
        cli.config.clone(),
        cli.config_dir.clone(),
        &|k| std::env::var(k).ok(),
    ));
    // #2241: trust every repo MCP server in this process (never recorded).
    amp_core::mcp_trust::set_trust_all(cli.trust_repo_mcp);
    let result: Result<(), conn::CliError> = match cli.command {
        Some(Command::Version) => {
            println!("amp-harness {LONG_VERSION}");
            Ok(())
        }
        Some(Command::Login(args)) => login(args).await.map_err(Into::into),
        Some(Command::Logout(args)) => logout(args).await.map_err(Into::into),
        Some(Command::Doctor(args)) => doctor::run(args).await,
        Some(Command::Whoami(args)) => whoami::run(args.conn, args.json).await,
        Some(Command::Tickets(command)) => tickets::run(command).await,
        Some(Command::Run(args)) => run::run(args).await,
        Some(Command::Config(command)) => config_cmd::run(command).await,
        Some(Command::Models(command)) => models_cmd::run(command).await,
        Some(Command::Connect(args)) => connect::run(*args).await,
        Some(Command::Disconnect(args)) => connect::disconnect(args).await,
        Some(Command::Init(args)) => setup::init(args).await,
        Some(Command::Server(args)) => server_cmd::run(args).await,
        Some(Command::Mcp(command)) => mcp_cmd::run(command).await,
        Some(Command::Agents(command)) => agents_cmd::run(command).await,
        Some(Command::Skills(command)) => skills_cmd::run(command).await,
        Some(Command::Debug(command)) => debug::run(command).await,
        None => chat::run(cli.chat).await,
    };
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            if !e.message.is_empty() {
                eprintln!("amp-harness: {e}");
            }
            ExitCode::from(e.code)
        }
    }
}

/// Parses the command line. The top-level (chat) flags conflict with subcommands, so clap rejects
/// `amp-harness --url X doctor` ("the subcommand 'doctor' cannot be used with '--url'") while
/// `amp-harness doctor --url X` works. #2208: flags given before a subcommand are moved after it
/// and the line is parsed again, so both orders work for every subcommand that takes the flag; one
/// that doesn't (`version --url`) gets clap's own "unexpected argument" error for that subcommand.
fn parse_cli(args: Vec<OsString>) -> Cli {
    match Cli::try_parse_from(&args) {
        Ok(cli) => cli,
        Err(e) if e.kind() == clap::error::ErrorKind::ArgumentConflict => {
            match flags_after_subcommand(&args) {
                Some(moved) => Cli::try_parse_from(moved).unwrap_or_else(|e| e.exit()),
                None => e.exit(),
            }
        }
        Err(e) => e.exit(),
    }
}

/// `[bin, --url, X, doctor, --json]` -> `[bin, doctor, --json, --url, X]`: the leading top-level
/// long flags (with their values) moved to the end of the subcommand's arguments, before a `--`.
/// `None` when the line doesn't start with known long flags followed by a subcommand name.
fn flags_after_subcommand(args: &[OsString]) -> Option<Vec<OsString>> {
    let cmd = Cli::command();
    let is_subcommand = |t: &str| {
        cmd.get_subcommands()
            .any(|s| s.get_name() == t || s.get_all_aliases().any(|a| a == t))
    };
    let mut i = 1;
    loop {
        let t = args.get(i)?.to_str()?;
        if is_subcommand(t) {
            break;
        }
        let flag = t.strip_prefix("--").filter(|f| !f.is_empty())?;
        let (name, inline_value) = match flag.split_once('=') {
            Some((name, _)) => (name, true),
            None => (flag, false),
        };
        let arg = cmd.get_arguments().find(|a| a.get_long() == Some(name))?;
        i += if !inline_value && arg.get_action().takes_values() {
            2
        } else {
            1
        };
    }
    if i == 1 {
        return None;
    }
    let (flags, rest) = (&args[1..i], &args[i..]);
    let end = rest.iter().position(|a| a == "--").unwrap_or(rest.len());
    let mut moved = vec![args[0].clone()];
    moved.extend_from_slice(&rest[..end]);
    moved.extend_from_slice(flags);
    moved.extend_from_slice(&rest[end..]);
    Some(moved)
}

async fn login(args: LoginArgs) -> Result<(), McpError> {
    let settings = args.server.settings()?;
    let outcome = browser_login(
        &settings,
        args.no_browser,
        Duration::from_secs(args.timeout),
    )
    .await?;

    println!("Logged in to {}", outcome.key);
    println!("  client:  {}", outcome.client_id);
    if !outcome.scopes.is_empty() {
        println!("  scopes:  {}", outcome.scopes.join(" "));
    }
    println!(
        "  refresh: {}",
        if outcome.has_refresh_token {
            "yes"
        } else {
            "no (log in again when the token expires)"
        }
    );
    println!("  stored:  {}", outcome.stored_in);
    if let amp_mcp::Auth::Static { env, .. } = &settings.auth {
        eprintln!("note: {env} is set and takes precedence over this login until it is unset.");
    }
    Ok(())
}

/// The browser OAuth login (D3) for `settings.url`, with the waiting messages on stderr. Used by
/// `login`, the first run and `init`.
pub(crate) async fn browser_login(
    settings: &Settings,
    no_browser: bool,
    timeout: Duration,
) -> Result<oauth::LoginOutcome, McpError> {
    let backend = CredentialBackend::resolve()?;
    let options = LoginOptions::new(settings.url.clone(), backend)
        .with_ca_file(settings.ca_file.clone())
        .with_timeout(timeout);
    eprintln!("Logging in to {} ...", settings.url);
    oauth::login(options, |prompt: &LoginPrompt| {
        eprintln!(
            "Waiting for the browser on {} (Ctrl-C to cancel).",
            prompt.redirect_uri
        );
        let opened = !no_browser && oauth::open_browser(&prompt.authorization_url).is_ok();
        if opened {
            eprintln!("Opened your browser. If nothing happened, open this URL:");
        } else {
            eprintln!("Open this URL in a browser to log in:");
        }
        eprintln!("\n  {}\n", prompt.authorization_url);
    })
    .await
}

async fn logout(args: ServerArgs) -> Result<(), McpError> {
    let settings = args.settings()?;
    let backend = CredentialBackend::resolve()?;
    if oauth::logout(&settings.url, &backend).await? {
        println!(
            "Logged out of {} (removed from {})",
            amp_mcp::credential_key(&settings.url),
            backend.describe()
        );
    } else {
        println!(
            "No stored login for {} in {}",
            amp_mcp::credential_key(&settings.url),
            backend.describe()
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::ffi::OsString;

    use super::{Cli, flags_after_subcommand};
    use clap::{CommandFactory, Parser};

    #[test]
    fn cli_definition_is_valid() {
        Cli::command().debug_assert();
    }

    fn line(s: &str) -> Vec<OsString> {
        s.split(' ').map(OsString::from).collect()
    }

    /// #2208: clap still rejects top-level flags before a subcommand, which is why they are moved.
    #[test]
    fn flags_before_a_subcommand_move_after_it() {
        let rejected = Cli::try_parse_from(line("amp-harness --url http://x/mcp doctor"));
        assert_eq!(
            rejected.err().map(|e| e.kind()),
            Some(clap::error::ErrorKind::ArgumentConflict)
        );
        for (given, moved) in [
            (
                "amp-harness --url http://x/mcp doctor --json",
                "amp-harness doctor --json --url http://x/mcp",
            ),
            (
                "amp-harness --url=http://x/mcp --verbose tickets list",
                "amp-harness tickets list --url=http://x/mcp --verbose",
            ),
            (
                "amp-harness --verbose mcp call amp_whoami -- {}",
                "amp-harness mcp call amp_whoami --verbose -- {}",
            ),
        ] {
            assert_eq!(
                flags_after_subcommand(&line(given)),
                Some(line(moved)),
                "{given}"
            );
        }
        // `--role doctor` is a flag's value, not the subcommand.
        assert_eq!(
            flags_after_subcommand(&line("amp-harness --role doctor --url u doctor")),
            Some(line("amp-harness doctor --role doctor --url u"))
        );
        for left in [
            "amp-harness doctor --url u",
            "amp-harness --url u",
            "amp-harness --nope doctor",
            "amp-harness stray doctor",
        ] {
            assert_eq!(flags_after_subcommand(&line(left)), None, "{left}");
        }
        let cli = super::parse_cli(line("amp-harness --url http://x/mcp --verbose doctor"));
        assert!(cli.verbose && matches!(cli.command, Some(super::Command::Doctor(_))));
    }

    /// #2232: `--config` / `--config-dir` are global: before or after any subcommand, and for the
    /// chat (no subcommand).
    #[test]
    fn config_flags_are_global() {
        use std::path::PathBuf;
        for given in [
            "amp-harness --config /c.toml --config-dir /d doctor --json",
            "amp-harness doctor --json --config /c.toml --config-dir /d",
            "amp-harness --url http://x/mcp --config /c.toml doctor --config-dir /d",
            "amp-harness server set h --yes --config /c.toml --config-dir /d",
            "amp-harness connect --list --config=/c.toml --config-dir=/d",
            "amp-harness --config /c.toml --config-dir /d",
        ] {
            let cli = super::parse_cli(line(given));
            assert_eq!(cli.config, Some(PathBuf::from("/c.toml")), "{given}");
            assert_eq!(cli.config_dir, Some(PathBuf::from("/d")), "{given}");
        }
    }
}
