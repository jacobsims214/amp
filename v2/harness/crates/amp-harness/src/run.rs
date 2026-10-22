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
//! - `--print-system-prompt` builds the session and prints the system prompt as the model would
//!   receive it, without calling a model.
//!
//! The faux provider (`api = "faux"` in config.toml) plus `AMP_HARNESS_FAUX_SCRIPT` runs it
//! offline and deterministically.

use std::{
    io::{Read, Write},
    path::PathBuf,
    sync::Arc,
};

use amp_core::{
    agent::{AgentSpec, BuiltAgent, Runtime, RuntimeOptions, SessionKind},
    events::{ChannelEmitter, RunEvent, Totals},
    models::thinking_name,
    project::{ENV_CEILING_DIR, ProjectMarker},
    providers::SystemKeys,
};
use clap::Args;
use rpi_harness::agent_harness::{AgentLane, HarnessRunOutcome};
use serde_json::{Value, json};

use crate::conn::{self, CliError, ConnArgs};

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
        } => eprintln!("[tool error] {name}: {}", preview(text, 400)),
        RunEvent::Retry {
            attempt,
            max_retries,
            delay_ms,
            error,
        } => eprintln!("[retry {attempt}/{max_retries} in {delay_ms} ms] {error}"),
        RunEvent::Assistant {
            error: Some(error), ..
        } => eprintln!("[model error] {error}"),
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
    })
}

pub async fn run(args: RunArgs) -> Result<(), CliError> {
    let prompt = match &args.prompt {
        Some(raw) => Some(read_prompt(raw)?),
        None => None,
    };
    if prompt.as_deref().is_some_and(|p| p.trim().is_empty()) {
        return Err(CliError::usage("the prompt is empty"));
    }

    let cwd = match &args.cwd {
        Some(dir) => dir.clone(),
        None => {
            std::env::current_dir().map_err(|e| CliError::new(format!("current directory: {e}")))?
        }
    };
    let cwd = std::fs::canonicalize(&cwd)
        .map_err(|e| CliError::usage(format!("--cwd {}: {e}", cwd.display())))?;
    let ceiling = std::env::var_os(ENV_CEILING_DIR)
        .filter(|v| !v.is_empty())
        .map(PathBuf::from);
    let project = ProjectMarker::find_within(&cwd, ceiling.as_deref())?;
    let repo_root = project
        .as_ref()
        .and_then(|p| p.path.parent().map(PathBuf::from));

    let settings = args.conn.settings()?;
    let client = Arc::new(conn::connect(&settings).await?);
    let env = |k: &str| std::env::var(k).ok();
    let keys = SystemKeys::from_env();
    let rt = Runtime::load(
        Arc::clone(&client),
        RuntimeOptions {
            repo_root,
            models: None,
            keys: &keys,
            env: &env,
            state_dir: None,
        },
    )
    .map_err(|e| CliError::new(e.to_string()))?;

    let role_name = args
        .role
        .clone()
        .unwrap_or_else(|| rt.roles.manager().id.clone());
    let role_id = rt
        .role(&role_name)
        .map_err(|e| CliError::usage(e.to_string()))?
        .id
        .clone();
    let (emitter, mut rx) = ChannelEmitter::new();
    let mut spec = AgentSpec::new(role_id.clone(), cwd.clone());
    spec.project = project.clone();
    spec.emitter = Some(emitter.into_emitter());
    spec.session = if args.no_session || args.print_system_prompt {
        SessionKind::Memory
    } else {
        SessionKind::Jsonl {
            dir: rt.state_dir.join("sessions").join(&role_id),
        }
    };
    let agent = rt
        .build(spec)
        .await
        .map_err(|e| CliError::new(e.to_string()))?;

    if args.print_system_prompt {
        println!("{}", agent.full_system_prompt(&rt.skills));
        return Ok(());
    }
    let prompt = prompt.expect("clap requires -p unless --print-system-prompt");

    if args.json {
        emit_json(&session_json(&rt, &agent, project.as_ref()));
    }

    let harness = Arc::clone(&agent.harness);
    let mut totals = Totals::default();
    let mut printed_text = false;
    let mut handle = |event: rpi_agent::AgentEvent, totals: &mut Totals| {
        if let Some(e) = RunEvent::from_agent(&event) {
            totals.observe(&e);
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
                let _ = harness.abort().await;
            }
            r = &mut run => break r,
        }
    };
    // Every event was sent before the run returned; drain what is still queued.
    while let Ok(event) = rx.try_recv() {
        handle(event, &mut totals);
    }

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
            } => (
                "failed",
                final_message
                    .as_ref()
                    .map(amp_core::events::assistant_text)
                    .unwrap_or_default(),
                Some(format!("{}: {}", error.code, error.message)),
            ),
            HarnessRunOutcome::Suspended { .. } => (
                "suspended",
                String::new(),
                Some("the provider deferred its response".to_owned()),
            ),
        },
        Err(e) => ("failed", String::new(), Some(format!("{e}"))),
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
        _ => Err(CliError::new(format!(
            "run {outcome}: {}",
            error.unwrap_or_default()
        ))),
    }
}

#[cfg(test)]
mod tests {
    use super::preview;

    #[test]
    fn previews_are_one_line_and_bounded() {
        assert_eq!(preview("a\n  b", 10), "a b");
        assert_eq!(preview("abcdefghijkl", 5), "abcde...");
    }
}
