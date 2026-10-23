//! `amp-harness run -p "..." [--json]`: one headless turn of an agent (the manager by default)
//! against AMP (D7, M2-4).
//!
//! It connects to AMP like every other command, builds the role's session with
//! `amp_core::agent::Runtime`, sends the prompt and runs until the agent stops.
//!
//! - Text output: the assistant's text streams to stdout; tool calls, tool errors and retries go
//!   to stderr, one line each.
//! - `--json`: one JSON object per line on stdout. First `session` (role, model, tools, project,
//!   MCP server), then the run's events (`turn_start`, `text_delta`, `assistant`, `tool_start`,
//!   `tool_end`, `retry`, `turn_end`), then `result` (outcome, final text, usage totals). Fields
//!   may be added, never renamed or removed.
//! - Exit 0 when the run completed, 1 when it failed or was aborted (Ctrl-C aborts the run).
//! - #2225: a model that keeps repeating one tool call is stopped by the repeat guard
//!   ([`amp_core::repeat`]): the outcome is `stopped`, it exits 1, and its one stderr line says
//!   so.
//! - `--print-system-prompt` builds the session and prints the system prompt as the model would
//!   receive it, without calling a model.
//!
//! The manager gets `amp_run_ticket` (M2-5, see [`crate::session`]); Ctrl-C aborts the manager
//! and cancels its workers.
//!
//! External MCP servers (#2239): `run -p` waits for each configured server's first connection
//! attempt (bounded by its `startup_timeout_secs`) so a headless run sees the same tools every
//! time; a server that is not ready by then is one `amp-harness: MCP server ...` warning line on
//! stderr, not a failure. `--json`'s `session` lists them under `mcp_servers`. Every server is
//! shut down (stdio process groups included) before the command exits.
//!
//! The faux provider (`api = "faux"` in config.toml) plus `AMP_HARNESS_FAUX_SCRIPT` runs it
//! offline and deterministically.

use std::{
    io::{Read, Write},
    path::PathBuf,
    sync::Arc,
};

use amp_core::{
    agent::{BuiltAgent, Runtime},
    events::{RunEvent, Totals},
    models::thinking_name,
    project::ProjectMarker,
    provider_error,
};
use clap::Args;
use rpi_harness::agent_harness::{AgentLane, HarnessRunOutcome};
use serde_json::{Value, json};

use crate::{
    conn::{CliError, ConnArgs},
    session,
};

#[derive(Args, Debug)]
pub struct RunArgs {
    #[command(flatten)]
    conn: ConnArgs,
    /// The prompt. `-` reads it from stdin.
    #[arg(
        short = 'p',
        long = "prompt",
        required_unless_present = "print_system_prompt"
    )]
    prompt: Option<String>,
    /// Print JSON events, one per line.
    #[arg(long)]
    json: bool,
    /// The role to run (default: the manager). Accepts amp:amp-worker-backend, amp-worker-backend
    /// or worker-backend.
    #[arg(long)]
    role: Option<String>,
    /// Working directory for the agent's tools (default: the current directory).
    #[arg(long)]
    cwd: Option<PathBuf>,
    /// Keep the transcript in memory instead of a JSONL file under
    /// ~/.local/state/amp-harness/sessions/<role>/.
    #[arg(long)]
    no_session: bool,
    /// Print the system prompt the model would get, and exit without calling a model.
    #[arg(long)]
    print_system_prompt: bool,
}

fn read_prompt(raw: &str) -> Result<String, CliError> {
    if raw != "-" {
        return Ok(raw.to_owned());
    }
    let mut text = String::new();
    std::io::stdin()
        .read_to_string(&mut text)
        .map_err(|e| CliError::new(format!("read the prompt from stdin: {e}")))?;
    Ok(text)
}

fn emit_json(value: &impl serde::Serialize) {
    if let Ok(line) = serde_json::to_string(value) {
        let mut out = std::io::stdout().lock();
        let _ = writeln!(out, "{line}");
        let _ = out.flush();
    }
}

/// Shortens one-line previews of tool arguments and results for the text output.
fn preview(text: &str, max: usize) -> String {
    let one_line = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if one_line.chars().count() <= max {
        return one_line;
    }
    let cut: String = one_line.chars().take(max).collect();
    format!("{cut}...")
}

/// The text-mode line for a refusal that needs a person in the AMP web UI (#2171).
fn refusal_line(r: &amp_core::approval::Refusal) -> String {
    use amp_core::approval::RefusalKind;
    let what = match (r.kind, r.ticket_id) {
        // #2198: per-ticket approval; its owner approves it (or a project lead on their behalf).
        (RefusalKind::PlanApproval, Some(t)) if r.per_ticket => format!(
            "waiting for #{t}'s owner to approve it in AMP (you, if it's yours; or a project lead \
             on their behalf)"
        ),
        (RefusalKind::PlanApproval, _) => {
            "waiting for your approval: approve the plan in AMP".to_owned()
        }
        (RefusalKind::PersonInUi, _) => "waiting for you in the AMP web UI".to_owned(),
    };
    match &r.link {
        Some(link) => format!("[{what}] {link} (the manager stopped here)"),
        None => format!("[{what}] (the manager stopped here)"),
    }
}

/// Prints one event in text mode. Returns whether it printed assistant text to stdout.
fn print_text_event(event: &RunEvent) -> bool {
    match event {
        RunEvent::TextDelta { delta } => {
            let mut out = std::io::stdout().lock();
            let _ = write!(out, "{delta}");
            let _ = out.flush();
            return true;
        }
        RunEvent::ToolStart {
            name, arguments, ..
        } => eprintln!("[tool] {name} {}", preview(&arguments.to_string(), 160)),
        RunEvent::ToolEnd {
            name,
            is_error: true,
            text,
            ..
        } => {
            eprintln!("[tool error] {name}: {}", preview(text, 400));
            // #2171: a step only a person takes; the manager's turn ended on it.
            if let Some(r) = amp_core::approval::refusal(text) {
                eprintln!("{}", refusal_line(&r));
            }
        }
        RunEvent::Retry {
            attempt,
            max_retries,
            delay_ms,
            error,
        } => eprintln!(
            "[retry {attempt}/{max_retries} in {delay_ms} ms] {}",
            provider_error::summarize("", error)
        ),
        // One line (#2207); `--verbose` adds the provider's whole body (without the account id).
        RunEvent::Assistant {
            error: Some(error),
            provider,
            ..
        } => {
            eprintln!(
                "[model error] {}",
                provider_error::summarize(provider, error)
            );
            if crate::conn::verbose() {
                eprintln!("[model error body] {}", provider_error::redact(error));
            }
        }
        _ => {}
    }
    false
}

fn session_json(rt: &Runtime, agent: &BuiltAgent, project: Option<&ProjectMarker>) -> Value {
    let s = rt.mcp.session();
    json!({
        "type": "session",
        "role": agent.role.id,
        "provider": agent.resolved.provider,
        "model": agent.resolved.model_id,
        "thinking": thinking_name(agent.resolved.thinking),
        "project_id": project.map(|p| p.project_id),
        "cwd": agent.policy.root(),
        "mcp": {
            "url": s.url,
            "protocol": s.protocol_version.to_string(),
            "server": s.server_name,
            "server_version": s.server_version,
            "auth": s.auth,
            "instructions": s.instructions.as_deref().is_some_and(|i| !i.trim().is_empty()),
        },
        "tools": agent.tool_names,
        "skills": agent.role.skills,
        "context_files": agent.context_files,
        "session_file": agent.session_file,
        "faux_scripted": rt.faux_scripted,
        "mcp_servers": rt.mcp_hub.health(),
    })
}

/// One stderr line per MCP server that is not ready for a headless run.
fn mcp_warnings(rt: &Runtime, starting: &[String]) {
    use amp_core::mcp_hub::ServerStatus;
    for h in rt.mcp_hub.health() {
        let label = match &h.instance {
            Some(i) => format!("{} (#{i})", h.name),
            None => h.name.clone(),
        };
        match &h.status {
            ServerStatus::Ready | ServerStatus::Idle | ServerStatus::Disabled => {}
            ServerStatus::Starting if starting.contains(&h.name) => eprintln!(
                "amp-harness: MCP server `{label}` is still starting; this run goes on without its tools"
            ),
            ServerStatus::Starting => {}
            other => eprintln!("amp-harness: MCP server `{label}`: {other}"),
        }
    }
}

pub async fn run(args: RunArgs) -> Result<(), CliError> {
    let prompt = match &args.prompt {
        Some(raw) => Some(read_prompt(raw)?),
        None => None,
    };
    if prompt.as_deref().is_some_and(|p| p.trim().is_empty()) {
        return Err(CliError::usage("the prompt is empty"));
    }

    let session::Session {
        rt,
        agent,
        events: rx,
        project,
        supervisor,
        mcp_starting,
        ..
    } = session::build(session::SessionRequest {
        conn: &args.conn,
        role: args.role.as_deref(),
        cwd: args.cwd.as_deref(),
        memory: args.no_session || args.print_system_prompt,
        // Headless: no AMP, no run (the message says why and what to do).
        allow_offline: false,
        wait_mcp: true,
        // #2241: headless never asks; untrusted repo MCP servers are skipped with a warning.
        ask_trust: false,
    })
    .await?;
    mcp_warnings(&rt, &mcp_starting);
    let result = drive(&args, prompt, &rt, agent, rx, project, supervisor).await;
    // #2239: stdio servers get EOF and their process groups are killed before we exit.
    rt.mcp_hub.shutdown().await;
    result
}

async fn drive(
    args: &RunArgs,
    prompt: Option<String>,
    rt: &Arc<Runtime>,
    agent: BuiltAgent,
    mut rx: tokio::sync::mpsc::UnboundedReceiver<rpi_agent::AgentEvent>,
    project: Option<ProjectMarker>,
    supervisor: Option<Arc<amp_core::supervisor::Supervisor>>,
) -> Result<(), CliError> {
    if args.print_system_prompt {
        println!("{}", agent.full_system_prompt(&rt.catalog().skills));
        return Ok(());
    }
    let prompt = prompt.expect("clap requires -p unless --print-system-prompt");

    if args.json {
        emit_json(&session_json(rt, &agent, project.as_ref()));
    }

    let harness = Arc::clone(&agent.harness);
    let mut totals = Totals::default();
    let mut printed_text = false;
    // Text mode printed the model's error already: the exit message doesn't repeat it (#2207).
    let mut printed_model_error = false;
    // #2225: the repeat guard stopped the run.
    let mut repeat_stop: Option<amp_core::repeat::RepeatStop> = None;
    let mut handle = |event: rpi_agent::AgentEvent, totals: &mut Totals| {
        if let Some(e) = RunEvent::from_agent(&event) {
            totals.observe(&e);
            if let RunEvent::ToolEnd {
                is_error: true,
                text,
                ..
            } = &e
                && let Some(stop) = amp_core::repeat::stopped(text)
            {
                repeat_stop = Some(stop);
            }
            if !args.json && matches!(e, RunEvent::Assistant { error: Some(_), .. }) {
                printed_model_error = true;
            }
            if args.json {
                emit_json(&e);
            } else if print_text_event(&e) {
                printed_text = true;
            }
        }
    };

    let run = harness.prompt_text(&prompt, Vec::new());
    tokio::pin!(run);
    let mut aborted_by_user = false;
    let result = loop {
        tokio::select! {
            biased;
            Some(event) = rx.recv() => handle(event, &mut totals),
            _ = tokio::signal::ctrl_c(), if !aborted_by_user => {
                aborted_by_user = true;
                eprintln!("amp-harness: aborting the run (Ctrl-C)");
                if let Some(sup) = &supervisor {
                    sup.cancel_all();
                }
                let _ = harness.abort().await;
            }
            r = &mut run => break r,
        }
    };
    // Every event was sent before the run returned; drain what is still queued.
    while let Ok(event) = rx.try_recv() {
        handle(event, &mut totals);
    }

    // `error` is one line for people; `error_detail` the provider's whole body (`--json` only).
    let mut error_detail: Option<String> = None;
    let (outcome, text, error) = match result {
        Ok(r) => match r.outcome {
            HarnessRunOutcome::Completed { final_message, .. } => (
                "completed",
                amp_core::events::assistant_text(&final_message),
                None,
            ),
            HarnessRunOutcome::Aborted { final_message, .. } => (
                "aborted",
                amp_core::events::assistant_text(&final_message),
                Some("the run was aborted".to_owned()),
            ),
            HarnessRunOutcome::Failed {
                error,
                final_message,
                ..
            } => {
                let provider = final_message.as_ref().map_or("", |m| m.provider.as_str());
                let summary = provider_error::run_failure(&error.code, &error.message, provider);
                let detail = provider_error::redact(&error.message);
                if detail != summary {
                    error_detail = Some(detail);
                }
                (
                    "failed",
                    final_message
                        .as_ref()
                        .map(amp_core::events::assistant_text)
                        .unwrap_or_default(),
                    Some(summary),
                )
            }
            HarnessRunOutcome::Suspended { .. } => (
                "suspended",
                String::new(),
                Some("the provider deferred its response".to_owned()),
            ),
        },
        Err(e) => ("failed", String::new(), Some(format!("{e}"))),
    };
    let (outcome, error) = match (&repeat_stop, outcome) {
        (Some(stop), "completed") => ("stopped", Some(stop.reason())),
        _ => (outcome, error),
    };
    let text = if text.is_empty() {
        totals.last_text.clone()
    } else {
        text
    };

    if args.json {
        emit_json(&json!({
            "type": "result",
            "outcome": outcome,
            "text": text,
            "error": error,
            "error_detail": error_detail,
            "turns": totals.turns,
            "tool_calls": totals.tool_calls,
            "tool_errors": totals.tool_errors,
            "tools_used": totals.tools_used,
            "usage": totals.usage,
            "model": agent.resolved.model_ref(),
            "session_file": agent.session_file,
        }));
    } else if printed_text {
        println!();
    } else if !text.is_empty() {
        println!("{text}");
    }

    match outcome {
        "completed" => Ok(()),
        // The one stderr line (main prints it as `amp-harness: ...`).
        "stopped" if let Some(stop) = &repeat_stop => Err(CliError::new(
            stop.user_line("set another model for the role in config.toml"),
        )),
        "failed" if printed_model_error => Err(CliError::new("run failed (the model error above)")),
        _ => Err(CliError::new(format!(
            "run {outcome}: {}",
            error.unwrap_or_default()
        ))),
    }
}

#[cfg(test)]
mod tests {
    use super::{preview, refusal_line};

    #[test]
    fn previews_are_one_line_and_bounded() {
        assert_eq!(preview("a\n  b", 10), "a b");
        assert_eq!(preview("abcdefghijkl", 5), "abcde...");
    }

    #[test]
    fn refusals_say_who_acts_and_where() {
        let r = amp_core::approval::refusal(
            "tool error: amp_dispatch_task: ticket 143 was planned after the plan was last \
             approved; ... A person approves it in the AMP web UI (Approve plan): /project/50/plan. \
             Agents can't approve.",
        )
        .unwrap();
        assert_eq!(
            refusal_line(&r),
            "[waiting for your approval: approve the plan in AMP] /project/50/plan (the manager \
             stopped here)"
        );
        // #2198: per-ticket approval names whose approval it waits for.
        let r = amp_core::approval::refusal(
            "tool error: amp_dispatch_task: ticket 2203 isn't approved: nobody has approved it \
             since it was planned or its scope last changed, and this project needs each ticket \
             approved before agents start it. Its owner approves it in the AMP web UI (Approve \
             plan; a project lead may approve it on their behalf): /project/2/plan. Agents can't \
             approve.",
        )
        .unwrap();
        assert_eq!(
            refusal_line(&r),
            "[waiting for #2203's owner to approve it in AMP (you, if it's yours; or a project \
             lead on their behalf)] /project/2/plan (the manager stopped here)"
        );
    }
}
