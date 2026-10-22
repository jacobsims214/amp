//! `amp-harness`: the AMP agent harness binary (D1).
//!
//! - M1-3: `login` / `logout` (OAuth with the keychain credential store, D3).
//! - M1-4: `doctor`, `whoami`, `tickets list|get` (each with `--json`) and the hidden `mcp call`.
//! - M2-3: `config show` (providers and the provider/model each role resolves to).
//!
//! Later milestones add the TUI (default), init and run.

mod config_cmd;
mod conn;
mod doctor;
mod mcp_cmd;
mod tickets;
mod whoami;

use std::{path::PathBuf, process::ExitCode, time::Duration};

use amp_mcp::{
    CredentialBackend, McpError, Overrides, Settings,
    oauth::{self, LoginOptions, LoginPrompt},
};
use clap::{Args, Parser, Subcommand};

/// Harness version plus the exact upstream versions it was built against (D11).
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
);

#[derive(Parser)]
#[command(
    name = "amp-harness",
    about = "AMP agent harness",
    version = env!("CARGO_PKG_VERSION"),
    long_version = LONG_VERSION,
)]
struct Cli {
    #[command(subcommand)]
    command: Option<Command>,
}

#[derive(Subcommand)]
enum Command {
    /// Print the harness version and the rpi/rmcp pins.
    Version,
    /// Log in to an AMP server in the browser (OAuth) and keep the login in the OS keychain.
    Login(LoginArgs),
    /// Forget the stored login for an AMP server.
    Logout(ServerArgs),
    /// Check the connection to AMP: server, protocol, auth, tools, who you are, the project.
    Doctor(JsonArgs),
    /// Show who AMP thinks you are.
    Whoami(JsonArgs),
    /// List and read tickets.
    #[command(subcommand)]
    Tickets(tickets::TicketsCommand),
    /// Providers, models and the model each role runs on.
    #[command(subcommand)]
    Config(config_cmd::ConfigCommand),
    /// Debug: call AMP tools directly.
    #[command(subcommand, hide = true)]
    Mcp(mcp_cmd::McpCommand),
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
    let cli = Cli::parse();
    let result: Result<(), conn::CliError> = match cli.command {
        Some(Command::Version) => {
            println!("amp-harness {LONG_VERSION}");
            Ok(())
        }
        Some(Command::Login(args)) => login(args).await.map_err(Into::into),
        Some(Command::Logout(args)) => logout(args).await.map_err(Into::into),
        Some(Command::Doctor(args)) => doctor::run(args.conn, args.json).await,
        Some(Command::Whoami(args)) => whoami::run(args.conn, args.json).await,
        Some(Command::Tickets(command)) => tickets::run(command).await,
        Some(Command::Config(command)) => config_cmd::run(command).await,
        Some(Command::Mcp(command)) => mcp_cmd::run(command).await,
        None => {
            eprintln!(
                "amp-harness {}: try `amp-harness doctor` or --help",
                env!("CARGO_PKG_VERSION")
            );
            Ok(())
        }
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

async fn login(args: LoginArgs) -> Result<(), McpError> {
    let settings = args.server.settings()?;
    let backend = CredentialBackend::resolve()?;
    let options = LoginOptions::new(settings.url.clone(), backend)
        .with_ca_file(settings.ca_file.clone())
        .with_timeout(Duration::from_secs(args.timeout));
    eprintln!("Logging in to {} ...", settings.url);
    let no_browser = args.no_browser;
    let outcome = oauth::login(options, |prompt: &LoginPrompt| {
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
    use super::Cli;
    use clap::CommandFactory;

    #[test]
    fn cli_definition_is_valid() {
        Cli::command().debug_assert();
    }
}
