//! `amp-harness`: the AMP agent harness binary (D1).
//!
//! M1-3: `login` / `logout` (OAuth with the keychain credential store, D3). Later milestones add
//! the TUI (default), doctor, init, run, tickets and mcp call.

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
    let result = match cli.command {
        Some(Command::Version) => {
            println!("amp-harness {LONG_VERSION}");
            Ok(())
        }
        Some(Command::Login(args)) => login(args).await,
        Some(Command::Logout(args)) => logout(args).await,
        None => {
            eprintln!(
                "amp-harness {}: try `amp-harness login` or --help",
                env!("CARGO_PKG_VERSION")
            );
            Ok(())
        }
    };
    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("amp-harness: {e}");
            ExitCode::FAILURE
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
