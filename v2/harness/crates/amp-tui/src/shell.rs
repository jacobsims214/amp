//! The chat shell's controller: one agent harness, the [`ChatView`], the slash commands.
//!
//! It is terminal-free. The terminal loop ([`crate::terminal`]) and the headless driver
//! ([`Shell::drive_until_idle`], used by tests and `amp-harness --snapshot`) both feed it the same
//! three inputs: editor submissions ([`Shell::submit`]), rpi agent events from the session's
//! [`amp_core::events::ChannelEmitter`] ([`Shell::handle_event`]) and finished runs
//! ([`Shell::finish_run`]).
//!
//! Every prompt is `prompt_text` on the same harness, so the conversation carries on between
//! prompts. A run is spawned as its own task so the screen keeps updating while it streams.

use std::{sync::Arc, time::Duration};

use amp_core::events::{RunEvent, Totals};
use amp_core::meta::MetaHandle;
use amp_core::models::thinking_name;
use rpi_agent::AgentEvent;
use rpi_ai::{Model, ThinkingLevel};
use rpi_harness::agent_harness::{AgentHarness, AgentLane, HarnessRunOutcome, RunResult};
use tokio::sync::mpsc::{UnboundedReceiver, UnboundedSender, unbounded_channel};

use crate::view::{ChatView, FooterInfo};

/// A finished `prompt_text` call (`Err` also covers a run task that panicked).
pub type RunDone = Result<RunResult, String>;

/// What the shell shows about the session it drives.
#[derive(Debug, Clone, Default)]
pub struct ShellInfo {
    /// The role id (`amp-manager`).
    pub role: String,
    pub provider: String,
    pub model_id: String,
    pub thinking: ThinkingLevel,
    /// What the config asked for, re-applied (clamped) when `/model` switches models.
    pub requested_thinking: Option<ThinkingLevel>,
    pub context_window: u64,
    pub cwd: String,
    pub git_branch: Option<String>,
    /// Lines of the welcome banner at the top of the transcript.
    pub banner: Vec<String>,
}

impl ShellInfo {
    fn footer(&self) -> FooterInfo {
        FooterInfo {
            model: self.model_id.clone(),
            thinking: thinking_name(self.thinking).to_owned(),
            context_window: self.context_window,
            cwd: self.cwd.clone(),
            git_branch: self.git_branch.clone(),
            session_name: Some(short_role(&self.role).to_owned()),
        }
    }

    fn model_ref(&self) -> String {
        format!("{}/{}", self.provider, self.model_id)
    }
}

/// A model `/model` can switch to: `provider/model` plus the rpi model.
#[derive(Debug, Clone)]
pub struct ModelChoice {
    pub reference: String,
    pub model: Model,
}

/// What the caller does after a submission.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Submit {
    Continue,
    Quit,
}

pub const HELP: &str = "commands: /model [provider/model]  /usage  /help  /quit\n\
keys: Enter send · Shift+Enter new line · Esc abort the run · Ctrl+C abort, clear, or quit";

pub struct Shell {
    view: ChatView,
    harness: Arc<AgentHarness>,
    info: ShellInfo,
    models: Vec<ModelChoice>,
    totals: Totals,
    prompts: u32,
    running: bool,
    done_tx: UnboundedSender<RunDone>,
    /// The AMP metadata poller (#2104), if the caller started one.
    meta: Option<MetaHandle>,
}

impl Shell {
    /// The shell and the receiver its runs finish on (select on it next to the agent events).
    pub fn new(
        harness: Arc<AgentHarness>,
        info: ShellInfo,
        models: Vec<ModelChoice>,
    ) -> (Self, UnboundedReceiver<RunDone>) {
        let (done_tx, done_rx) = unbounded_channel();
        let view = ChatView::new();
        view.set_footer(&info.footer());
        view.add_banner(&info.banner);
        (
            Self {
                view,
                harness,
                info,
                models,
                totals: Totals::default(),
                prompts: 0,
                running: false,
                done_tx,
                meta: None,
            },
            done_rx,
        )
    }

    pub fn view(&self) -> &ChatView {
        &self.view
    }

    pub fn info(&self) -> &ShellInfo {
        &self.info
    }

    pub fn is_running(&self) -> bool {
        self.running
    }

    /// Usage and counts over every run of this session.
    pub fn totals(&self) -> &Totals {
        &self.totals
    }

    /// Attaches the AMP metadata poller: from now on every `amp_*` tool call that ends without
    /// error refreshes what it touched ([`MetaHandle::observe`]).
    /// The header and status line show its snapshot.
    pub fn set_meta(&mut self, meta: MetaHandle) {
        self.view.chrome().set_meta(meta.clone());
        self.meta = Some(meta);
    }

    /// The AMP metadata poller, for the header and status line (M3-3).
    pub fn meta(&self) -> Option<&MetaHandle> {
        self.meta.as_ref()
    }

    pub fn handle_event(&mut self, event: &AgentEvent) {
        if let Some(e) = RunEvent::from_agent(event) {
            self.totals.observe(&e);
        }
        if let Some(meta) = &self.meta {
            meta.observe(event);
        }
        self.view.apply(event);
    }

    /// Handles one editor submission: a slash command or a prompt to the agent.
    pub async fn submit(&mut self, text: &str) -> Submit {
        let trimmed = text.trim();
        if trimmed.is_empty() {
            return Submit::Continue;
        }
        if let Some(rest) = trimmed.strip_prefix('/') {
            let (name, arg) = match rest.split_once(char::is_whitespace) {
                Some((name, arg)) => (name, arg.trim()),
                None => (rest, ""),
            };
            return self.command(name, arg).await;
        }
        if self.running {
            self.view
                .add_notice("still working: wait for the reply, or press Esc to abort it");
            return Submit::Continue;
        }
        self.view.add_user(trimmed);
        self.start_run(trimmed.to_owned());
        Submit::Continue
    }

    fn start_run(&mut self, prompt: String) {
        self.running = true;
        self.prompts += 1;
        self.view.set_working(true);
        let harness = Arc::clone(&self.harness);
        let done = self.done_tx.clone();
        tokio::spawn(async move {
            // The run gets its own task so a panic in it still reports back here.
            let run = tokio::spawn(async move { harness.prompt_text(&prompt, Vec::new()).await });
            let result = match run.await {
                Ok(Ok(r)) => Ok(r),
                Ok(Err(e)) => Err(e.to_string()),
                Err(e) => Err(format!("the run crashed: {e}")),
            };
            let _ = done.send(result);
        });
    }

    /// Asks the running prompt to stop; it then finishes as aborted.
    pub async fn abort(&mut self) {
        if !self.running {
            return;
        }
        self.view.add_notice("aborting…");
        if let Err(e) = self.harness.abort().await {
            self.view.add_error(&format!("abort: {e}"));
        }
    }

    /// Records a finished run. Drain the agent events first: they were all sent before it ended.
    pub fn finish_run(&mut self, result: RunDone) {
        self.running = false;
        self.view.set_working(false);
        match result {
            Ok(r) => match r.outcome {
                HarnessRunOutcome::Completed { .. } => {}
                HarnessRunOutcome::Aborted { .. } => self.view.add_notice("aborted"),
                HarnessRunOutcome::Failed { error, .. } => self
                    .view
                    .add_error(&format!("run failed: {}: {}", error.code, error.message)),
                HarnessRunOutcome::Suspended { .. } => self
                    .view
                    .add_notice("the provider deferred its response (run suspended)"),
            },
            Err(e) => self.view.add_error(&format!("run failed: {e}")),
        }
    }

    async fn command(&mut self, name: &str, arg: &str) -> Submit {
        match name {
            "quit" | "exit" | "q" => return Submit::Quit,
            "help" | "?" => self.view.add_notice(HELP),
            "usage" => self.view.add_notice(&self.usage_text()),
            "model" if arg.is_empty() => self.view.add_notice(&self.model_text()),
            "model" => self.switch_model(arg).await,
            "abort" => self.abort().await,
            "" => self.view.add_notice("empty command (try /help)"),
            other => self
                .view
                .add_notice(&format!("unknown command: /{other} (try /help)")),
        }
        Submit::Continue
    }

    fn usage_text(&self) -> String {
        let t = &self.totals;
        let u = &t.usage;
        format!(
            "usage this session: {} prompt(s), {} turn(s), {} tool call(s) ({} failed)\n\
             tokens: input {} · output {} · cache read {} · cache write {} · cost ${:.4}\n\
             model: {} • thinking {}",
            self.prompts,
            t.turns,
            t.tool_calls,
            t.tool_errors,
            u.input_tokens,
            u.output_tokens,
            u.cache_read_tokens,
            u.cache_write_tokens,
            u.cost_usd,
            self.info.model_ref(),
            thinking_name(self.info.thinking),
        )
    }

    fn model_text(&self) -> String {
        let mut text = format!(
            "model: {} • thinking {} (role {})",
            self.info.model_ref(),
            thinking_name(self.info.thinking),
            short_role(&self.info.role),
        );
        if !self.models.is_empty() {
            text.push_str("\navailable: ");
            text.push_str(
                &self
                    .models
                    .iter()
                    .map(|m| m.reference.as_str())
                    .collect::<Vec<_>>()
                    .join(", "),
            );
            text.push_str("\nswitch with /model provider/model");
        }
        text
    }

    async fn switch_model(&mut self, reference: &str) {
        if self.running {
            self.view
                .add_notice("can't switch models during a run (Esc aborts it)");
            return;
        }
        let Some(choice) = self
            .models
            .iter()
            .find(|m| m.reference == reference)
            .cloned()
        else {
            self.view.add_error(&format!(
                "unknown model `{reference}` (/model lists the available ones)"
            ));
            return;
        };
        let thinking = if choice.model.reasoning {
            choice.model.clamp_thinking_level(
                self.info
                    .requested_thinking
                    .unwrap_or(ThinkingLevel::Medium),
            )
        } else {
            ThinkingLevel::Off
        };
        let model = choice.model.clone();
        if let Err(e) = self.harness.set_model(model).await {
            self.view.add_error(&format!("/model: {e}"));
            return;
        }
        if let Err(e) = self.harness.set_thinking_level(thinking).await {
            self.view.add_error(&format!("/model: thinking: {e}"));
        }
        let (provider, model_id) = reference.split_once('/').unwrap_or(("", reference));
        self.info.provider = provider.to_owned();
        self.info.model_id = model_id.to_owned();
        self.info.thinking = thinking;
        self.info.context_window = choice.model.context_window;
        self.view.set_footer(&self.info.footer());
        self.view.add_notice(&format!(
            "model: {} • thinking {} (this session only)",
            self.info.model_ref(),
            thinking_name(thinking)
        ));
    }

    /// Runs until no prompt is running (or `timeout`), applying events and finished runs.
    /// The headless driver for tests and `--snapshot`; the terminal loop does the same in its
    /// `select!`.
    pub async fn drive_until_idle(
        &mut self,
        events: &mut UnboundedReceiver<AgentEvent>,
        done: &mut UnboundedReceiver<RunDone>,
        timeout: Duration,
    ) -> Result<(), String> {
        let deadline = tokio::time::Instant::now() + timeout;
        while self.running {
            tokio::select! {
                biased;
                Some(event) = events.recv() => self.handle_event(&event),
                Some(result) = done.recv() => {
                    while let Ok(event) = events.try_recv() {
                        self.handle_event(&event);
                    }
                    self.finish_run(result);
                }
                _ = tokio::time::sleep_until(deadline) => {
                    return Err(format!("the run did not finish within {timeout:?}"));
                }
            }
        }
        while let Ok(event) = events.try_recv() {
            self.handle_event(&event);
        }
        Ok(())
    }
}

/// `amp-manager` -> `manager`, `amp:amp-worker-backend` -> `worker-backend`.
pub fn short_role(role: &str) -> &str {
    let role = role.rsplit(':').next().unwrap_or(role);
    role.strip_prefix("amp-").unwrap_or(role)
}

#[cfg(test)]
mod tests {
    use super::short_role;

    #[test]
    fn short_role_drops_the_prefixes() {
        assert_eq!(short_role("amp-manager"), "manager");
        assert_eq!(short_role("amp:amp-worker-backend"), "worker-backend");
        assert_eq!(short_role("reviewer"), "reviewer");
    }
}
