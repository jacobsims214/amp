//! `amp-harness`: the AMP agent harness binary (D1).
//!
//! Scaffold (M1-1): only `--version` / `version` exist. Later milestones add the TUI (default),
//! login, logout, doctor, init, run, tickets and mcp call.

use clap::{Parser, Subcommand};

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
}

fn main() {
    let cli = Cli::parse();
    match cli.command {
        Some(Command::Version) => println!("amp-harness {LONG_VERSION}"),
        None => {
            eprintln!(
                "amp-harness {}: scaffold only; try --version",
                env!("CARGO_PKG_VERSION")
            );
        }
    }
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
