//! `amp-harness` with no subcommand: the chat TUI with the manager (D10, M2-7).
//!
//! The manager is built exactly as `run -p` builds it ([`crate::session::build`]), with a JSONL
//! session by default, and handed to [`amp_tui::Shell`]. Every prompt goes to the same harness,
//! so the conversation carries on.
//!
//! The chat needs a terminal on stdin and stdout; scripts use `run -p`. The hidden `--snapshot
//! COLSxROWS` mode drives the same shell without a terminal: it submits each `--input` (prompts
//! and slash commands), waits for each run to finish, and prints the final screen as plain text.
//! The integration tests use it, and so can screenshots and M3's render checks (`--ansi` keeps
//! the colours). `--meta` also starts the AMP metadata poller (header, status line and the AMP
//! slash commands, M3-4), and `--input key:<enter|esc|up|down|y|n|o>` presses a key on an open
//! AMP panel (Enter on a question, then `key:y` to confirm the answer). In `--snapshot` nothing
//! opens a browser: `/open` says which URL it would open.
//!
//! The AMP theme (M3-1) is installed before the shell is built: `--theme`, `AMP_HARNESS_THEME`,
//! config.toml `[tui] theme`, else `auto` (the terminal's background via OSC 11; dark in
//! `--snapshot`). Colour depth: `AMP_HARNESS_COLOR`, `[tui] color`, else `COLORTERM`.

use std::{
    io::IsTerminal,
    path::{Path, PathBuf},
    process::Command,
    sync::Arc,
    time::Duration,
};

use amp_core::{
    agent::Runtime,
    meta::{Connector, MetaHandle, MetaOptions, MetaPoller},
    models::thinking_name,
};
use amp_tui::{AmpTheme, ModelChoice, Shell, ShellInfo, Submit, ThemeSettings};
use clap::Args;

use crate::{
    conn::{CliError, ConnArgs},
    session,
};

/// How long `--snapshot` waits for one prompt's run.
const SNAPSHOT_RUN_TIMEOUT: Duration = Duration::from_secs(120);
/// How long `--theme auto` waits for the terminal to report its background colour.
const BACKGROUND_QUERY_TIMEOUT: Duration = Duration::from_millis(300);

#[derive(Args, Debug, Default)]
pub struct ChatArgs {
    #[command(flatten)]
    conn: ConnArgs,
    /// The role to chat with (default: the manager).
    #[arg(long)]
    role: Option<String>,
    /// Working directory for the agent's tools (default: the current directory).
    #[arg(long)]
    cwd: Option<PathBuf>,
    /// Keep the transcript in memory instead of a JSONL file under
    /// ~/.local/state/amp-harness/sessions/<role>/.
    #[arg(long)]
    no_session: bool,
    /// Colour theme: auto (by the terminal's background), dark, light, or a theme .json file
    /// (default: AMP_HARNESS_THEME, then config.toml [tui] theme, then auto).
    #[arg(long, value_name = "auto|dark|light|FILE")]
    theme: Option<String>,
    /// Debug: no terminal; submit each --input, then print the screen at COLSxROWS as text.
    #[arg(long, hide = true, value_name = "COLSxROWS", value_parser = parse_size)]
    snapshot: Option<(usize, usize)>,
    /// Debug (with --snapshot): a prompt or slash command to submit. Repeatable.
    #[arg(long = "input", hide = true, requires = "snapshot")]
    inputs: Vec<String>,
    /// Debug (with --snapshot): print the screen with its ANSI colours.
    #[arg(long, hide = true, requires = "snapshot")]
    ansi: bool,
    /// Debug (with --snapshot): start the AMP metadata poller too (header, status line, AMP
    /// commands), waiting up to 10 s for it to load.
    #[arg(long, hide = true, requires = "snapshot")]
    meta: bool,
}

/// How long `--snapshot --meta` waits for the poller to load (and to settle after each input).
const SNAPSHOT_META_WAIT: Duration = Duration::from_secs(10);

/// Whether the poller still has something on its way: the user, the project and its board, the
/// current ticket (each loaded or failed).
fn meta_pending(meta: &MetaHandle) -> bool {
    let m = meta.snapshot();
    let open = |loaded: bool, failed: bool| !loaded && !failed;
    open(m.user.get().is_some(), m.user.error.is_some())
        || (m.project_id.is_some()
            && (open(m.project.get().is_some(), m.project.error.is_some())
                || open(m.board.get().is_some(), m.board.error.is_some())))
        || m.ticket_id.is_some_and(|id| {
            open(
                m.ticket.get().is_some_and(|t| t.id == id),
                m.ticket.error.is_some(),
            )
        })
}

/// Waits (bounded) for the shell's AMP command calls and the poller's loads, then applies them.
async fn settle_meta(shell: &mut Shell, meta: &MetaHandle) -> Result<(), CliError> {
    shell
        .settle(SNAPSHOT_RUN_TIMEOUT)
        .await
        .map_err(CliError::new)?;
    let deadline = tokio::time::Instant::now() + SNAPSHOT_META_WAIT;
    // Commands sent to the poller (`/ticket`, `/project`) land on its task first.
    tokio::time::sleep(Duration::from_millis(50)).await;
    while meta_pending(meta) && tokio::time::Instant::now() < deadline {
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    shell.poll();
    Ok(())
}

fn parse_size(raw: &str) -> Result<(usize, usize), String> {
    let (w, h) = raw
        .split_once(['x', 'X'])
        .ok_or_else(|| format!("`{raw}` is not COLSxROWS (e.g. 100x30)"))?;
    let w: usize = w
        .trim()
        .parse()
        .map_err(|_| format!("bad width in `{raw}`"))?;
    let h: usize = h
        .trim()
        .parse()
        .map_err(|_| format!("bad height in `{raw}`"))?;
    if w < 10 || h < 6 {
        return Err(format!("`{raw}` is too small (at least 10x6)"));
    }
    Ok((w, h))
}

pub async fn run(args: ChatArgs) -> Result<(), CliError> {
    if args.snapshot.is_none()
        && !(std::io::stdin().is_terminal() && std::io::stdout().is_terminal())
    {
        return Err(CliError::usage(
            "the chat needs a terminal; for scripts use `amp-harness run -p \"...\"` (see --help)",
        ));
    }

    // Before connecting, so a bad theme setting fails fast; before the shell, so every
    // component renders in it.
    let theme = load_theme(&args)?;
    amp_tui::install_theme(&theme);

    let session = session::build(session::SessionRequest {
        conn: &args.conn,
        role: args.role.as_deref(),
        cwd: args.cwd.as_deref(),
        memory: args.no_session,
    })
    .await?;
    let info = shell_info(&session);
    let models = model_choices(&session.rt);
    let (mut shell, mut done) = Shell::new(session.agent.harness.clone(), info, models);
    let project_id = session.project.as_ref().map(|p| p.project_id);
    let mut events = session.events;

    let Some((width, height)) = args.snapshot else {
        shell.set_meta(start_meta(&args.conn, project_id)?);
        if let Some(sup) = session.supervisor.clone() {
            shell
                .view()
                .chrome()
                .set_workers(Arc::new(move || sup.workers()));
        }
        return amp_tui::terminal::run(shell, events, done)
            .await
            .map_err(CliError::new);
    };
    // No browser from a snapshot: /open reports the URL it would open.
    shell.set_opener(Arc::new(|_url: &str| {
        Err("--snapshot does not open a browser".to_owned())
    }));
    let meta = if args.meta {
        let meta = start_meta(&args.conn, project_id)?;
        shell.set_meta(meta.clone());
        if let Some(sup) = session.supervisor.clone() {
            shell
                .view()
                .chrome()
                .set_workers(Arc::new(move || sup.workers()));
        }
        settle_meta(&mut shell, &meta).await?;
        Some(meta)
    } else {
        None
    };
    for input in &args.inputs {
        if let Some(key) = input.strip_prefix("key:") {
            shell.press(key).map_err(CliError::usage)?;
        } else {
            if shell.submit(input).await == Submit::Quit {
                break;
            }
            shell
                .drive_until_idle(&mut events, &mut done, SNAPSHOT_RUN_TIMEOUT)
                .await
                .map_err(CliError::new)?;
        }
        if let Some(meta) = &meta {
            settle_meta(&mut shell, meta).await?;
        }
    }
    let lines = if args.ansi {
        shell.view().render_frame(width, height)
    } else {
        shell.view().render_plain(width, height)
    };
    for line in lines {
        println!("{line}");
    }
    Ok(())
}

/// The theme from `--theme`, the environment and config.toml `[tui]`. `auto` asks the terminal
/// for its background, except in `--snapshot` (no terminal), where it is dark.
fn load_theme(args: &ChatArgs) -> Result<AmpTheme, CliError> {
    let env = |k: &str| std::env::var(k).ok();
    let config_path = args
        .conn
        .config
        .clone()
        .or_else(|| amp_mcp::default_config_path(&env));
    let config = config_path.and_then(|p| std::fs::read_to_string(p).ok());
    let settings = ThemeSettings::resolve(args.theme.as_deref(), &env, config.as_deref())
        .map_err(|e| CliError::usage(e.to_string()))?;
    let interactive = args.snapshot.is_none();
    settings
        .load(&env, || {
            interactive
                .then(|| amp_tui::theme::select::query_background(BACKGROUND_QUERY_TIMEOUT))
                .flatten()
        })
        .map_err(|e| CliError::usage(e.to_string()))
}

/// Starts the AMP metadata poller (#2104) on its own MCP connection, made in the background with
/// the same settings as the session's (so the TUI opens at once, and reconnects are possible).
/// Its calls never go through the model. `--snapshot` does not start it, so its screen has no AMP
/// header or status line and the model's calls are the only AMP calls; `debug render-chrome
/// --live` is the render check with live metadata.
fn start_meta(conn: &ConnArgs, project_id: Option<i64>) -> Result<MetaHandle, CliError> {
    let settings = conn.settings()?;
    let web_url = settings.web_url.clone();
    let connector: Connector = Arc::new(move || {
        let settings = settings.clone();
        Box::pin(async move { crate::conn::connect(&settings).await })
    });
    Ok(MetaPoller::spawn(
        None,
        MetaOptions {
            project_id,
            connector: Some(connector),
            web_url,
            ..MetaOptions::default()
        },
    ))
}

fn shell_info(s: &session::Session) -> ShellInfo {
    let resolved = &s.agent.resolved;
    let mcp = s.rt.mcp.session();
    let project = match &s.project {
        Some(p) => match &p.project_name {
            Some(name) => format!("project #{} {name}", p.project_id),
            None => format!("project #{}", p.project_id),
        },
        None => "no .amp.json here (no project)".to_owned(),
    };
    let session_line = match &s.agent.session_file {
        Some(path) => format!("session {}", path.display()),
        None => "session in memory".to_owned(),
    };
    ShellInfo {
        role: s.agent.role.id.clone(),
        provider: resolved.provider.clone(),
        model_id: resolved.model_id.clone(),
        thinking: resolved.thinking,
        requested_thinking: resolved.requested_thinking,
        context_window: resolved.model.context_window,
        cwd: s.cwd.display().to_string(),
        git_branch: git_branch(&s.cwd),
        banner: vec![
            format!(
                "AMP harness {} · {}",
                env!("CARGO_PKG_VERSION"),
                amp_tui::shell::short_role(&s.agent.role.id)
            ),
            format!("{project} · {} ({})", mcp.url, mcp.protocol_version),
            format!(
                "{}/{} • thinking {} · {session_line}",
                resolved.provider,
                resolved.model_id,
                thinking_name(resolved.thinking)
            ),
            "/help for commands · Esc aborts a reply · Ctrl+C quits".to_owned(),
        ],
    }
}

/// Every model of every provider that has a key (or needs none): what `/model` can switch to.
fn model_choices(rt: &Runtime) -> Vec<ModelChoice> {
    let mut out = Vec::new();
    for (provider_id, provider) in &rt.models.providers {
        if rt.providers.get(provider_id).is_err() {
            continue;
        }
        for (model_id, model) in &provider.models {
            out.push(ModelChoice {
                reference: format!("{provider_id}/{model_id}"),
                model: model.clone(),
            });
        }
    }
    out
}

/// The current branch for the footer, or `None` outside a repository or on a detached head.
fn git_branch(cwd: &Path) -> Option<String> {
    let out = Command::new("git")
        .args(["rev-parse", "--abbrev-ref", "HEAD"])
        .current_dir(cwd)
        .stderr(std::process::Stdio::null())
        .output()
        .ok()?;
    let branch = String::from_utf8(out.stdout).ok()?.trim().to_owned();
    (out.status.success() && !branch.is_empty() && branch != "HEAD").then_some(branch)
}

#[cfg(test)]
mod tests {
    use super::parse_size;

    #[test]
    fn snapshot_sizes() {
        assert_eq!(parse_size("100x30"), Ok((100, 30)));
        assert_eq!(parse_size("80X24"), Ok((80, 24)));
        assert!(parse_size("80").is_err());
        assert!(parse_size("5x5").is_err());
    }
}
