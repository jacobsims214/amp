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
//! AMP panel (Enter on a question, then `key:y` to confirm the answer). `--input key:paste:<text>`
//! pastes (into `/connect`'s hidden key entry when one is open; tests only). In `--snapshot` nothing
//! opens a browser: `/open` says which URL it would open.
//!
//! The AMP theme (M3-1) is installed before the shell is built: `--theme`, `AMP_HARNESS_THEME`,
//! config.toml `[tui] theme`, else `auto` (the terminal's background via OSC 11; dark in
//! `--snapshot`). Colour depth: `AMP_HARNESS_COLOR`, `[tui] color`, else `COLORTERM`. Thinking
//! (#2177): `AMP_HARNESS_SHOW_THINKING`, `[tui] show_thinking` (collapsed|full|hidden), else
//! collapsed; Ctrl+T (`--input key:ctrl+t`) expands the last one. Ctrl+O (`--input key:ctrl+o`)
//! shows every tool call's arguments and result (#2183). Mouse reporting (#2185):
//! `AMP_HARNESS_MOUSE`, `[tui] mouse` (true|false), else on; off gives the terminal's plain
//! click-drag selection back (the wheel then no longer scrolls the transcript; PageUp/PageDown do).
//! Padding (#2251): `AMP_HARNESS_PADDING`, `[tui] padding` (0-8 columns each side), else 2.
//!
//! `/server` (#2164) gets a [`crate::server_cmd::Switcher`]: it lists the saved servers and moves
//! the session (its AMP connection, the model's AMP tools and prompt, the metadata poller) to
//! another one without a restart.
//!
//! **AMP unreachable at start (#2178).** The chat opens anyway, on an offline client: the
//! transcript says why in one sentence with a next step (`amp-harness doctor`, `amp-harness server
//! set`, `--url`), the header's dot is red, and chatting with the model works without AMP tools. A
//! background retry ([`retry_offline`], backoff 2 s doubling to 60 s) connects it; then the model
//! gets its AMP tools and the server's instructions ([`crate::server_cmd::Switcher::connect_offline`])
//! and the transcript says so. `/server <name>` connects (or switches) at once. Headless commands
//! (`run -p`, `whoami`, …) still exit non-zero with the same one-sentence message. In
//! `--snapshot`, `--input wait:amp` waits (bounded) for that connection.
//!
//! The shell gets the session's worker supervisor: quitting while `amp_run_ticket` runs cancels
//! the workers and waits (bounded) until their abandoned runs are reported ([`Shell::quit`]).
//!
//! `/mcp`, `/agents`, `/skills`, `/model roles` and `/config` (#2245, [`crate::chat_config`]) are
//! panels over the session's configuration; what they change is applied through `/reload`'s path.
//! In `--snapshot`, `--input wait:mcp` waits (bounded) until no MCP server is still starting.
//!
//! `/reload` (#2242, [`crate::reload`]) picks up agents, skills, models and MCP servers changed by
//! the CLI in another terminal; a watcher on those files does it on its own (not in
//! `--snapshot`; `AMP_HARNESS_AUTO_RELOAD=0` turns it off). In `--snapshot`, `--input exec:<cmd>`
//! runs `sh -c <cmd>` between inputs (bounded; a failure ends the snapshot), so a test can change
//! the configuration while the chat runs.

use std::{
    io::IsTerminal,
    path::{Path, PathBuf},
    process::Command,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use amp_core::{
    meta::{Connector, MetaHandle, MetaOptions, MetaPoller},
    models::{ModelsConfig, thinking_name},
    providers::ProviderSet,
};
use amp_tui::{AmpTheme, ModelChoice, Notifier, Shell, ShellInfo, Submit, ThemeSettings};
use clap::Args;

use crate::{
    conn::{self, CliError, ConnArgs},
    server_cmd::Switcher,
    session, setup,
};

/// How long `--snapshot` waits for one prompt's run.
const SNAPSHOT_RUN_TIMEOUT: Duration = Duration::from_secs(120);
/// How long `--theme auto` waits for the terminal to start answering its background query (#2155).
const BACKGROUND_QUERY_TIMEOUT: Duration = Duration::from_secs(1);

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
    /// Skip the first-run questions (server, login, project, provider key) and start the chat as is.
    #[arg(long)]
    no_setup: bool,
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

/// The offline retry's first wait and its cap (#2178); the wait doubles in between.
const RETRY_FIRST: Duration = Duration::from_secs(2);
const RETRY_MAX: Duration = Duration::from_secs(60);
/// How long `--input wait:amp` waits for the offline session to connect.
const SNAPSHOT_CONNECT_WAIT: Duration = Duration::from_secs(30);

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
    let show_thinking = load_show_thinking(&args)?;
    let mouse = load_mouse(&args)?;
    let padding = load_padding(&args)?;

    // First run (D12): ask for whatever is missing before connecting for real. In --snapshot only
    // with scripted answers (AMP_HARNESS_PROMPTS=stdin), so render checks never stop on a question.
    let scripted = std::env::var(setup::ui::ENV_PROMPTS).is_ok_and(|v| v.trim() == "stdin");
    let outcome = if !args.no_setup && (args.snapshot.is_none() || scripted) {
        setup::first_run(&args.conn, args.cwd.as_deref()).await?
    } else {
        None
    };

    let session = session::build(session::SessionRequest {
        conn: &args.conn,
        role: args.role.as_deref(),
        cwd: args.cwd.as_deref(),
        memory: args.no_session,
        allow_offline: true,
        // #2239: only eager MCP servers hold the chat up; the rest join as they connect.
        wait_mcp: false,
        // #2241: ask about untrusted repo MCP servers (in the terminal, before the TUI opens).
        ask_trust: true,
    })
    .await?;
    let settings = args.conn.settings()?;
    let marker_warning = marker_warning(&session).await;
    let info = shell_info(&session, marker_warning.as_deref());
    let models = model_choices(
        &session.rt.current_models(),
        &session.rt.current_providers(),
    );
    let (mut shell, mut done) = Shell::new(session.agent.harness.clone(), info, models);
    shell.set_show_thinking(show_thinking);
    shell.set_mouse_capture(mouse);
    shell.set_padding(padding);
    // A suggested first prompt (new project: seed the KB; new user: the AMP guide). Nothing is
    // sent until the person presses Enter.
    if let Some(text) = outcome.and_then(|o| o.prefill) {
        shell.view().editor().set_text(&text);
    }
    // /connect: store a provider key (hidden) and use it without a restart (#2163).
    shell.set_connect(crate::connect::ChatConnect::new(Arc::clone(&session.rt))?);
    // Quitting mid-dispatch cancels the manager's workers and waits for their run reports (D8).
    shell.set_supervisor(session.supervisor.clone());
    let project_id = session.project.as_ref().map(|p| p.project_id);
    // `/server` (#2164): list the saved servers and switch this session to another one.
    let switcher = Arc::new(
        Switcher::new(
            args.conn.clone(),
            settings.clone(),
            Arc::clone(&session.rt),
            Arc::clone(&session.agent.harness),
            Arc::clone(&session.agent.policy),
            session.project.clone(),
            args.snapshot.is_none() || args.meta,
        )
        .await,
    );
    shell.set_server_control(Arc::clone(&switcher) as Arc<dyn amp_tui::ServerControl>);
    // `/reload` (#2242): CLI changes made in another terminal, without a restart.
    shell.set_reload(Arc::new(crate::reload::ChatReload::new(&session)));
    // #2245: `/mcp`, `/agents`, `/skills`, `/model roles`, `/config`; their changes reload.
    shell.set_config(Arc::new(crate::chat_config::ChatConfig::new(
        crate::chat_config::ChatConfigParts {
            rt: Arc::clone(&session.rt),
            cwd: session.cwd.clone(),
            role: session.agent.role.id.clone(),
            settings: Some(settings.clone()),
            workers: session.supervisor.as_ref().map(|s| s.config().clone()),
            theme_flag: args.theme.clone(),
            notes: shell.notifier(),
            browser: args.snapshot.is_none(),
        },
    )));
    // AMP unreachable (#2178): say why and what to do, and keep trying in the background.
    let connected = Arc::new(AtomicBool::new(session.offline.is_none()));
    if let Some(e) = &session.offline {
        shell.view().add_error(&format!(
            "{} The chat works without AMP meanwhile and keeps retrying (the manager gets its AMP \
             tools once connected); `/server <name|address>` switches from here.",
            conn::connect_message(&settings.url, Some(&settings.auth), e)
        ));
        if conn::verbose() {
            shell.view().add_notice(&format!("cause: {e}"));
        }
        tokio::spawn(retry_offline(
            Arc::clone(&switcher),
            shell.notifier(),
            Arc::clone(&connected),
        ));
    }
    // #2241: repo MCP servers left untrusted say so (and how to trust them) in the transcript.
    for u in session.rt.mcp_hub.untrusted() {
        shell.view().add_warning(&format!(
            "MCP server `{}` not started: {}",
            u.name,
            u.reason()
        ));
    }
    // #2245: `mcp 3/4` in the AMP status line.
    {
        let hub = Arc::clone(&session.rt.mcp_hub);
        shell
            .view()
            .chrome()
            .set_mcp(Arc::new(move || hub.summary()));
    }
    let mut events = session.events;
    // #2239: one transcript line per MCP server change ("github: 12 tools", a failure).
    tokio::spawn(forward_mcp_events(session.mcp_events, shell.notifier()));
    let hub = Arc::clone(&session.rt.mcp_hub);

    let Some((width, height)) = args.snapshot else {
        shell.set_meta(start_meta(&args.conn, project_id)?);
        // The same as `/reload`, when the configuration files change (#2242).
        let watcher = crate::reload::auto_reload(&|k| std::env::var(k).ok()).then(|| {
            let notes = shell.notifier();
            tokio::spawn(crate::reload::watch(
                session.rt.paths.clone(),
                crate::reload::WATCH_EVERY,
                move || notes.reload(),
            ))
        });
        if let Some(sup) = session.supervisor.clone() {
            shell
                .view()
                .chrome()
                .set_workers(Arc::new(move || sup.workers()));
        }
        let session_file = session.agent.session_file.clone();
        let result = amp_tui::terminal::run(shell, events, done).await;
        if let Some(watcher) = watcher {
            watcher.abort();
        }
        // Stdio MCP servers get EOF and their process groups are killed before we exit.
        hub.shutdown().await;
        // Back on the main screen: where the conversation was saved (#2184).
        let home = std::env::var_os("HOME").map(PathBuf::from);
        if let Some(line) = exit_line(session_file.as_deref(), home.as_deref()) {
            println!("{line}");
        }
        return result.map_err(CliError::new);
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
        if input == "wait:amp" {
            wait_connected(&mut shell, &connected).await?;
        } else if input == "wait:mcp" {
            wait_mcp(&mut shell, &hub).await?;
        } else if let Some(command) = input.strip_prefix("exec:") {
            exec_input(command).await?;
        } else if let Some(key) = input.strip_prefix("key:") {
            shell.press(key).await.map_err(CliError::usage)?;
        } else {
            if shell.submit(input).await == Submit::Quit {
                break;
            }
            shell
                .drive_until_idle(&mut events, &mut done, SNAPSHOT_RUN_TIMEOUT)
                .await
                .map_err(CliError::new)?;
            // Commands that finish in the background (`/server`) land before the next input.
            shell
                .settle(SNAPSHOT_RUN_TIMEOUT)
                .await
                .map_err(CliError::new)?;
        }
        if let Some(meta) = &meta {
            settle_meta(&mut shell, meta).await?;
        } else {
            // A command's background job (`/connect`) finishes before the next input.
            shell
                .settle(SNAPSHOT_RUN_TIMEOUT)
                .await
                .map_err(CliError::new)?;
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
    hub.shutdown().await;
    Ok(())
}

/// Puts each MCP hub change in the transcript until the hub goes away.
async fn forward_mcp_events(
    mut rx: tokio::sync::broadcast::Receiver<amp_core::mcp_hub::HubEvent>,
    notes: amp_tui::Notifier,
) {
    use tokio::sync::broadcast::error::RecvError;
    loop {
        match rx.recv().await {
            Ok(event) if event.is_error() => notes.error(event.line()),
            Ok(event) => notes.notice(event.line()),
            Err(RecvError::Lagged(_)) => continue,
            Err(RecvError::Closed) => break,
        }
    }
}

/// The offline session's background retry (#2178): connect with backoff until it works, or until
/// `/server` connected the session first. Each failure is quiet unless it changed kind (refused,
/// then host not found…); the header's dot stays red meanwhile.
async fn retry_offline(switcher: Arc<Switcher>, notes: Notifier, connected: Arc<AtomicBool>) {
    let mut wait = RETRY_FIRST;
    let mut last_kind = None;
    loop {
        tokio::time::sleep(wait).await;
        match switcher.connect_offline().await {
            Ok(Some(line)) => {
                notes.notice(line);
                break;
            }
            Ok(None) => break,
            Err(e) => {
                let kind = e.kind();
                if last_kind.is_some_and(|k| k != kind) {
                    notes.error(format!("Still can't reach AMP ({}); retrying.", e.reason()));
                }
                last_kind = Some(kind);
                wait = (wait * 2).min(RETRY_MAX);
            }
        }
    }
    connected.store(true, Ordering::Release);
}

/// How long `--input exec:<cmd>` may take.
const SNAPSHOT_EXEC_TIMEOUT: Duration = Duration::from_secs(60);

/// `--input exec:<cmd>` (tests): runs `sh -c <cmd>` in the chat's environment and waits for it
/// (bounded). A failure (non-zero exit, timeout) ends the snapshot with its output.
async fn exec_input(command: &str) -> Result<(), CliError> {
    let child = tokio::process::Command::new("sh")
        .arg("-c")
        .arg(command)
        .stdin(std::process::Stdio::null())
        .kill_on_drop(true)
        .output();
    let out = tokio::time::timeout(SNAPSHOT_EXEC_TIMEOUT, child)
        .await
        .map_err(|_| {
            CliError::new(format!(
                "exec: `{command}` still running after {SNAPSHOT_EXEC_TIMEOUT:?}"
            ))
        })?
        .map_err(|e| CliError::new(format!("exec: `{command}`: {e}")))?;
    if !out.status.success() {
        return Err(CliError::new(format!(
            "exec: `{command}` failed ({}):\n{}{}",
            out.status,
            String::from_utf8_lossy(&out.stdout),
            String::from_utf8_lossy(&out.stderr)
        )));
    }
    Ok(())
}

/// How long `--input wait:mcp` waits for the MCP servers to settle.
const SNAPSHOT_MCP_WAIT: Duration = Duration::from_secs(30);

/// `--input wait:mcp` (#2245): waits (bounded) until no MCP server is starting, then applies the
/// hub's transcript lines.
async fn wait_mcp(shell: &mut Shell, hub: &amp_core::mcp_hub::McpHub) -> Result<(), CliError> {
    use amp_core::mcp_hub::ServerStatus;
    let deadline = tokio::time::Instant::now() + SNAPSHOT_MCP_WAIT;
    loop {
        let starting: Vec<String> = hub
            .health()
            .into_iter()
            .filter(|h| h.status == ServerStatus::Starting)
            .map(|h| h.name)
            .collect();
        if starting.is_empty() {
            break;
        }
        if tokio::time::Instant::now() >= deadline {
            return Err(CliError::new(format!(
                "wait:mcp: still starting after {SNAPSHOT_MCP_WAIT:?}: {}",
                starting.join(", ")
            )));
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    // The hub's events reach the transcript through a forwarding task.
    tokio::time::sleep(Duration::from_millis(50)).await;
    shell.poll();
    Ok(())
}

/// `--input wait:amp`: waits (bounded) until the offline session connected, then applies its line.
async fn wait_connected(shell: &mut Shell, connected: &AtomicBool) -> Result<(), CliError> {
    let deadline = tokio::time::Instant::now() + SNAPSHOT_CONNECT_WAIT;
    while !connected.load(Ordering::Acquire) {
        if tokio::time::Instant::now() >= deadline {
            return Err(CliError::new(format!(
                "wait:amp: still not connected after {SNAPSHOT_CONNECT_WAIT:?}"
            )));
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    shell.poll();
    Ok(())
}

/// The theme from `--theme`, the environment and config.toml `[tui]`. `auto` asks the terminal
/// for its background, except in `--snapshot` (no terminal), where it is dark.
fn load_theme(args: &ChatArgs) -> Result<AmpTheme, CliError> {
    let env = |k: &str| std::env::var(k).ok();
    let config = config_text(args);
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

/// config.toml's text, if there is one (`--config` / `--config-dir` apply, #2232).
fn config_text(_args: &ChatArgs) -> Option<String> {
    conn::config_paths()
        .user_file
        .and_then(|p| std::fs::read_to_string(p).ok())
}

/// How thinking shows (#2177): `AMP_HARNESS_SHOW_THINKING`, then config.toml `[tui]
/// show_thinking`, else collapsed.
fn load_show_thinking(args: &ChatArgs) -> Result<amp_tui::ThinkingDisplay, CliError> {
    let env = |k: &str| std::env::var(k).ok();
    amp_tui::theme::select::resolve_show_thinking(&env, config_text(args).as_deref())
        .map_err(|e| CliError::usage(e.to_string()))
}

/// Mouse reporting (#2185): `AMP_HARNESS_MOUSE`, then config.toml `[tui] mouse`, else on.
fn load_mouse(args: &ChatArgs) -> Result<bool, CliError> {
    let env = |k: &str| std::env::var(k).ok();
    amp_tui::theme::select::resolve_mouse(&env, config_text(args).as_deref())
        .map_err(|e| CliError::usage(e.to_string()))
}

/// Left and right padding (#2251): `AMP_HARNESS_PADDING`, then config.toml `[tui] padding`,
/// else [`amp_tui::DEFAULT_PADDING`].
fn load_padding(args: &ChatArgs) -> Result<usize, CliError> {
    let env = |k: &str| std::env::var(k).ok();
    amp_tui::theme::select::resolve_padding(&env, config_text(args).as_deref())
        .map_err(|e| CliError::usage(e.to_string()))
}

/// Starts the AMP metadata poller (#2104) on its own MCP connection, made in the background with
/// the same settings as the session's (so the TUI opens at once, and reconnects are possible).
/// Its calls never go through the model. `--snapshot` does not start it, so its screen has no AMP
/// header or status line and the model's calls are the only AMP calls; `debug render-chrome
/// --live` is the render check with live metadata.
fn start_meta(conn: &ConnArgs, project_id: Option<i64>) -> Result<MetaHandle, CliError> {
    Ok(start_meta_with(conn.settings()?, project_id))
}

/// [`start_meta`] for given settings (`/server` starts one for the new server, #2164).
pub(crate) fn start_meta_with(settings: amp_mcp::Settings, project_id: Option<i64>) -> MetaHandle {
    let web_url = settings.web_url.clone();
    let connector: Connector = Arc::new(move || {
        let settings = settings.clone();
        Box::pin(async move { crate::conn::connect(&settings).await })
    });
    MetaPoller::spawn(
        None,
        MetaOptions {
            project_id,
            connector: Some(connector),
            web_url,
            ..MetaOptions::default()
        },
    )
}

/// The line printed after the TUI exits (#2184): `Session saved · ~/.local/state/...jsonl`, with
/// `home` shown as `~`. None without a session file (`--no-session`) or when nothing was written
/// to it, so it never names a file that isn't there.
fn exit_line(session_file: Option<&Path>, home: Option<&Path>) -> Option<String> {
    let path = session_file.filter(|p| p.is_file())?;
    Some(format!("Session saved · {}", tilde(path, home)))
}

/// `path` with a leading `home` written as `~`.
fn tilde(path: &Path, home: Option<&Path>) -> String {
    match home
        .filter(|h| !h.as_os_str().is_empty())
        .and_then(|h| path.strip_prefix(h).ok())
    {
        Some(rest) if rest.as_os_str().is_empty() => "~".to_owned(),
        Some(rest) => format!("~/{}", rest.display()),
        None => path.display().to_string(),
    }
}

/// The banner's session line (#2186): `session 01a1…c795 (/usage for path)`, one row at 80
/// columns, where the whole path took three; `/usage` and the exit line name the file.
fn session_banner(file: Option<&Path>) -> String {
    match file {
        Some(path) => format!("session {} (/usage for path)", session_id(path)),
        None => "session in memory (--no-session)".to_owned(),
    }
}

/// A session file's short id: its uuid (the stem after the `<millis>-` prefix) as the first and
/// last four characters, `01a1…c795`.
fn session_id(path: &Path) -> String {
    let stem = path
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default();
    let id = match stem.split_once('-') {
        Some((millis, rest))
            if !millis.is_empty() && millis.bytes().all(|b| b.is_ascii_digit()) =>
        {
            rest.to_owned()
        }
        _ => stem,
    };
    let chars: Vec<char> = id.chars().collect();
    if chars.len() <= 9 {
        return id;
    }
    let head: String = chars[..4].iter().collect();
    let tail: String = chars[chars.len() - 4..].iter().collect();
    format!("{head}…{tail}")
}

/// How long the banner waits for this server's name of the marker's project (#2206).
const MARKER_CHECK_TIMEOUT: Duration = Duration::from_secs(3);

/// The banner's warning when `.amp.json` was made for another server or names another project
/// than this server's (#2206). Asks AMP for the project's name only when the marker could
/// disagree with it (it has an `amp_api` or a name), within [`MARKER_CHECK_TIMEOUT`].
async fn marker_warning(s: &session::Session) -> Option<String> {
    let marker = s.project.as_ref()?;
    let url = s.rt.mcp.session().url.clone();
    let server_name =
        if s.offline.is_none() && (marker.amp_api.is_some() || marker.project_name.is_some()) {
            let call = amp_core::tools::call_json(
                &s.rt.mcp,
                "amp_get_project",
                serde_json::json!({"project_id": marker.project_id}),
            );
            tokio::time::timeout(MARKER_CHECK_TIMEOUT, call)
                .await
                .ok()
                .and_then(Result::ok)
                .and_then(|v| {
                    v.pointer("/project/name")
                        .or_else(|| v.get("name"))
                        .and_then(|n| n.as_str())
                        .map(str::to_owned)
                })
        } else {
            None
        };
    marker.server_warning(&url, server_name.as_deref()).map(|w| {
        format!("warning: {w}. If that's not this folder's project here, `amp-harness init` picks this server's.")
    })
}

fn shell_info(s: &session::Session, marker_warning: Option<&str>) -> ShellInfo {
    let resolved = &s.agent.resolved;
    let mcp = s.rt.mcp.session();
    let project = match &s.project {
        Some(p) => match &p.project_name {
            Some(name) => format!("project #{} {name}", p.project_id),
            None => format!("project #{}", p.project_id),
        },
        None => "no .amp.json here (no project)".to_owned(),
    };
    let home = std::env::var_os("HOME").map(PathBuf::from);
    let session_file = s
        .agent
        .session_file
        .as_deref()
        .map(|p| tilde(p, home.as_deref()));
    let mut banner = vec![
        format!(
            "AMP harness {} · {}",
            env!("CARGO_PKG_VERSION"),
            amp_tui::shell::short_role(&s.agent.role.id)
        ),
        if s.offline.is_some() {
            format!("{project} · {} (not connected)", mcp.url)
        } else {
            format!("{project} · {} ({})", mcp.url, mcp.protocol_version)
        },
    ];
    banner.extend(marker_warning.map(str::to_owned));
    banner.extend([
        format!(
            "{}/{} • thinking {}",
            resolved.provider,
            resolved.model_id,
            thinking_name(resolved.thinking)
        ),
        session_banner(s.agent.session_file.as_deref()),
        "/help for commands · Esc aborts a reply · Ctrl+C twice quits".to_owned(),
    ]);
    ShellInfo {
        role: s.agent.role.id.clone(),
        provider: resolved.provider.clone(),
        model_id: resolved.model_id.clone(),
        thinking: resolved.thinking,
        requested_thinking: resolved.requested_thinking,
        context_window: resolved.model.context_window,
        cwd: s.cwd.display().to_string(),
        git_branch: git_branch(&s.cwd),
        session_file,
        banner,
    }
}

/// Every model of every provider that has a key (or needs none): what `/model` can switch to.
pub(crate) fn model_choices(models: &ModelsConfig, providers: &ProviderSet) -> Vec<ModelChoice> {
    let mut out = Vec::new();
    for (provider_id, provider) in &models.providers {
        if providers.get(provider_id).is_err() {
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
    use super::{exit_line, parse_size, session_banner, session_id, tilde};
    use std::path::Path;

    #[test]
    fn the_exit_line_names_the_session_file_with_a_tilde() {
        let dir = scratch("exit-line");
        let home = dir.as_path();
        let file = home.join(".local/state/amp-harness/sessions/amp-manager/abc.jsonl");
        std::fs::create_dir_all(file.parent().unwrap()).unwrap();
        std::fs::write(&file, "{}\n").unwrap();
        assert_eq!(
            exit_line(Some(&file), Some(home)).as_deref(),
            Some("Session saved · ~/.local/state/amp-harness/sessions/amp-manager/abc.jsonl")
        );
        // Outside home, or no home: the full path.
        assert_eq!(
            exit_line(Some(&file), Some(Path::new("/elsewhere"))),
            Some(format!("Session saved · {}", file.display()))
        );
        assert_eq!(
            exit_line(Some(&file), None),
            Some(format!("Session saved · {}", file.display()))
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn no_session_file_means_no_exit_line() {
        // --no-session: the transcript was in memory.
        assert_eq!(exit_line(None, Some(Path::new("/home/me"))), None);
        // A session file nothing was written to is not named.
        let dir = scratch("no-file");
        assert_eq!(exit_line(Some(&dir.join("never.jsonl")), None), None);
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// A fresh directory for one test (a few bytes; removed by the test).
    fn scratch(name: &str) -> std::path::PathBuf {
        let dir = std::env::temp_dir().join(format!("amp-harness-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        dir
    }

    #[test]
    fn tilde_only_replaces_a_whole_leading_home() {
        let home = Some(Path::new("/home/me"));
        assert_eq!(tilde(Path::new("/home/me/a/b"), home), "~/a/b");
        assert_eq!(tilde(Path::new("/home/me"), home), "~");
        assert_eq!(tilde(Path::new("/home/meow/a"), home), "/home/meow/a");
        assert_eq!(
            tilde(Path::new("/home/me/a"), Some(Path::new(""))),
            "/home/me/a"
        );
    }

    #[test]
    fn the_banner_names_the_session_by_its_short_id() {
        let path = Path::new(
            "/Users/ada/.local/state/amp-harness/sessions/amp-manager/\
             1792702008122-01a1655c-3f27-76e5-a3f9-a4e1bec7c795.jsonl",
        );
        assert_eq!(session_id(path), "01a1…c795");
        let line = session_banner(Some(path));
        assert_eq!(line, "session 01a1…c795 (/usage for path)");
        // One row at 80 columns with the transcript's padding.
        assert!(line.chars().count() <= 78);
        assert_eq!(session_id(Path::new("/x/short.jsonl")), "short");
        assert_eq!(session_id(Path::new("/x/12-ab.jsonl")), "ab");
        assert_eq!(session_banner(None), "session in memory (--no-session)");
    }

    #[test]
    fn snapshot_sizes() {
        assert_eq!(parse_size("100x30"), Ok((100, 30)));
        assert_eq!(parse_size("80X24"), Ok((80, 24)));
        assert!(parse_size("80").is_err());
        assert!(parse_size("5x5").is_err());
    }
}
