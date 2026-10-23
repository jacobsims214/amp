//! `amp-harness` with no subcommand: the chat TUI with the manager (D10, M2-7).
//!
//! The manager is built exactly as `run -p` builds it ([`crate::session::build`]), with a JSONL
//! session by default, and handed to [`amp_tui::Shell`]. Every prompt goes to the same harness,
//! so the conversation carries on.
//!
//! The chat needs a terminal on stdin and stdout; scripts use `run -p`. The hidden `--snapshot
//! COLSxROWS` mode drives the same shell without a terminal: it submits each `--input` (prompts
//! and slash commands), waits for each run to finish, and prints the final screen as plain text.
//! The integration tests use it, and so can screenshots and M3's render checks.
//!
//! The shell gets the session's worker supervisor: quitting while `amp_run_ticket` runs cancels
//! the workers and waits (bounded) until their abandoned runs are reported ([`Shell::quit`]).

use std::{
    io::IsTerminal,
    path::{Path, PathBuf},
    process::Command,
    time::Duration,
};

use amp_core::{agent::Runtime, models::thinking_name};
use amp_tui::{ModelChoice, Shell, ShellInfo, Submit};
use clap::Args;

use crate::{
    conn::{CliError, ConnArgs},
    session,
};

/// How long `--snapshot` waits for one prompt's run.
const SNAPSHOT_RUN_TIMEOUT: Duration = Duration::from_secs(120);

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
    /// Debug: no terminal; submit each --input, then print the screen at COLSxROWS as text.
    #[arg(long, hide = true, value_name = "COLSxROWS", value_parser = parse_size)]
    snapshot: Option<(usize, usize)>,
    /// Debug (with --snapshot): a prompt or slash command to submit. Repeatable.
    #[arg(long = "input", hide = true, requires = "snapshot")]
    inputs: Vec<String>,
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
    // Quitting mid-dispatch cancels the manager's workers and waits for their run reports (D8).
    shell.set_supervisor(session.supervisor.clone());
    let mut events = session.events;

    let Some((width, height)) = args.snapshot else {
        return amp_tui::terminal::run(shell, events, done)
            .await
            .map_err(CliError::new);
    };
    for input in &args.inputs {
        if shell.submit(input).await == Submit::Quit {
            break;
        }
        shell
            .drive_until_idle(&mut events, &mut done, SNAPSHOT_RUN_TIMEOUT)
            .await
            .map_err(CliError::new)?;
    }
    for line in shell.view().render_plain(width, height) {
        println!("{line}");
    }
    Ok(())
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
