//! `amp-harness debug ...`: hidden developer commands.
//!
//! `debug render-chrome --width N [--theme X] [--color truecolor|256] [--plain] [--live]` prints
//! the chat TUI's AMP header and status line (M3-3, #2106) at one width: the header lines, then
//! the status line. Without `--live` it draws a built-in sample (an in-progress ticket, two
//! questions, plan approval pending, workers); with `--live` it starts the metadata poller on its
//! own AMP connection, waits (bounded by `--wait`) for the user, project, board and ticket to load,
//! and draws what AMP returned. For screenshots, design review and render tests.

use std::{sync::Arc, time::Duration};

use amp_core::{
    meta::{AmpMeta, MetaOptions, MetaPoller},
    project::ProjectMarker,
};
use amp_tui::{
    ChromeData, ColorDepth, ThemeSettings,
    chrome::{plain, render_header, render_status, sample},
    theme::palette,
};
use clap::{Args, Subcommand, ValueEnum};

use crate::conn::{self, CliError, ConnArgs};

#[derive(Subcommand, Debug)]
pub enum DebugCommand {
    /// Print the chat's AMP header and status line at one width.
    RenderChrome(RenderChromeArgs),
}

#[derive(Clone, Copy, Debug, ValueEnum)]
pub enum ColorArg {
    Truecolor,
    #[value(name = "256")]
    Ansi256,
}

#[derive(Args, Debug)]
pub struct RenderChromeArgs {
    #[command(flatten)]
    conn: ConnArgs,
    /// Terminal width in columns.
    #[arg(long, default_value_t = 100, value_parser = clap::value_parser!(u16).range(20..=1000))]
    width: u16,
    /// dark, light, or a theme .json file (default: AMP_HARNESS_THEME, config.toml, then dark).
    #[arg(long, value_name = "dark|light|FILE")]
    theme: Option<String>,
    /// Colour depth (default: AMP_HARNESS_COLOR, config.toml, then COLORTERM).
    #[arg(long)]
    color: Option<ColorArg>,
    /// No colours or links: plain text.
    #[arg(long)]
    plain: bool,
    /// Sample only: leave the current ticket out (the header's second line is hidden).
    #[arg(long, conflicts_with = "live")]
    no_ticket: bool,
    /// Draw live AMP metadata from the server instead of the sample.
    #[arg(long)]
    live: bool,
    /// With --live: the project (default: .amp.json).
    #[arg(long, requires = "live")]
    project: Option<i64>,
    /// With --live: the current ticket.
    #[arg(long, requires = "live")]
    ticket: Option<i64>,
    /// With --live: seconds to wait for the metadata to load.
    #[arg(long, default_value_t = 10, requires = "live")]
    wait: u64,
}

pub async fn run(command: DebugCommand) -> Result<(), CliError> {
    match command {
        DebugCommand::RenderChrome(args) => render_chrome(args).await,
    }
}

async fn render_chrome(args: RenderChromeArgs) -> Result<(), CliError> {
    let env = |k: &str| std::env::var(k).ok();
    let config_path = args
        .conn
        .config
        .clone()
        .or_else(|| amp_mcp::default_config_path(&env));
    let config = config_path.and_then(|p| std::fs::read_to_string(p).ok());
    let mut settings = ThemeSettings::resolve(args.theme.as_deref(), &env, config.as_deref())
        .map_err(|e| CliError::usage(e.to_string()))?;
    if let Some(color) = args.color {
        settings.depth = Some(match color {
            ColorArg::Truecolor => ColorDepth::TrueColor,
            ColorArg::Ansi256 => ColorDepth::Ansi256,
        });
    }
    // No terminal to ask: `auto` is dark.
    let theme = settings
        .load(&env, || None)
        .map_err(|e| CliError::usage(e.to_string()))?;
    amp_tui::install_theme(&theme);

    let (data, complete) = if args.live {
        let (meta, complete) = live_meta(&args).await?;
        (ChromeData::new(meta), complete)
    } else {
        (sample::data(!args.no_ticket), true)
    };

    let width = usize::from(args.width);
    let p = palette();
    let mut lines = render_header(&data, &p, width);
    lines.extend(render_status(&data, &p, width));
    for line in lines {
        if args.plain {
            println!("{}", plain(&line));
        } else {
            println!("{line}");
        }
    }
    if complete {
        Ok(())
    } else {
        Err(CliError::new(format!(
            "AMP metadata did not finish loading within {} s (drawn as far as it got)",
            args.wait
        )))
    }
}

/// Starts the poller on its own connection and waits until every item it was asked for has
/// loaded or failed, or `--wait` runs out. Returns the snapshot and whether it is complete.
async fn live_meta(args: &RenderChromeArgs) -> Result<(AmpMeta, bool), CliError> {
    let project_id = match args.project {
        Some(id) => Some(id),
        None => ProjectMarker::find_from_cwd()?.map(|m| m.project_id),
    };
    let settings = args.conn.settings()?;
    let client = Arc::new(conn::connect(&settings).await?);
    let handle = MetaPoller::spawn(
        Some(client),
        MetaOptions {
            project_id,
            ticket_id: args.ticket,
            web_url: settings.web_url.clone(),
            ..MetaOptions::default()
        },
    );
    let mut rx = handle.subscribe();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(args.wait);
    loop {
        let meta = rx.borrow_and_update().clone();
        if loaded(&meta) {
            return Ok(((*meta).clone(), true));
        }
        match tokio::time::timeout_at(deadline, rx.changed()).await {
            Ok(Ok(())) => {}
            _ => {
                let meta = handle.snapshot();
                return Ok(((*meta).clone(), loaded(&meta)));
            }
        }
    }
}

/// Every item the header and status line need has an answer (a value or an error).
fn loaded(m: &AmpMeta) -> bool {
    let done = |value: bool, error: bool| value || error;
    let user = done(m.user.value.is_some(), m.user.error.is_some());
    let project = m.project_id.is_none()
        || (done(m.project.value.is_some(), m.project.error.is_some())
            && done(m.board.value.is_some(), m.board.error.is_some()));
    let plan = !m.plan_required() || done(m.plan.value.is_some(), m.plan.error.is_some());
    let ticket = m.ticket_id.is_none() || done(m.ticket.value.is_some(), m.ticket.error.is_some());
    user && project && plan && ticket
}
